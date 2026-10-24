// Package mcpclient adapts session-owned MCP servers to ordinary gated tools.
package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/internal/mcpwire"
	"go.harness.dev/harness/internal/permission"
)

// StartupError identifies a failed endpoint without exposing its configuration.
type StartupError struct {
	Server string
	Phase  string
	Err    error
}

// Error retains protocol diagnostics for local callers.
func (e *StartupError) Error() string {
	return fmt.Sprintf("mcp_startup: server %s %s: %v", e.Server, e.Phase, e.Err)
}

// Unwrap lets protocol adapters classify the underlying failure.
func (e *StartupError) Unwrap() error { return e.Err }

// Manager owns connections and complete catalogs for one Tempest session.
type Manager struct {
	ctx          context.Context
	cancel       context.CancelFunc
	cwd          string
	spillDir     string
	logger       *slog.Logger
	servers      []*server
	closeOnce    sync.Once
	closeErr     error
	admitProcess func(context.Context) (func(), error)
}

type progressCall struct {
	update agent.AgentToolUpdateCallback
	last   float64
	seen   bool
}
type server struct {
	manager        *Manager
	config         mcpconfig.Server
	startupTimeout time.Duration
	callTimeout    time.Duration
	mu             sync.Mutex
	client         *mcpwire.Client
	tools          []agent.AgentTool
	refreshDone    chan struct{}
	refreshing     bool
	dirty          bool
	unavailable    error
	closed         bool
	token          chan struct{}
	progress       map[string]*progressCall
	next           atomic.Uint64
}

// Open discovers every configured server or closes all partially opened peers.
func Open(ctx context.Context, cwd, spillDir string, configs []mcpconfig.Server, logger *slog.Logger, admitProcess func(context.Context) (func(), error)) (*Manager, error) {
	normalized, err := mcpconfig.Normalize(configs)
	if err != nil {
		return nil, err
	}
	if len(normalized) > 0 {
		cwd, err = filepath.EvalSymlinks(cwd)
		if err != nil {
			return nil, fmt.Errorf("mcp_config: workspace: %w", err)
		}
	}
	if spillDir != "" {
		spillDir, err = filepath.Abs(spillDir)
		if err != nil {
			return nil, err
		}
	}
	if logger == nil {
		logger = slog.Default()
	}
	lifetime, cancel := context.WithCancel(context.Background())
	manager := &Manager{ctx: lifetime, cancel: cancel, cwd: cwd, spillDir: spillDir, logger: logger, admitProcess: admitProcess}
	for _, cfg := range normalized {
		peer := &server{manager: manager, config: cfg, startupTimeout: mustDuration(cfg.StartupTimeout), callTimeout: mustDuration(cfg.CallTimeout), token: make(chan struct{}, 1), progress: map[string]*progressCall{}}
		peer.token <- struct{}{}
		manager.servers = append(manager.servers, peer)
	}
	if err = manager.Prepare(ctx); err != nil {
		closeCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = manager.Close(closeCtx)
		return nil, err
	}
	return manager, nil
}

// Tools returns detached definitions in model-name order, without mutable catalog slices.
func (m *Manager) Tools() []agent.AgentTool {
	return m.bundleTools()
}

// Defaults returns per-tool trusted server defaults for the currently installed catalog.
func (m *Manager) Defaults() map[string]permission.Decision {
	defaults := map[string]permission.Decision{}
	for _, peer := range m.servers {
		decision, _ := permission.ParseDecision(peer.config.Permission)
		peer.mu.Lock()
		for _, tool := range peer.tools {
			defaults[tool.Name] = decision
		}
		peer.mu.Unlock()
	}
	return defaults
}

// Prepare joins pending refreshes and reconnects unavailable servers once before a run.
func (m *Manager) Prepare(ctx context.Context) error {
	for _, peer := range m.servers {
		peer.mu.Lock()
		if peer.closed {
			peer.mu.Unlock()
			return errors.New("mcp_startup: manager closed")
		}
		if !peer.refreshing && (peer.client == nil || !peer.client.Alive() || peer.unavailable != nil) {
			peer.startRefreshLocked(ctx, true)
		}
		done := peer.refreshDone
		refreshing := peer.refreshing
		peer.mu.Unlock()
		if refreshing {
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			case <-m.ctx.Done():
				return m.ctx.Err()
			}
		}
		peer.mu.Lock()
		err := peer.unavailable
		peer.mu.Unlock()
		if err != nil {
			return &StartupError{Server: peer.config.Name, Phase: "connect/discovery", Err: err}
		}
	}
	return ctx.Err()
}

func (s *server) startRefreshLocked(ctx context.Context, reconnect bool) {
	s.refreshing = true
	s.refreshDone = make(chan struct{})
	done := s.refreshDone
	go func() {
		refreshCtx, cancel := context.WithTimeout(ctx, s.startupTimeout)
		stop := context.AfterFunc(s.manager.ctx, cancel)
		defer func() { stop(); cancel() }()
		var err error
		s.mu.Lock()
		client := s.client
		s.mu.Unlock()
		if reconnect {
			if client != nil {
				closeCtx, closeCancel := context.WithTimeout(context.Background(), retirementTimeout)
				_ = client.Close(closeCtx)
				closeCancel()
			}
			client, err = mcpwire.Dial(refreshCtx, s.config, s.manager.cwd, mcpwire.Handlers{Notify: s.notification, AdmitProcess: s.manager.admitProcess})
			s.mu.Lock()
			s.client = client
			s.mu.Unlock()
		}
		for err == nil {
			s.mu.Lock()
			s.dirty = false
			s.mu.Unlock()
			var tools []agent.AgentTool
			tools, err = s.discover(refreshCtx, client)
			s.mu.Lock()
			if err == nil {
				s.tools = tools
				s.unavailable = nil
			}
			again := s.dirty && err == nil && !s.closed
			if !again {
				s.unavailable = err
				s.refreshing = false
				close(done)
			}
			s.mu.Unlock()
			if !again {
				return
			}
		}
		s.mu.Lock()
		s.unavailable = err
		s.refreshing = false
		close(done)
		s.mu.Unlock()
	}()
}
func mustDuration(value string) time.Duration {
	duration, _ := time.ParseDuration(value)
	return duration
}

// Close cancels refresh/calls, retires transports, and joins their readers and children.
func (m *Manager) Close(ctx context.Context) error {
	m.closeOnce.Do(func() {
		m.cancel()
		for _, peer := range m.servers {
			peer.mu.Lock()
			peer.closed = true
			client := peer.client
			done := peer.refreshDone
			refreshing := peer.refreshing
			peer.mu.Unlock()
			if client != nil {
				m.closeErr = errors.Join(m.closeErr, client.Close(ctx))
			}
			if refreshing {
				select {
				case <-done:
				case <-ctx.Done():
					m.closeErr = errors.Join(m.closeErr, ctx.Err())
				}
			}
			// Dial may have published its client after the first snapshot.
			peer.mu.Lock()
			latest := peer.client
			peer.mu.Unlock()
			if latest != nil && latest != client {
				m.closeErr = errors.Join(m.closeErr, latest.Close(ctx))
			}
		}
	})
	return m.closeErr
}
