package messaging

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"clawsynapse/internal/discovery"
	"clawsynapse/internal/identity"
	"clawsynapse/internal/logging"
	"clawsynapse/internal/natsbus"
	"clawsynapse/internal/protocol"
	"clawsynapse/internal/replay"
	"clawsynapse/pkg/types"
)

const contentPreviewLimit = 160

type PublishRequest struct {
	TargetNode string
	Type       string
	AgentID    string
	Message    string
	SessionKey string
	Metadata   map[string]any
}

type PublishResult struct {
	MessageID  string
	SessionKey string
}

type TransferHandler func(env protocol.MessageEnvelope)

type Service struct {
	mu                  sync.Mutex
	log                 *slog.Logger
	peers               *discovery.Registry
	bus                 *natsbus.Client
	nodeID              string
	identity            *identity.Identity
	trustMode           string
	deliverablePrefixes []string
	inbox               []protocol.MessageEnvelope
	handler             MessageHandler
	transferHandler     TransferHandler
	// outbox (T2.7) makes agent replies reliable; nil keeps the legacy
	// best-effort reply behavior.
	outbox *Outbox
	// dispatcher (T2.1) serializes deliveries per session key; nil keeps
	// the legacy fire-and-forget goroutine per delivery.
	dispatcher *sessionDispatcher
	// replay (T2.2) drops duplicate inbox envelopes by id; nil keeps the
	// legacy at-most-once-by-luck behavior.
	replay *replay.ReplayGuard

	// draining (T2.6): once set, new inbox deliveries are Nak'd (the
	// durable inbox redelivers them after restart) and dispatch is
	// rejected. drainWG counts queued+running handler executions and
	// pending remembers their envelopes so a drain timeout can answer
	// them with an interruption error.
	draining   atomic.Bool
	drainWG    sync.WaitGroup
	pendingMu  sync.Mutex
	pending    map[string]protocol.MessageEnvelope
	pendingSeq atomic.Int64 // key for envelopes without an id
}

func NewService(log *slog.Logger, peers *discovery.Registry, bus *natsbus.Client, nodeID string, id *identity.Identity, trustMode string, deliverablePrefixes []string) *Service {
	if len(deliverablePrefixes) == 0 {
		deliverablePrefixes = []string{"chat", "task"}
	}
	return &Service{log: log, peers: peers, bus: bus, nodeID: nodeID, identity: id, trustMode: trustMode, deliverablePrefixes: deliverablePrefixes, inbox: []protocol.MessageEnvelope{}, dispatcher: newSessionDispatcher(log), pending: map[string]protocol.MessageEnvelope{}}
}

func (s *Service) SetMessageHandler(handler MessageHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handler = handler
}

func (s *Service) SetTransferHandler(h TransferHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transferHandler = h
}

// EnableReplayGuard wires the shared replay guard (T2.2): envelopes whose
// id was already accepted within the TTL are dropped with a warning before
// delivery. Passing nil keeps the legacy behavior (tests).
func (s *Service) EnableReplayGuard(g *replay.ReplayGuard) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replay == nil {
		s.replay = g
	}
}

// dedupInbox reports whether env is a duplicate delivery. The check runs
// after trust verification so an unauthenticated sender cannot pre-register
// ids of legitimate future messages.
func (s *Service) dedupInbox(env protocol.MessageEnvelope) bool {
	s.mu.Lock()
	g := s.replay
	s.mu.Unlock()
	if g == nil || env.ID == "" {
		return false
	}
	if !g.CheckAndRemember("msg:"+env.ID, 24*time.Hour) {
		s.log.Warn("duplicate message dropped",
			logging.Event("message.duplicate"),
			logging.From(env.From),
			logging.MessageID(env.ID),
			logging.MessageType(env.Type),
		)
		return true
	}
	return false
}

func (s *Service) Start() error {
	inboxSubject := "clawsynapse.msg." + s.nodeID + ".inbox"
	if s.bus == nil {
		return errors.New("nats client is required")
	}
	// T2.5: durable consumer — messages published while this node is
	// disconnected (or whose delivery errored) are redelivered on
	// reconnect. JetStream unavailable → falls back to core subscription
	// inside the bus (legacy fire-and-forget) with a warning.
	sub, err := s.bus.SubscribeDurable(inboxSubject, "inbox-"+s.nodeID, s.handleInbox)
	if err != nil {
		return err
	}
	s.log.Info("subscribed to inbox",
		logging.Event("message.subscribe"),
		logging.Subject(inboxSubject),
		slog.Bool("durable", sub.Durable()),
	)
	return nil
}

func (s *Service) Publish(req PublishRequest) (PublishResult, error) {
	if req.TargetNode == "" {
		return PublishResult{}, errors.New("targetNode is required")
	}
	if s.bus == nil {
		return PublishResult{}, errors.New("nats client is required")
	}
	peer, ok := s.peers.Get(req.TargetNode)
	if !ok {
		return PublishResult{}, errors.New("target peer not found")
	}
	if s.trustMode != "open" {
		if peer.TrustStatus != types.TrustTrusted {
			return PublishResult{}, protocol.NewError("control.unauthorized", "peer is not trusted")
		}
		if peer.AuthStatus != types.AuthAuthenticated {
			return PublishResult{}, errors.New("peer is not authenticated")
		}
	}

	sessionKey := req.SessionKey
	if strings.TrimSpace(sessionKey) == "" {
		sessionKey = newSessionKey()
	}

	msgType := req.Type
	if msgType == "" {
		msgType = "chat.message"
	}

	env := protocol.MessageEnvelope{
		ID:              randID(),
		Type:            msgType,
		AgentID:         strings.TrimSpace(req.AgentID),
		From:            s.nodeID,
		To:              req.TargetNode,
		Content:         req.Message,
		SessionKey:      sessionKey,
		Metadata:        req.Metadata,
		Ts:              time.Now().UnixMilli(),
		ProtocolVersion: "v1",
	}
	env.Sig = identity.Sign(s.identity.PrivateKey, []byte(s.signatureInput(env)))

	subject := "clawsynapse.msg." + req.TargetNode + ".inbox"
	if err := s.bus.PublishJSON(subject, env); err != nil {
		return PublishResult{}, err
	}
	s.log.Info("message published",
		logging.Event("message.sent"),
		logging.To(req.TargetNode),
		logging.MessageID(env.ID),
		logging.MessageType(env.Type),
		logging.SessionKey(sessionKey),
		logging.ContentLength(req.Message),
		logging.ContentPreview(req.Message, contentPreviewLimit),
	)
	return PublishResult{MessageID: env.ID, SessionKey: sessionKey}, nil
}

func (s *Service) RecentMessages(limit int) []protocol.MessageEnvelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > len(s.inbox) {
		limit = len(s.inbox)
	}
	start := len(s.inbox) - limit
	out := make([]protocol.MessageEnvelope, limit)
	copy(out, s.inbox[start:])
	return out
}

// handleInbox processes one raw inbox payload. Its error drives the
// durable inbox (T2.5): nil → Ack, non-nil → Nak (redelivered up to
// MaxDeliver). Only transient losses (session queue full) return an
// error; permanent failures (decode, untrusted sender, duplicate) ack —
// redelivery could not fix them.
// Drain stops accepting new inbox deliveries and waits for the in-flight
// handler executions until ctx expires (T2.6). On timeout the still
// pending envelopes get an interruption error reply — the platform learns
// the task did not finish — and the caller is expected to cancel the
// adapter's root context right after so gateway runs actually stop.
func (s *Service) Drain(ctx context.Context) {
	s.draining.Store(true)
	s.log.Info("messaging drain started")

	done := make(chan struct{})
	go func() {
		s.drainWG.Wait()
		close(done)
	}()

	select {
	case <-done:
		s.log.Info("messaging drain complete")
		return
	case <-ctx.Done():
	}

	s.pendingMu.Lock()
	leftovers := make([]protocol.MessageEnvelope, 0, len(s.pending))
	for _, env := range s.pending {
		leftovers = append(leftovers, env)
	}
	s.pending = map[string]protocol.MessageEnvelope{}
	s.pendingMu.Unlock()

	for _, env := range leftovers {
		s.replyToSender(env, "node shutting down, task interrupted", true)
	}
	s.log.Warn("messaging drain timed out; in-flight deliveries interrupted",
		slog.Int("count", len(leftovers)),
	)
}

func (s *Service) handleInbox(subject string, data []byte) error {
	if s.draining.Load() {
		// T2.6: shut-down in progress. Nak so the durable inbox
		// redelivers this envelope after the node restarts.
		return fmt.Errorf("node draining: message %s deferred", subject)
	}
	var env protocol.MessageEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		s.log.Warn("decode inbox message failed", logging.Subject(subject), logging.Error(err))
		return nil
	}

	if env.To != "" && env.To != s.nodeID {
		return nil
	}

	if s.trustMode == "open" {
		s.log.Info("message received",
			logging.Event("message.received"),
			logging.From(env.From),
			logging.MessageID(env.ID),
			logging.MessageType(env.Type),
			logging.SessionKey(env.SessionKey),
			logging.ContentLength(env.Content),
			logging.ContentPreview(env.Content, contentPreviewLimit),
		)
		return s.acceptAndDispatch(env)
	}

	peer, ok := s.peers.Get(env.From)
	if !ok {
		s.log.Warn("message sender not found", logging.From(env.From))
		return nil
	}
	if peer.TrustStatus != types.TrustTrusted {
		s.log.Warn("reject message from untrusted peer", logging.From(env.From), logging.TrustStatus(peer.TrustStatus))
		return nil
	}

	pub, err := s.peerPublicKey(env.From)
	if err != nil {
		s.log.Warn("sender public key unavailable", logging.From(env.From), logging.Error(err))
		return nil
	}
	if !identity.Verify(pub, []byte(s.signatureInput(env)), env.Sig) {
		s.log.Warn("invalid message signature", logging.From(env.From), logging.MessageID(env.ID))
		return nil
	}

	s.log.Info("message received",
		logging.Event("message.received"),
		logging.From(env.From),
		logging.MessageID(env.ID),
		logging.MessageType(env.Type),
		logging.SessionKey(env.SessionKey),
		logging.ContentLength(env.Content),
		logging.ContentPreview(env.Content, contentPreviewLimit),
	)
	return s.acceptAndDispatch(env)
}

// acceptAndDispatch dedups, records and enqueues the envelope. A failed
// enqueue (queue full) forgets the dedup key before erroring so the
// Nak-driven redelivery is not mistaken for a duplicate (T2.5).
func (s *Service) acceptAndDispatch(env protocol.MessageEnvelope) error {	if s.dedupInbox(env) {
		return nil
	}
	s.acceptInbox(env)
	if s.maybeDeliver(env) {
		return nil
	}
	s.mu.Lock()
	g := s.replay
	s.mu.Unlock()
	if g != nil {
		g.Forget("msg:" + env.ID)
	}
	return fmt.Errorf("inbox delivery for %s dropped: session queue full", env.ID)
}

func (s *Service) acceptInbox(env protocol.MessageEnvelope) {
	if s.trustMode == "open" {
		if _, ok := s.peers.Get(env.From); !ok && env.From != "" {
			s.peers.Upsert(types.Peer{NodeID: env.From, AuthStatus: types.AuthAuthenticated, TrustStatus: types.TrustTrusted})
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.inbox = append(s.inbox, env)
	if len(s.inbox) > 1000 {
		s.inbox = s.inbox[len(s.inbox)-1000:]
	}
}

// maybeDeliver routes env to the agent handler (or the transfer handler).
// It reports whether the envelope was consumed or intentionally dropped
// (true = safe to ack) versus lost to backpressure (false = the durable
// inbox should Nak and redeliver, T2.5).
func (s *Service) maybeDeliver(env protocol.MessageEnvelope) bool {
	if env.Type == "transfer.available" {
		s.mu.Lock()
		th := s.transferHandler
		s.mu.Unlock()
		if th != nil {
			go th(env)
		}
		return true
	}

	if !isDeliverableType(env.Type, s.deliverablePrefixes) {
		return true // not for the agent: intentional drop
	}
	handler := s.messageHandler()
	if handler == nil {
		return true // no consumer configured: redelivery cannot help
	}

	return s.dispatchSession(env, func() {
		result, err := handler.HandleMessage(IncomingMessage{
			MessageID:  env.ID,
			Type:       env.Type,
			AgentID:    env.AgentID,
			From:       env.From,
			To:         env.To,
			Message:    env.Content,
			SessionKey: env.SessionKey,
			Metadata:   cloneMetadata(env.Metadata),
		})
		if err != nil {
			s.log.Warn("deliver message to agent failed",
				logging.Event("message.deliver.failed"),
				logging.From(env.From),
				logging.MessageID(env.ID),
				logging.SessionKey(env.SessionKey),
				logging.Error(err),
			)
			s.replyToSender(env, err.Error(), true)
			return
		}
		s.log.Info("message delivered to agent",
			logging.Event("message.deliver.ok"),
			logging.From(env.From),
			logging.MessageID(env.ID),
			logging.SessionKey(env.SessionKey),
		)
		if result.Reply != "" {
			s.replyToSender(env, result.Reply, false)
		}
	})
}

// dispatchSession routes a delivery through the per-sessionKey serial
// queue (T2.1): same-key deliveries run in arrival order on one worker,
// different keys stay concurrent. Without a dispatcher (legacy) it is a
// plain fire-and-forget goroutine. Returns whether the delivery was
// enqueued (false = queue full and the drop deadline passed, or the
// service is draining — T2.6).
//
// Every accepted delivery is registered in pending + drainWG until its
// handler finishes, so Drain knows exactly what is in flight.
func (s *Service) dispatchSession(env protocol.MessageEnvelope, fn func()) bool {
	if s.draining.Load() {
		return false // T2.6: no new work while draining
	}
	key := env.ID
	if key == "" {
		key = fmt.Sprintf("anon-%d", s.pendingSeq.Add(1))
	}
	s.pendingMu.Lock()
	s.pending[key] = env
	s.pendingMu.Unlock()
	s.drainWG.Add(1)

	var d *sessionDispatcher
	s.mu.Lock()
	d = s.dispatcher
	s.mu.Unlock()

	clearPending := func() {
		s.pendingMu.Lock()
		delete(s.pending, key)
		s.pendingMu.Unlock()
		s.drainWG.Done()
	}

	var enqueued bool
	wrapped := func() {
		defer clearPending()
		fn()
	}
	if d == nil {
		go wrapped()
		enqueued = true
	} else {
		enqueued = d.Dispatch(sessionDispatchKey(env), wrapped)
	}
	if !enqueued {
		clearPending() // dropped by backpressure: not in flight anymore
	}
	return enqueued
}

// EnableOutbox switches agent replies (replyToSender) to the reliable
// outbox path: entries persist under dir before each publish attempt and
// a background flusher redelivers failures with backoff. Safe to call
// once; a second call is a no-op.
func (s *Service) EnableOutbox(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outbox != nil {
		return
	}
	ob := NewOutbox(dir, s.log, func(req PublishRequest) error {
		_, err := s.Publish(req)
		return err
	})
	s.outbox = ob
	ob.Start()
}

func (s *Service) getOutbox() *Outbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outbox
}

func (s *Service) replyToSender(orig protocol.MessageEnvelope, content string, isError bool) {
	if orig.From == "" {
		return
	}
	if strings.HasSuffix(orig.Type, ".response") || strings.HasSuffix(orig.Type, ".error") {
		return
	}
	replyType := replyTypeFor(orig.Type, isError)
	req := PublishRequest{
		TargetNode: orig.From,
		Type:       replyType,
		SessionKey: orig.SessionKey,
		Message:    content,
	}

	if ob := s.getOutbox(); ob != nil {
		// T2.7 reliable return path: persist first, then attempt an
		// immediate delivery; the flusher redelivers on failure. The
		// attempt also advances retry state, so the first failure
		// already counts toward the dead-letter budget.
		entry, enqErr := ob.Enqueue(req)
		if enqErr != nil {
			// Persistence failed — fall back to best-effort publish so
			// the reply is not lost purely because the disk hiccupped.
			s.log.Error("outbox enqueue failed; reply is best-effort",
				logging.Event("message.reply.failed"),
				logging.To(orig.From),
				logging.MessageID(orig.ID),
				logging.Error(enqErr),
			)
			if err := s.publishToBus(req); err != nil {
				s.log.Warn("send reply to sender failed",
					logging.Event("message.reply.failed"),
					logging.To(orig.From),
					logging.MessageID(orig.ID),
					logging.Error(err),
				)
			}
			return
		}
		ob.attempt(*entry)
		return
	}

	if err := s.publishToBus(req); err != nil {
		s.log.Warn("send reply to sender failed",
			logging.Event("message.reply.failed"),
			logging.To(orig.From),
			logging.MessageID(orig.ID),
			logging.Error(err),
		)
	}
}

func (s *Service) publishToBus(req PublishRequest) error {
	_, err := s.Publish(req)
	return err
}

func replyTypeFor(msgType string, isError bool) string {
	suffix := ".response"
	if isError {
		suffix = ".error"
	}
	if idx := strings.Index(msgType, "."); idx != -1 {
		return msgType[:idx] + suffix
	}
	return "msg" + suffix
}

func (s *Service) messageHandler() MessageHandler {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handler
}

// isDeliverableType returns true for message types that should be forwarded to local agent handlers.
// Each prefix is a module name (e.g. "chat"); the type matches if it starts with "chat.".
func isDeliverableType(t string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(t, p+".") {
			return true
		}
	}
	return false
}

func cloneMetadata(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (s *Service) peerPublicKey(peerNode string) (ed25519.PublicKey, error) {
	peer, ok := s.peers.Get(peerNode)
	if !ok {
		return nil, errors.New("peer not found")
	}
	if peer.Metadata == nil {
		return nil, errors.New("peer metadata is empty")
	}
	v, ok := peer.Metadata["publicKey"].(string)
	if !ok || v == "" {
		return nil, errors.New("peer public key is unavailable")
	}
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, errors.New("invalid peer public key size")
	}
	return ed25519.PublicKey(b), nil
}

func (s *Service) signatureInput(env protocol.MessageEnvelope) string {
	return join(env.Type, env.From, env.To, fmt.Sprintf("%d", env.Ts), env.Content, env.ID)
}

func join(parts ...string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\n"
		}
		out += p
	}
	return out
}

func randID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func newSessionKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		ts := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(ts >> ((i % 8) * 8))
		}
	}

	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	return hex.EncodeToString(b[0:4]) + "-" +
		hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" +
		hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:16])
}
