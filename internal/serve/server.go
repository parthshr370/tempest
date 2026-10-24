// Package serve hosts durable sessions over stdio and authenticated WebSockets.
package serve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"go.harness.dev/harness/internal/a2a"
	"go.harness.dev/harness/internal/acp"
	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest"
	"go.harness.dev/harness/tempest/extension"
)

// Options sets process-wide session defaults and lifecycle deadlines.
type Options struct {
	SessionDir         string
	IdleTimeout        time.Duration
	ShutdownTimeout    time.Duration
	PermissionTimeout  time.Duration
	Token              string
	DefaultNew         tempest.NewSession
	Provider           tempest.ProviderConfig
	Stack              *tempest.StackConfig
	ExtensionProcesses []extension.ProcessOptions
	Clock              acp.Clock
	A2ABaseURL         string
	Cwd                string
	// OnListen, when set, receives the bound address before ListenAndServe accepts connections.
	OnListen func(net.Addr)
}

type entry struct {
	session    *tempest.Session
	refs       int
	lastActive time.Time
	closing    bool
}

// Server owns all journal writers and accepted connections in this process.
type Server struct {
	mu          sync.Mutex
	opts        Options
	host        *tempest.Host
	sessions    map[string]*entry
	connections map[*rpc.Conn]struct{}
	opening     map[string]*opening
	openers     sync.WaitGroup
	a2a         *a2a.Server
	origin      string
	http        *http.Server
	listener    net.Listener
	closed      bool
	closeOnce   sync.Once
	closeDone   chan struct{}
	closeErr    error
	ctx         context.Context
	cancel      context.CancelFunc
	broker      *acp.Broker
	reaperDone  chan struct{}
}

// New resolves the default journal directory before accepting clients.
func New(opts Options) (*Server, error) {
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = 30 * time.Minute
	}
	if opts.ShutdownTimeout == 0 {
		opts.ShutdownTimeout = 10 * time.Second
	}
	if opts.PermissionTimeout == 0 {
		opts.PermissionTimeout = 5 * time.Minute
	}
	if opts.IdleTimeout < 0 || opts.ShutdownTimeout < 0 || opts.PermissionTimeout < 0 {
		return nil, errors.New("server timeouts must be positive")
	}
	if opts.SessionDir == "" {
		cfg, err := (config.Config{SkipProject: true}).Resolve()
		if err != nil {
			return nil, err
		}
		opts.SessionDir = cfg.SessionDir()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{opts: opts, sessions: make(map[string]*entry), opening: make(map[string]*opening), connections: make(map[*rpc.Conn]struct{}), ctx: ctx, cancel: cancel, closeDone: make(chan struct{}), reaperDone: make(chan struct{}), broker: acp.NewBroker(opts.PermissionTimeout, opts.Clock)}
	if err := s.initInterop(); err != nil {
		cancel()
		return nil, err
	}
	host, err := tempest.OpenHost(ctx, tempest.HostOptions{SessionDir: opts.SessionDir, Provider: opts.Provider, Stack: opts.Stack, Prompter: s.broker, Questioner: s.broker.Question, MCPServers: opts.DefaultNew.MCPServers, Options: tempest.Options{ExtensionProcesses: opts.ExtensionProcesses}})
	if err != nil {
		cancel()
		return nil, err
	}
	s.host = host
	go s.reaper()
	return s, nil
}

// RunContext survives connection loss and ends only at server shutdown.
func (s *Server) RunContext() context.Context { return s.ctx }

// New creates a session using defaults for fields omitted by the client.
func (s *Server) New(ctx context.Context, cwd string, n *tempest.NewSession) (*tempest.Session, error) {
	settings := *n
	if settings.Model == "" {
		settings.Model = s.opts.DefaultNew.Model
	}
	if settings.PermissionMode == "" {
		settings.PermissionMode = s.opts.DefaultNew.PermissionMode
	}
	if settings.MCPServers == nil {
		settings.MCPServers = s.opts.DefaultNew.MCPServers
	}
	servers, err := mcpconfig.Normalize(settings.MCPServers)
	if err != nil {
		return nil, err
	}
	settings.MCPServers = servers
	if settings.Environment.ID == "" {
		settings.Environment = s.opts.DefaultNew.Environment
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, tempest.ErrClosed
	}
	s.openers.Add(1)
	s.mu.Unlock()
	defer s.openers.Done()
	ctx, cancel := s.openContext(ctx)
	defer cancel()
	session, err := s.host.Create(ctx, tempest.AgentOptions{Cwd: cwd, New: &settings})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if !s.closed {
		s.sessions[session.ID()] = &entry{session: session, lastActive: time.Now()}
		s.mu.Unlock()
		return session, nil
	}
	s.mu.Unlock()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), s.opts.ShutdownTimeout)
	defer closeCancel()
	_ = s.host.CloseSession(closeCtx, session.ID())
	return nil, tempest.ErrClosed
}

// Fork publishes a pinned sibling in the same registry as ordinary sessions.
func (s *Server) Fork(ctx context.Context, parentID, entryID string, options tempest.ForkOptions) (*tempest.Session, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, tempest.ErrClosed
	}
	s.openers.Add(1)
	s.mu.Unlock()
	defer s.openers.Done()
	ctx, cancel := s.openContext(ctx)
	defer cancel()
	child, err := s.host.Fork(ctx, parentID, entryID, options)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	closed := s.closed
	if !closed {
		s.sessions[child.ID()] = &entry{session: child, lastActive: time.Now()}
	}
	s.mu.Unlock()
	if closed {
		return nil, errors.Join(tempest.ErrClosed, s.host.CloseSession(context.Background(), child.ID()))
	}
	return child, nil
}

// Defaults returns the defaults used when session/new omits Tempest overrides.
func (s *Server) Defaults() tempest.NewSession {
	settings := s.opts.DefaultNew
	settings.MCPServers, _ = mcpconfig.Normalize(settings.MCPServers)
	return settings
}

// Resume reuses a live writer instead of reopening its journal.
func (s *Server) Resume(ctx context.Context, cwd, id string, servers []tempest.MCPServerConfig) (*tempest.Session, error) {
	explicit := servers != nil
	var err error
	if explicit {
		servers, err = mcpconfig.Normalize(servers)
		if err != nil {
			return nil, err
		}
	}
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, tempest.ErrClosed
		}
		if e := s.sessions[id]; e != nil {
			if e.closing {
				s.mu.Unlock()
				return nil, tempest.ErrClosed
			}
			if explicit && !e.session.MCPServersEqual(servers) {
				s.mu.Unlock()
				return nil, tempest.ErrMCPConfigConflict
			}
			if cwd != "" && e.session.Snapshot().Cwd != cwd {
				s.mu.Unlock()
				return nil, errors.New("workspace differs from live session")
			}
			e.lastActive = time.Now()
			s.mu.Unlock()
			return e.session, nil
		}
		if pending := s.opening[id]; pending != nil {
			s.mu.Unlock()
			select {
			case <-pending.done:
				if pending.err != nil {
					return nil, pending.err
				}
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		pending := &opening{done: make(chan struct{})}
		s.opening[id] = pending
		s.openers.Add(1)
		s.mu.Unlock()
		return s.resumeOpen(ctx, cwd, id, servers, pending)
	}
}

// Acquire prevents the idle reaper from closing a session until release.
func (s *Server) Acquire(id string) (*tempest.Session, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.sessions[id]
	if s.closed || e == nil || e.closing {
		return nil, nil, journal.ErrSessionNotFound
	}
	e.refs++
	e.lastActive = time.Now()
	var once sync.Once
	return e.session, func() { once.Do(func() { s.mu.Lock(); e.refs--; e.lastActive = time.Now(); s.mu.Unlock() }) }, nil
}

// CloseSession permanently retires a session, even if joining its run times out.
// Its journal stays locked until that run finishes.
func (s *Server) CloseSession(ctx context.Context, id string) error {
	s.mu.Lock()
	e := s.sessions[id]
	if e == nil || e.closing {
		s.mu.Unlock()
		return journal.ErrSessionNotFound
	}
	e.closing = true
	s.mu.Unlock()
	err := s.host.CloseSession(ctx, id)
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
	return err
}

// List pages durable journals newest first with an opaque snapshot cursor.
func (s *Server) List(ctx context.Context, cwd, cursor string) ([]acp.SessionInfo, string, error) {
	page, err := s.host.List(ctx, tempest.ListOptions{Cwd: cwd, Cursor: cursor, Limit: 100})
	if err != nil {
		return nil, "", err
	}
	result := make([]acp.SessionInfo, 0, len(page.Sessions))
	for _, info := range page.Sessions {
		var last *journal.RunSummary
		if info.LastRun != nil {
			last = &journal.RunSummary{RunID: info.LastRun.RunID, StopReason: info.LastRun.StopReason, Error: info.LastRun.Error, Finished: info.LastRun.Finished, FinishedAt: info.LastRun.FinishedAt}
		}
		result = append(result, acp.SessionInfo{SessionID: info.SessionID, Cwd: info.Cwd, Title: info.Title, UpdatedAt: info.UpdatedAt, LastRun: last})
	}
	return result, page.NextCursor, nil
}

func (s *Server) reaper() {
	defer close(s.reaperDone)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-ticker.C:
			s.reap(now)
		}
	}
}
func (s *Server) reap(now time.Time) {
	s.mu.Lock()
	var stale []*entry
	for _, e := range s.sessions {
		if !e.closing && e.refs == 0 && e.session.Snapshot().Run.Status != "running" && now.Sub(e.lastActive) > s.opts.IdleTimeout {
			e.closing = true
			stale = append(stale, e)
		}
	}
	s.mu.Unlock()
	for _, e := range stale {
		ctx, cancel := context.WithTimeout(context.Background(), s.opts.ShutdownTimeout)
		// Close permanently rejects admission, even when its wait expires.
		_ = s.host.CloseSession(ctx, e.session.ID())
		cancel()
		s.mu.Lock()
		delete(s.sessions, e.session.ID())
		s.mu.Unlock()
	}
}

// ServeStdio serves one client. EOF doesn't cancel its accepted runs.
func (s *Server) ServeStdio(stream io.ReadWriteCloser) error {
	return s.serveConn(rpc.NewStream(stream))
}
func (s *Server) serveConn(c *rpc.Conn) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = c.Close()
		return tempest.ErrClosed
	}
	s.connections[c] = struct{}{}
	s.mu.Unlock()
	defer func() { _ = c.Close(); s.mu.Lock(); delete(s.connections, c); s.mu.Unlock() }()
	return acp.NewClient(c, s, s.broker).Serve()
}

// Handler exposes the authenticated /acp upgrade and unauthenticated /healthz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		sessions := len(s.sessions)
		running := 0
		for _, e := range s.sessions {
			if e.session.Snapshot().Run.Status == "running" {
				running++
			}
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int{"sessions": sessions, "running": running})
	})
	mux.Handle("/acp", s.authorize(true, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			return
		}
		_ = s.serveConn(rpc.NewWebSocket(ws))
	})))
	mux.Handle("/a2a", s.authorize(false, true, http.HandlerFunc(s.handleA2A)))
	mux.Handle("/.well-known/agent-card.json", s.publicCard())
	return mux
}

// ListenAndServe refuses unauthenticated binds outside numeric loopback or localhost.
func (s *Server) ListenAndServe(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if s.opts.Token == "" && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("bearer token required when listening outside loopback")
	}
	if s.opts.A2ABaseURL == "" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("A2A base URL required outside numeric loopback")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	if s.opts.A2ABaseURL == "" {
		if err := s.configureA2A("http://" + listener.Addr().String()); err != nil {
			_ = listener.Close()
			return err
		}
	}
	server := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = listener.Close()
		return tempest.ErrClosed
	}
	s.http = server
	s.listener = listener
	s.mu.Unlock()
	if s.opts.OnListen != nil {
		s.opts.OnListen(listener.Addr())
	}
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// Close stops admission, aborts runs, joins journals, then closes client transports.
// The caller's deadline can shorten, but never lengthen, ShutdownTimeout.
func (s *Server) Close(ctx context.Context) error {
	s.closeOnce.Do(func() { go s.shutdown() })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.closeDone:
		return s.closeErr
	}
}
func (s *Server) shutdown() {
	defer close(s.closeDone)
	s.mu.Lock()
	s.closed = true
	entries := make([]*entry, 0, len(s.sessions))
	for _, e := range s.sessions {
		entries = append(entries, e)
	}
	listener := s.listener
	s.mu.Unlock()
	if listener != nil {
		_ = listener.Close()
	}
	for _, e := range entries {
		e.session.Cancel()
	}
	s.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.ShutdownTimeout)
	defer cancel()
	opened := make(chan struct{})
	go func() { s.openers.Wait(); close(opened) }()
	select {
	case <-opened:
	case <-ctx.Done():
		s.closeErr = ctx.Err()
	}
	var wg sync.WaitGroup
	errCh := make(chan error, len(entries))
	for _, e := range entries {
		wg.Go(func() {
			if err := s.host.CloseSession(ctx, e.session.ID()); err != nil {
				errCh <- err
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		s.closeErr = ctx.Err()
	}
	select {
	case err := <-errCh:
		s.closeErr = errors.Join(s.closeErr, err)
	default:
	}
	s.closeErr = errors.Join(s.closeErr, s.host.Close(ctx))
	if s.a2a != nil {
		s.closeErr = errors.Join(s.closeErr, s.a2a.Close(ctx))
	}
	s.mu.Lock()
	connections := make([]*rpc.Conn, 0, len(s.connections))
	for connection := range s.connections {
		connections = append(connections, connection)
	}
	httpServer := s.http
	s.mu.Unlock()
	var drains sync.WaitGroup
	drainErrors := make(chan error, len(connections))
	for _, connection := range connections {
		drains.Go(func() {
			if err := connection.Drain(ctx); err != nil {
				drainErrors <- err
			}
			_ = connection.Close()
		})
	}
	drains.Wait()
	close(drainErrors)
	for err := range drainErrors {
		s.closeErr = errors.Join(s.closeErr, err)
	}
	if httpServer != nil {
		_ = httpServer.Close()
	}
	<-s.reaperDone
}
