package natsbus

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

type Client struct {
	mu   sync.RWMutex
	nc   *nats.Conn
	js   nats.JetStreamContext
	url  string
	name string

	connectedAt      int64
	lastDisconnectAt int64
	lastReconnectAt  int64
	disconnects      int64
	reconnects       int64
	lastError        string
	lastConnectedURL string
	closed           bool
}

type Status struct {
	Name             string `json:"name"`
	ServerURL        string `json:"serverUrl"`
	Connected        bool   `json:"connected"`
	Status           string `json:"status"`
	ConnectedAt      int64  `json:"connectedAt,omitempty"`
	LastDisconnectAt int64  `json:"lastDisconnectAt,omitempty"`
	LastReconnectAt  int64  `json:"lastReconnectAt,omitempty"`
	Disconnects      int64  `json:"disconnects"`
	Reconnects       int64  `json:"reconnects"`
	LastError        string `json:"lastError,omitempty"`
	InMsgs           uint64 `json:"inMsgs"`
	OutMsgs          uint64 `json:"outMsgs"`
	InBytes          uint64 `json:"inBytes"`
	OutBytes         uint64 `json:"outBytes"`
}

func Connect(ctx context.Context, servers []string, name string) (*Client, error) {
	if len(servers) == 0 {
		return nil, fmt.Errorf("empty nats servers")
	}

	url := strings.Join(servers, ",")
	c := &Client{url: url, name: name}

	nc, err := nats.Connect(url,
		nats.Name(name),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.disconnects++
			c.lastDisconnectAt = time.Now().UnixMilli()
			if err != nil {
				c.lastError = err.Error()
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.reconnects++
			c.lastReconnectAt = time.Now().UnixMilli()
			c.lastConnectedURL = nc.ConnectedUrl()
		}),
		nats.ClosedHandler(func(_ *nats.Conn) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.closed = true
		}),
	)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.nc = nc
	c.connectedAt = time.Now().UnixMilli()
	c.lastConnectedURL = nc.ConnectedUrl()
	c.mu.Unlock()

	js, err := nc.JetStream()
	if err != nil {
		slog.Warn("jetstream not available, transfer disabled", slog.String("error", err.Error()))
	} else {
		c.mu.Lock()
		c.js = js
		c.mu.Unlock()
	}

	go func() {
		<-ctx.Done()
		c.Close()
	}()

	return c, nil
}

func (c *Client) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()

	st := Status{
		Name:             c.name,
		ServerURL:        c.url,
		ConnectedAt:      c.connectedAt,
		LastDisconnectAt: c.lastDisconnectAt,
		LastReconnectAt:  c.lastReconnectAt,
		Disconnects:      c.disconnects,
		Reconnects:       c.reconnects,
		LastError:        c.lastError,
	}

	if c.nc == nil || c.closed {
		st.Connected = false
		st.Status = "closed"
		return st
	}

	stats := c.nc.Stats()
	st.InMsgs = stats.InMsgs
	st.OutMsgs = stats.OutMsgs
	st.InBytes = stats.InBytes
	st.OutBytes = stats.OutBytes

	st.ServerURL = c.lastConnectedURL
	status := c.nc.Status().String()
	st.Status = status
	st.Connected = c.nc.IsConnected()
	return st
}

func (c *Client) PublishJSON(subject string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.nc == nil {
		return fmt.Errorf("nats client is closed")
	}
	return c.nc.Publish(subject, b)
}

func (c *Client) Subscribe(subject string, handler func(subject string, data []byte)) (*nats.Subscription, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.nc == nil {
		return nil, fmt.Errorf("nats client is closed")
	}

	return c.nc.Subscribe(subject, func(msg *nats.Msg) {
		handler(msg.Subject, msg.Data)
	})
}

// DurableSubscription is the result of SubscribeDurable. When JetStream is
// unavailable the fallback core subscription is wrapped here so callers
// can treat both modes uniformly; Unsubscribes only applies to the core
// sub (JS consumers are managed server-side by their durable name).
type DurableSubscription struct {
	core *nats.Subscription
	js   bool
}

// Durable reports whether the subscription is JetStream-backed.
func (d *DurableSubscription) Durable() bool { return d != nil && d.js }

// Unsubscribe removes a fallback core subscription; a no-op for JS mode.
func (d *DurableSubscription) Unsubscribe() error {
	if d == nil || d.core == nil {
		return nil
	}
	return d.core.Unsubscribe()
}

// SubscribeDurable attaches handler to subject with at-least-once
// semantics: the handler's returned error decides Ack vs Nak, and a
// durable consumer (AckExplicit, AckWait 60s, MaxDeliver 5) redelivers
// everything the server still holds — including messages published while
// this client was disconnected.
//
// A stream covering "clawsynapse.>" is created on first use so core
// publishes land in JetStream. If JetStream is unavailable (server
// without JS, missing permissions, stream creation failure) the call
// degrades to a plain core subscription with a warning — fire-and-forget
// semantics, exactly the pre-T2.5 behavior.
func (c *Client) SubscribeDurable(subject, durable string, handler func(subject string, data []byte) error) (*DurableSubscription, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.nc == nil {
		return nil, fmt.Errorf("nats client is closed")
	}

	if c.js != nil {
		if err := c.subscribeJetStream(subject, durable, handler); err == nil {
			return &DurableSubscription{js: true}, nil
		} else {
			slog.Warn("jetstream durable subscribe failed; falling back to core subscription",
				slog.String("subject", subject),
				slog.String("durable", durable),
				slog.String("error", err.Error()),
			)
		}
	}

	sub, err := c.nc.Subscribe(subject, func(msg *nats.Msg) {
		_ = handler(msg.Subject, msg.Data) // core NATS has no acks
	})
	if err != nil {
		return nil, err
	}
	return &DurableSubscription{core: sub}, nil
}

// subscribeJetStream creates the CLAWSYNAPSE stream (if missing) and a
// durable push consumer, then wires handler with Ack/Nak. Requires the
// caller to hold c.mu (read).
func (c *Client) subscribeJetStream(subject, durable string, handler func(subject string, data []byte) error) error {
	const streamName = "CLAWSYNAPSE"
	if _, err := c.js.StreamInfo(streamName); err != nil {
		if _, err2 := c.js.AddStream(&nats.StreamConfig{
			Name:      streamName,
			Subjects:  []string{"clawsynapse.>"},
			Retention: nats.LimitsPolicy,
			Storage:   nats.FileStorage,
			MaxAge:    24 * time.Hour,
		}); err2 != nil {
			return fmt.Errorf("create stream %s: %w", streamName, err2)
		}
	}

	if _, err := c.js.Subscribe(subject, func(msg *nats.Msg) {
		if err := handler(msg.Subject, msg.Data); err != nil {
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
	},
		nats.Durable(durable),
		nats.ManualAck(),
		nats.AckWait(60*time.Second),
		nats.MaxDeliver(5),
		nats.DeliverAll(),
	); err != nil {
		return fmt.Errorf("durable consumer %s: %w", durable, err)
	}
	return nil
}

func (c *Client) FlushTimeout(timeout time.Duration) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.nc == nil {
		return fmt.Errorf("nats client is closed")
	}
	return c.nc.FlushTimeout(timeout)
}

func (c *Client) JetStream() nats.JetStreamContext {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.js
}

func (c *Client) HasJetStream() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.js != nil && c.nc != nil && c.nc.IsConnected()
}

func (c *Client) Conn() *nats.Conn {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.nc
}

func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.nc != nil {
		c.nc.Close()
		c.nc = nil
	}
	c.closed = true
}
