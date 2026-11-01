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
	"go.harness.dev/harness/internal/mcpoauth"
	"go.harness.dev/harness/internal/mcpwire"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/question"
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

// Options binds a Manager to its session. A zero field disables its feature.
type Options struct {
	Cwd          string
	SpillDir     string
	Logger       *slog.Logger
	AdmitProcess func(context.Context) (func(), error)
	// CredentialDir is the OAuth token store. Empty treats an HTTP 401 as a
	// rejected credential instead of starting authorization.
	CredentialDir string
	// Elicit shows server elicitations and authorization URLs to the user;
	// nil, or an answer of question.Unavailable, means no user can be
	// asked: elicitations are declined and authorization names the command.
	Elicit question.Elicitor
}

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
	elicit       question.Elicitor
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
	auth           *serverAuth
	mu             sync.Mutex
	client         *mcpwire.Client
	catalog        catalog
	refreshDone    chan struct{}
	refreshing     bool
	dirty          bool
	unavailable    error
	closed         bool
	token          chan struct{}
	progress       map[string]*progressCall
	next           atomic.Uint64
}

// Open discovers every configured server or closes all partially opened
// peers. A server that needs OAuth authorization does not fail Open: the
// first run authorizes it, or fails naming `tempest mcp login`.
func Open(ctx context.Context, configs []mcpconfig.Server, options Options) (*Manager, error) {
	normalized, err := mcpconfig.Normalize(configs)
	if err != nil {
		return nil, err
	}
	cwd, spillDir := options.Cwd, options.SpillDir
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
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	lifetime, cancel := context.WithCancel(context.Background())
	manager := &Manager{ctx: lifetime, cancel: cancel, cwd: cwd, spillDir: spillDir, logger: logger, admitProcess: options.AdmitProcess, elicit: options.Elicit}
	var store *mcpoauth.Store
	if options.CredentialDir != "" {
		store = mcpoauth.NewStore(options.CredentialDir)
	}
	for _, cfg := range normalized {
		peer := &server{manager: manager, config: cfg, startupTimeout: mustDuration(cfg.StartupTimeout), callTimeout: mustDuration(cfg.CallTimeout), token: make(chan struct{}, 1), progress: map[string]*progressCall{}}
		if store != nil && cfg.UsesOAuth() {
			if peer.auth, err = newServerAuth(cfg, store); err != nil {
				cancel()
				return nil, err
			}
		}
		peer.token <- struct{}{}
		manager.servers = append(manager.servers, peer)
	}
	if err = manager.prepare(ctx, false); err != nil {
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
		for _, tool := range peer.catalog.tools {
			defaults[tool.Name] = decision
		}
		peer.mu.Unlock()
	}
	return defaults
}

// Prepare joins pending refreshes and reconnects unavailable servers once
// before a run. A server that needs OAuth authorization is authorized here
// through Options.Elicit; without a user the run fails naming the command.
func (m *Manager) Prepare(ctx context.Context) error { return m.prepare(ctx, true) }

func (m *Manager) prepare(ctx context.Context, interactive bool) error {
	for _, peer := range m.servers {
		if err := peer.prepare(ctx, interactive); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *server) prepare(ctx context.Context, interactive bool) error {
	authorized := false
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return errors.New("mcp_startup: manager closed")
		}
		if !s.refreshing && (s.client == nil || !s.client.Alive() || s.unavailable != nil) {
			s.startRefreshLocked(ctx, true)
		}
		done := s.refreshDone
		refreshing := s.refreshing
		s.mu.Unlock()
		if refreshing {
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			case <-s.manager.ctx.Done():
				return s.manager.ctx.Err()
			}
		}
		s.mu.Lock()
		err := s.unavailable
		s.mu.Unlock()
		if err == nil {
			return nil
		}
		var login *mcpoauth.LoginRequired
		if !errors.As(err, &login) || s.auth == nil || authorized {
			return &StartupError{Server: s.config.Name, Phase: "connect/discovery", Err: err}
		}
		if !interactive {
			// Session startup has no user yet; the first run authorizes.
			s.manager.logger.Warn("MCP server needs authorization before its tools are offered", "server", s.config.Name)
			return nil
		}
		if err := s.authorize(ctx, login, s.auth.generation.Load()); err != nil {
			return &StartupError{Server: s.config.Name, Phase: "authorization", Err: err}
		}
		authorized = true
	}
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
			client, err = mcpwire.Dial(refreshCtx, s.config, s.manager.cwd, s.handlers())
			s.mu.Lock()
			s.client = client
			s.mu.Unlock()
		}
		for err == nil {
			s.mu.Lock()
			s.dirty = false
			s.mu.Unlock()
			var next catalog
			next, err = s.discover(refreshCtx, client)
			s.mu.Lock()
			if err == nil {
				s.catalog = next
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
