package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"clawsynapse/internal/adapter"
	"clawsynapse/internal/api"
	"clawsynapse/internal/auth"
	"clawsynapse/internal/capability"
	"clawsynapse/internal/config"
	"clawsynapse/internal/discovery"
	"clawsynapse/internal/identity"
	"clawsynapse/internal/logging"
	"clawsynapse/internal/messaging"
	"clawsynapse/internal/natsbus"
	"clawsynapse/internal/replay"
	"clawsynapse/internal/store"
	"clawsynapse/internal/transfer"
	"clawsynapse/internal/trust"
	"clawsynapse/pkg/types"
)

type App struct {
	log       *slog.Logger
	cfg       config.Config
	api       *api.Server
	discovery *discovery.Service
	auth      *auth.Service
	trust     *trust.Service
	messaging *messaging.Service
	transfer  *transfer.Service
	capability *capability.Service
	bus       *natsbus.Client
	peers     *discovery.Registry
	identity  *identity.Identity
	// agentAdapter is kept for the exit hook: adapters with batched
	// session persistence (T2.3) flush on shutdown via Close().
	agentAdapter adapter.AgentAdapter
	// adapterCancel aborts in-flight adapter calls after the drain grace
	// expires (T2.6 graceful exit).
	adapterCancel context.CancelFunc
}

func New(cfg config.Config, version string) (*App, error) {
	fs := store.NewFSStore(cfg.DataDir)
	if err := fs.EnsureLayout(); err != nil {
		return nil, fmt.Errorf("init fs store: %w", err)
	}

	id, err := identity.LoadOrCreate(cfg.IdentityKeyPath, cfg.IdentityPubPath)
	if err != nil {
		return nil, fmt.Errorf("load identity: %w", err)
	}

	// derive DID and subject-safe node ID from public key
	nodeDID := identity.DeriveNodeDID(id.PublicKey)
	nodeID := identity.DeriveNodeID(nodeDID)

	log, err := logging.New(logging.Options{
		Level:     cfg.LogLevel,
		Format:    cfg.LogFormat,
		AddSource: cfg.LogAddSource,
		FilePath:  cfg.LogFilePath,
		Rotate: logging.RotateOptions{
			MaxSizeMB:  cfg.LogRotateMaxSizeMB,
			MaxBackups: cfg.LogRotateMaxBackups,
			MaxAgeDays: cfg.LogRotateMaxAgeDays,
			Compress:   cfg.LogRotateCompress,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("init logger: %w", err)
	}
	log = log.With(
		slog.String("service", "clawsynapsed"),
		slog.String("nodeId", nodeID),
		slog.String("did", nodeDID),
	)

	peers := discovery.NewRegistry()
	peers.Upsert(types.Peer{
		NodeID:      nodeID,
		DID:         nodeDID,
		AuthStatus:  types.AuthAuthenticated,
		TrustStatus: types.TrustTrusted,
		Inbox:       "clawsynapse.msg." + nodeID + ".inbox",
		Metadata: map[string]any{
			"publicKey": base64.RawURLEncoding.EncodeToString(id.PublicKey),
		},
	})

	hb, err := time.ParseDuration(cfg.HeartbeatInterval)
	if err != nil {
		return nil, fmt.Errorf("parse heartbeat interval: %w", err)
	}
	ttl, err := time.ParseDuration(cfg.AnnounceTTL)
	if err != nil {
		return nil, fmt.Errorf("parse announce ttl: %w", err)
	}

	bus, err := natsbus.Connect(context.Background(), cfg.NATSServers, "clawsynapsed-"+nodeID)
	if err != nil {
		return nil, fmt.Errorf("connect nats: %w", err)
	}

	replayGuard, err := replay.NewReplayGuard(fs, 10000, 10*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("init replay guard: %w", err)
	}

	discoverySvc := discovery.NewService(log.With(slog.String("component", "discovery")), bus, peers, fs, nodeID, nodeDID, base64.RawURLEncoding.EncodeToString(id.PublicKey), hb, ttl, cfg.TrustMode, cfg.AgentAdapter)
	authSvc := auth.NewService(log.With(slog.String("component", "auth")), peers, bus, nodeID, id, replayGuard, cfg.TrustMode)
	discoverySvc.SetAutoAuthenticator(authSvc.StartChallenge)
	trustSvc, err := trust.NewService(log.With(slog.String("component", "trust")), peers, bus, fs, nodeID, id, cfg.TrustAutoApprove)
	if err != nil {
		return nil, fmt.Errorf("init trust service: %w", err)
	}
	messagingSvc := messaging.NewService(log.With(slog.String("component", "messaging")), peers, bus, nodeID, id, cfg.TrustMode, cfg.DeliverablePrefixes)
	// T2.7: agent replies go through the durable outbox under the data dir.
	messagingSvc.EnableOutbox(filepath.Join(cfg.DataDir, "outbox"))
	// T2.2: duplicate inbox envelopes are dropped via the shared replay guard.
	messagingSvc.EnableReplayGuard(replayGuard)
	agentAdapter, err := newAgentAdapter(cfg, nodeID, log, fs)
	if err != nil {
		return nil, fmt.Errorf("init agent adapter: %w", err)
	}
	agentAdapterTimeout, err := resolveAgentAdapterTimeout(cfg)
	if err != nil {
		return nil, err
	}
	var handlerOpts []messaging.HandlerOption
	if cfg.AgentAdapter == "webhook" {
		handlerOpts = append(handlerOpts, messaging.WithFeedbackDelivery())
	}
	// todo.* runs get their own (longer) timeout from the task config so a
	// 60m run is not killed by the 10m generic adapter timeout (T1.3).
	if taskCfg := taskConfigFrom(cfg.Task); taskCfg.RunTimeout > 0 {
		handlerOpts = append(handlerOpts, messaging.WithTaskRunTimeout(taskCfg.RunTimeout))
	}
	// T2.6: a cancelable root context lets shutdown interrupt in-flight
	// adapter calls once the drain grace expires.
	adapterRootCtx, adapterCancel := context.WithCancel(context.Background())
	handlerOpts = append(handlerOpts, messaging.WithRootContext(adapterRootCtx))
	adapterHandler := messaging.NewAdapterMessageHandler(agentAdapter, agentAdapterTimeout, handlerOpts...)
	messagingSvc.SetMessageHandler(adapterHandler)

	transferSvc := transfer.NewService(
		log.With(slog.String("component", "transfer")),
		peers, bus, messagingSvc, nodeID, id, cfg.TrustMode,
		transfer.TransferConfig{
			TransferDir: cfg.TransferDir,
			MaxFileSize: cfg.TransferMaxFileSize,
			TTL:         cfg.TransferTTL,
		},
	)
	messagingSvc.SetTransferHandler(transferSvc.HandleTransferNotification)

	transferSvc.OnReceived(func(rec transfer.TransferRecord) {
		content, _ := json.Marshal(map[string]any{
			"transferId": rec.TransferID,
			"fileName":   rec.FileName,
			"fileSize":   rec.FileSize,
			"localPath":  rec.LocalPath,
			"mimeType":   rec.MimeType,
		})
		msg := messaging.IncomingMessage{
			Type:     "transfer.received",
			From:     rec.PeerNode,
			To:       nodeID,
			Message:  string(content),
			Metadata: rec.Metadata,
		}
		if _, err := adapterHandler.HandleMessage(msg); err != nil {
			log.Warn("deliver transfer.received to agent failed",
				slog.String("transferId", rec.TransferID),
				slog.String("error", err.Error()),
			)
		}
	})

	capabilitySvc := capability.NewService(
		log.With(slog.String("component", "capability")),
		bus, peers, transferSvc, agentAdapter, nodeID, id,
	)

	// Phase 3.4: the local API requires a bearer token (persisted under
	// the data dir with 0600, reused across restarts). Empty disables it
	// only when the data dir is unavailable, which New returns earlier.
	apiToken, err := api.LoadOrCreateAPIToken(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("init local api token: %w", err)
	}
	log.Info("local api token ready", slog.String("path", filepath.Join(cfg.DataDir, "api_token")))

	apiServer := api.NewServer(cfg.LocalAPIAddr, peers, authSvc, trustSvc, messagingSvc, transferSvc, capabilitySvc, bus, agentAdapter, cfg.AgentAdapter, api.SelfInfo{
		NodeID:              nodeID,
		DID:                 nodeDID,
		IdentityFingerprint: identity.Fingerprint(id.PublicKey),
		TrustMode:           cfg.TrustMode,
	}, version, cfg, apiToken)

	return &App{
		log:       log,
		cfg:       cfg,
		api:       apiServer,
		discovery: discoverySvc,
		auth:      authSvc,
		trust:     trustSvc,
		messaging: messagingSvc,
		transfer:  transferSvc,
		capability: capabilitySvc,
		bus:       bus,
		peers:     peers,
		identity:  id,
		agentAdapter: agentAdapter,
		adapterCancel: adapterCancel,
	}, nil
}

func newAgentAdapter(cfg config.Config, nodeID string, log *slog.Logger, fs *store.FSStore) (adapter.AgentAdapter, error) {
	switch cfg.AgentAdapter {
	case "", "default":
		return adapter.NewDefaultAdapter(nodeID), nil
	case "openclaw":
		return adapter.NewOpenClawAdapter(adapter.OpenClawConfig{
			NodeID: nodeID,
			Logger: log.With(slog.String("component", "adapter"), slog.String("adapter", "openclaw")),
		})
	case "opencode":
		return adapter.NewOpenCodeAdapter(adapter.OpenCodeConfig{
			NodeID:       nodeID,
			Logger:       log.With(slog.String("component", "adapter"), slog.String("adapter", "opencode")),
			SessionStore: fs,
		})
	case "codex":
		return adapter.NewCodexAdapter(adapter.CodexConfig{
			NodeID:       nodeID,
			Logger:       log.With(slog.String("component", "adapter"), slog.String("adapter", "codex")),
			SessionStore: fs,
		})
	case "webhook":
		return adapter.NewWebhookAdapter(adapter.WebhookConfig{
			NodeID: nodeID,
			URL:    cfg.WebhookURL,
			Logger: log.With(slog.String("component", "adapter"), slog.String("adapter", "webhook")),
		})
	case "hermes":
		return adapter.NewHermesAdapter(adapter.HermesConfig{
			NodeID:       nodeID,
			Logger:       log.With(slog.String("component", "adapter"), slog.String("adapter", "hermes")),
			SessionStore: fs,
			AgentRole:    cfg.AgentRole,
			BaseURL:      cfg.HermesGatewayURL,
			APIKey:       cfg.HermesGatewayKey,
			Model:        cfg.HermesModel,
			ConfigPath:   cfg.HermesConfigPath,
			TodoMode:     cfg.HermesTodoMode,
			Task:         taskConfigFrom(cfg.Task),
			TaskStore:    store.NewTaskStore(cfg.DataDir),
		})
	default:
		return nil, fmt.Errorf("unsupported agent adapter: %s", cfg.AgentAdapter)
	}
}

func resolveAgentAdapterTimeout(cfg config.Config) (time.Duration, error) {
	timeout := strings.TrimSpace(cfg.AgentAdapterTimeout)
	if timeout == "" {
		return 10 * time.Minute, nil
	}
	d, err := time.ParseDuration(timeout)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("parse agent adapter timeout: %s", timeout)
	}
	return d, nil
}

// taskConfigFrom converts the config-file task settings into the adapter
// TaskConfig. Invalid duration strings fall back to the coordinator
// defaults (zero value).
func taskConfigFrom(t *config.TaskConfig) adapter.TaskConfig {
	var out adapter.TaskConfig
	if t == nil {
		return out
	}
	out.MaxConcurrentRuns = t.MaxConcurrentRuns
	out.QueueCapacity = t.QueueCapacity
	if d, err := time.ParseDuration(strings.TrimSpace(t.RunTimeout)); err == nil {
		out.RunTimeout = d
	}
	if d, err := time.ParseDuration(strings.TrimSpace(t.QueueWaitTimeout)); err == nil {
		out.QueueWaitTimeout = d
	}
	return out
}

func (a *App) Run(ctx context.Context) error {
	a.log.Info("starting clawsynapsed",
		slog.String("apiAddr", a.cfg.LocalAPIAddr),
		slog.String("trustMode", a.cfg.TrustMode),
		slog.String("identityFingerprint", identity.Fingerprint(a.identity.PublicKey)),
	)

	if err := a.auth.Start(); err != nil {
		return fmt.Errorf("start auth service: %w", err)
	}
	a.log.Info("auth subscriptions ready")
	if err := a.trust.Start(); err != nil {
		return fmt.Errorf("start trust service: %w", err)
	}
	a.log.Info("trust subscriptions ready")
	if err := a.messaging.Start(); err != nil {
		return fmt.Errorf("start messaging service: %w", err)
	}
	a.log.Info("messaging subscriptions ready")
	if err := a.bus.FlushTimeout(3 * time.Second); err != nil {
		a.log.Warn("nats not connected within timeout, transfer may be disabled", slog.String("error", err.Error()))
	}
	if err := a.transfer.Start(ctx); err != nil {
		return fmt.Errorf("start transfer service: %w", err)
	}
	if a.transfer.Enabled() {
		a.log.Info("transfer service ready")
	}
	if err := a.capability.Start(); err != nil {
		return fmt.Errorf("start capability service: %w", err)
	}
	a.log.Info("capability subscriptions ready")
	if err := a.bus.FlushTimeout(3 * time.Second); err != nil {
		a.log.Warn("nats flush timeout after control subscriptions", slog.String("error", err.Error()))
	}
	if err := a.discovery.Start(ctx); err != nil {
		return fmt.Errorf("start discovery service: %w", err)
	}
	a.log.Info("discovery subscriptions ready")
	if err := a.bus.FlushTimeout(3 * time.Second); err != nil {
		a.log.Warn("nats flush timeout after discovery start", slog.String("error", err.Error()))
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.api.Start()
	}()

	select {
	case <-ctx.Done():
		// T2.6 graceful exit: ① drain the messaging service (wait for
		// in-flight handler executions up to the grace period; timed-out
		// ones get an interruption .error reply), ② cancel the adapter
		// root context so gateway runs actually stop, ③ flush batched
		// adapter persistence, ④ close the bus, ⑤ shut the API down.
		graceCtx, graceCancel := context.WithTimeout(context.Background(), 30*time.Second)
		a.messaging.Drain(graceCtx)
		graceCancel()
		if a.adapterCancel != nil {
			a.adapterCancel()
		}
		if closer, ok := a.agentAdapter.(interface{ Close() }); ok {
			closer.Close()
		}
		a.bus.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return a.api.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}
