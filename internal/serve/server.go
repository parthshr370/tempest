// Package serve hosts durable sessions over stdio and authenticated WebSockets.
package serve

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"go.harness.dev/harness/internal/acp"
	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest"
)

// Options sets process-wide session defaults and lifecycle deadlines.
type Options struct {
	SessionDir        string
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	PermissionTimeout time.Duration
	Token             string
	DefaultNew        tempest.NewSession
	Provider          tempest.ProviderConfig
	Stack             *tempest.StackConfig
	Clock             acp.Clock
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
	sessions    map[string]*entry
	connections map[*rpc.Conn]struct{}
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
		cfg, err := (config.Config{}).Resolve()
		if err != nil {
			return nil, err
		}
		opts.SessionDir = cfg.SessionDir()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{opts: opts, sessions: make(map[string]*entry), connections: make(map[*rpc.Conn]struct{}), ctx: ctx, cancel: cancel, closeDone: make(chan struct{}), reaperDone: make(chan struct{}), broker: acp.NewBroker(opts.PermissionTimeout, opts.Clock)}
	go s.reaper()
	return s, nil
}

// RunContext survives connection loss and ends only at server shutdown.
func (s *Server) RunContext() context.Context { return s.ctx }

// New creates a session using defaults for fields omitted by the client.
func (s *Server) New(ctx context.Context, cwd string, n *tempest.NewSession) (*tempest.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, tempest.ErrClosed
	}
	settings := *n
	if settings.Model == "" {
		settings.Model = s.opts.DefaultNew.Model
	}
	if settings.PermissionMode == "" {
		settings.PermissionMode = s.opts.DefaultNew.PermissionMode
	}
	// New receives the merged budget from ACP; callers outside ACP use DefaultNew explicitly.
	session, err := tempest.Open(ctx, tempest.Options{SessionDir: s.opts.SessionDir, Cwd: cwd, New: &settings, Provider: s.opts.Provider, Stack: s.opts.Stack, Prompter: s.broker})
	if err != nil {
		return nil, err
	}
	s.sessions[session.ID()] = &entry{session: session, lastActive: time.Now()}
	return session, nil
}

// Defaults returns the defaults used when session/new omits Tempest overrides.
func (s *Server) Defaults() tempest.NewSession { return s.opts.DefaultNew }

// Resume reuses a live writer instead of reopening its journal.
func (s *Server) Resume(ctx context.Context, cwd, id string) (*tempest.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, tempest.ErrClosed
	}
	if e := s.sessions[id]; e != nil {
		if e.closing {
			return nil, tempest.ErrClosed
		}
		e.lastActive = time.Now()
		return e.session, nil
	}
	r := tempest.ResumeSession{SessionID: id, Budget: s.opts.DefaultNew.Budget, PermissionMode: s.opts.DefaultNew.PermissionMode}
	session, err := tempest.Open(ctx, tempest.Options{SessionDir: s.opts.SessionDir, Cwd: cwd, Resume: &r, Provider: s.opts.Provider, Stack: s.opts.Stack, Prompter: s.broker})
	if err != nil {
		return nil, err
	}
	s.sessions[session.ID()] = &entry{session: session, lastActive: time.Now()}
	return session, nil
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

// CloseSession aborts and closes a journal before making its ID reopenable.
func (s *Server) CloseSession(ctx context.Context, id string) error {
	s.mu.Lock()
	e := s.sessions[id]
	if e == nil || e.closing {
		s.mu.Unlock()
		return journal.ErrSessionNotFound
	}
	e.closing = true
	s.mu.Unlock()
	err := e.session.Close(ctx)
	s.mu.Lock()
	if err == nil {
		delete(s.sessions, id)
	} else {
		e.closing = false
	}
	s.mu.Unlock()
	return err
}

// List pages durable journals, newest first, using an offset cursor.
func (s *Server) List(ctx context.Context, cwd, cursor string) ([]acp.SessionInfo, string, error) {
	if cursor != "" {
		if _, err := strconv.Atoi(cursor); err != nil {
			return nil, "", rpc.NewError(-32602, "invalid_params", "invalid cursor")
		}
	}
	var result []acp.SessionInfo
	err := filepath.WalkDir(s.opts.SessionDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		loaded, err := journal.Load(ctx, path)
		if err != nil {
			return err
		}
		if cwd != "" && loaded.Header.Cwd != cwd {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		result = append(result, acp.SessionInfo{SessionID: loaded.Header.ID, Cwd: loaded.Header.Cwd, Title: loaded.Header.Title, UpdatedAt: info.ModTime(), LastRun: loaded.LastRun})
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].UpdatedAt.Equal(result[j].UpdatedAt) {
			return result[i].SessionID > result[j].SessionID
		}
		return result[i].UpdatedAt.After(result[j].UpdatedAt)
	})
	offset := 0
	if cursor != "" {
		offset, _ = strconv.Atoi(cursor)
	}
	if offset < 0 || offset > len(result) {
		return nil, "", rpc.NewError(-32602, "invalid_params", "invalid cursor")
	}
	end := min(offset+100, len(result))
	next := ""
	if end < len(result) {
		next = strconv.Itoa(end)
	}
	page := append([]acp.SessionInfo{}, result[offset:end]...)
	return page, next, nil
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
		err := e.session.Close(ctx)
		cancel()
		s.mu.Lock()
		if err == nil {
			delete(s.sessions, e.session.ID())
		} else {
			e.closing = false
		}
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
	mux.HandleFunc("/acp", func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Token != "" {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if token == r.Header.Get("Authorization") {
				token = r.URL.Query().Get("token")
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(s.opts.Token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			return
		}
		_ = s.serveConn(rpc.NewWebSocket(ws))
	})
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
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
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
	var wg sync.WaitGroup
	errCh := make(chan error, len(entries))
	for _, e := range entries {
		wg.Go(func() {
			if err := e.session.Close(ctx); err != nil {
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
	s.mu.Lock()
	for c := range s.connections {
		_ = c.Close()
	}
	if s.http != nil {
		_ = s.http.Close()
	}
	s.mu.Unlock()
	<-s.reaperDone
}
