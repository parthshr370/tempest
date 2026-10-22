package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.harness.dev/harness/internal/build"
	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/mcpwire"
	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest"
)

type call struct {
	ctx       context.Context
	cancel    context.CancelFunc
	cancelled bool // guarded by server.mu, including reply retirement
	token     json.RawMessage
	form      bool
	revision  string
}

type entry struct {
	session    *tempest.Session
	router     *routingPrompter
	cwd, model string
	busy       bool
	last       time.Time
}

type server struct {
	opts               Options
	conn               *rpc.Conn
	mu                 sync.Mutex
	sessions           map[string]*entry
	calls              map[string]*call
	initialized, ready bool
	revision           string
	form               bool
}

// Serve negotiates one stdio MCP connection and closes all of its sessions.
func Serve(ctx context.Context, stream io.ReadWriteCloser, opts Options) error {
	if opts.PermissionTimeout == 0 {
		opts.PermissionTimeout = 5 * time.Minute
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = 30 * time.Minute
	}
	if opts.ShutdownTimeout == 0 {
		opts.ShutdownTimeout = 10 * time.Second
	}
	if opts.PermissionTimeout < 0 || opts.IdleTimeout < 0 || opts.ShutdownTimeout < 0 {
		return fmt.Errorf("server timeouts must be positive")
	}
	if opts.Cwd == "" {
		var err error
		opts.Cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	if opts.SessionDir == "" {
		cfg, err := (config.Config{}).Resolve()
		if err != nil {
			return err
		}
		opts.SessionDir = cfg.SessionDir()
	}
	if !filepath.IsAbs(opts.Cwd) {
		return fmt.Errorf("server cwd must be absolute")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &server{opts: opts, conn: rpc.NewStream(stream), sessions: map[string]*entry{}, calls: map[string]*call{}}
	s.conn.SetDrainOnEOF(true)
	stopping := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
		case <-s.conn.Done():
		case <-stopping:
		}
		s.mu.Lock()
		for _, c := range s.calls {
			c.cancel()
		}
		s.mu.Unlock()
		shutdown, cancel := context.WithTimeout(context.Background(), opts.ShutdownTimeout)
		defer cancel()
		_ = s.conn.Drain(shutdown)
		s.mu.Lock()
		sessions := make([]*tempest.Session, 0, len(s.sessions))
		for _, e := range s.sessions {
			if e.session != nil {
				sessions = append(sessions, e.session)
			}
		}
		s.mu.Unlock()
		var wg sync.WaitGroup
		for _, sess := range sessions {
			wg.Go(func() {
				if err := sess.Close(shutdown); err != nil {
					opts.Logger.Warn("MCP session close failed", "error", err)
				}
			})
		}
		joined := make(chan struct{})
		go func() { wg.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-shutdown.Done():
		}
		_ = s.conn.Close()
	}()
	reaperCtx, stopReaper := context.WithCancel(context.Background())
	reaperDone := make(chan struct{})
	go func() { defer close(reaperDone); s.reap(reaperCtx) }()
	err := s.conn.ServeOrdered(s.handle)
	close(stopping)
	<-stopped
	stopReaper()
	<-reaperDone
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func invalid(err error) *rpc.Error { return rpc.NewError(-32602, "invalid_params", err.Error()) }

func (s *server) handle(ctx context.Context, method string, raw json.RawMessage) (any, *rpc.Error) {
	if strings.HasPrefix(method, "notifications/") {
		switch method {
		case "notifications/initialized":
			if s.initialized {
				s.ready = true
			}
		case "notifications/cancelled":
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(raw, &p) == nil {
				s.mu.Lock()
				if c := s.calls[string(bytes.TrimSpace(p.RequestID))]; c != nil {
					c.cancelled = true
					c.cancel()
				}
				s.mu.Unlock()
			}
		}
		return nil, nil
	}
	switch method {
	case "initialize":
		if s.initialized {
			return nil, invalid(fmt.Errorf("already initialized"))
		}
		var p struct {
			ProtocolVersion string                     `json:"protocolVersion"`
			Capabilities    map[string]json.RawMessage `json:"capabilities"`
			ClientInfo      json.RawMessage            `json:"clientInfo"`
		}
		if json.Unmarshal(raw, &p) != nil || p.ProtocolVersion == "" {
			return nil, invalid(fmt.Errorf("initialize requires protocolVersion"))
		}
		s.revision = p.ProtocolVersion
		if s.revision != mcpwire.SupportedVersions[0] && s.revision != mcpwire.SupportedVersions[1] {
			s.revision = mcpwire.PreferredVersion
		}
		s.form = supportsForm(s.revision, p.Capabilities["elicitation"])
		s.initialized = true
		return map[string]any{"protocolVersion": s.revision, "capabilities": map[string]any{"tools": map[string]bool{"listChanged": false}}, "serverInfo": map[string]string{"name": "Tempest", "version": build.Current().Version}}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list", "tools/call":
		if !s.ready {
			return nil, rpc.NewError(-32002, "not_initialized", "initialize and notifications/initialized are required")
		}
	default:
		return nil, rpc.NewError(-32601, "method_not_found", "unknown method: "+method)
	}
	if method == "tools/list" {
		return map[string]any{"tools": []json.RawMessage{toolDefinition}}, nil
	}
	var p struct {
		Name      string                     `json:"name"`
		Arguments json.RawMessage            `json:"arguments"`
		Meta      map[string]json.RawMessage `json:"_meta"`
	}
	if err := decodeStrict(raw, &p); err != nil {
		return nil, invalid(err)
	}
	if p.Name != "tempest_run" {
		return nil, invalid(fmt.Errorf("unknown tool %q", p.Name))
	}
	var in RunInput
	if err := decodeRunInput(p.Arguments, &in); err != nil {
		return nil, invalid(err)
	}
	settings, err := in.settings(s.opts.DefaultNew)
	if err != nil {
		return nil, invalid(err)
	}
	tokenRaw := p.Meta["progressToken"]
	if len(tokenRaw) > 0 {
		var token any
		d := json.NewDecoder(bytes.NewReader(tokenRaw))
		d.UseNumber()
		if d.Decode(&token) != nil {
			return nil, invalid(fmt.Errorf("invalid progress token"))
		}
		switch v := token.(type) {
		case string:
		case json.Number:
			if strings.ContainsAny(string(v), ".eE") {
				return nil, invalid(fmt.Errorf("progress token must be a string or integer"))
			}
		default:
			return nil, invalid(fmt.Errorf("progress token must be a string or integer"))
		}
	}
	id := string(rpc.RequestID(ctx))
	if id == "" || id == "null" {
		return nil, invalid(fmt.Errorf("tools/call requires a request id"))
	}
	callCtx, cancel := context.WithCancel(ctx)
	c := &call{ctx: callCtx, cancel: cancel, token: tokenRaw, form: s.form, revision: s.revision}
	s.mu.Lock()
	if s.calls[id] != nil {
		s.mu.Unlock()
		cancel()
		return nil, invalid(fmt.Errorf("request id is already active"))
	}
	s.calls[id] = c
	var reserved *entry
	if in.SessionID != "" {
		reserved = s.sessions[in.SessionID]
		if reserved != nil && reserved.busy {
			delete(s.calls, id)
			s.mu.Unlock()
			cancel()
			return failure(tempest.ErrRunActive), nil
		}
		if reserved == nil {
			reserved = &entry{busy: true}
			s.sessions[in.SessionID] = reserved
		} else {
			reserved.busy = true
		}
	}
	s.mu.Unlock()
	return rpc.Deferred(func() (any, *rpc.Error) {
		value := s.run(c, in, settings, reserved)
		s.mu.Lock()
		cancelled := c.cancelled
		delete(s.calls, id)
		s.mu.Unlock()
		cancel()
		if cancelled {
			return rpc.NoResponse{}, nil
		}
		return value, nil
	}), nil
}

func (s *server) run(c *call, in RunInput, settings tempest.NewSession, e *entry) toolResult {
	key := in.SessionID
	defer func() {
		if e != nil {
			s.mu.Lock()
			if e.session == nil {
				delete(s.sessions, key)
			} else {
				e.busy = false
				e.last = time.Now()
			}
			s.mu.Unlock()
		}
	}()
	if err := c.ctx.Err(); err != nil {
		return failure(err)
	}
	if in.Cwd != "" {
		in.Cwd = filepath.Clean(in.Cwd)
	}
	cwd := in.Cwd
	if cwd == "" {
		cwd = s.opts.Cwd
	}
	if e != nil && e.session != nil {
		if in.Cwd != "" && in.Cwd != e.cwd {
			return failure(fmt.Errorf("cwd differs from resumed session workspace"))
		}
		if settings.Model != e.model {
			return failure(fmt.Errorf("model differs from live session; let it close before reopening with another model"))
		}
	} else {
		router := &routingPrompter{conn: s.conn, timeout: s.opts.PermissionTimeout}
		options := tempest.Options{SessionDir: s.opts.SessionDir, Cwd: cwd, New: &settings, Provider: s.opts.Provider, Stack: s.opts.Stack, Prompter: router, Logger: s.opts.Logger}
		if in.SessionID != "" {
			path, loaded, err := s.lookup(c.ctx, in.SessionID)
			if err != nil {
				return failure(err)
			}
			if in.Cwd != "" && in.Cwd != loaded.Header.Cwd {
				return failure(fmt.Errorf("cwd differs from resumed session workspace"))
			}
			cwd = loaded.Header.Cwd
			options.Cwd = cwd
			options.New = nil
			options.Resume = &tempest.ResumeSession{Path: path, Model: settings.Model, Budget: settings.Budget, PermissionMode: settings.PermissionMode, MCPServers: settings.MCPServers}
		}
		sess, err := tempest.Open(c.ctx, options)
		if err != nil {
			return failure(err)
		}
		if e == nil {
			e = &entry{}
		}
		s.mu.Lock()
		e.session = sess
		e.router = router
		e.cwd = cwd
		e.model = settings.Model
		e.busy = true
		key = sess.ID()
		s.sessions[key] = e
		s.mu.Unlock()
	}
	if err := e.session.SetBudget(settings.Budget); err != nil {
		return failure(err)
	}
	e.router.set(c)
	defer e.router.clear(c)
	_, updates, unwatch := e.session.Watch(context.Background(), tempest.WatchOptions{Buffer: 256})
	defer unwatch()
	if err := c.ctx.Err(); err != nil {
		return failure(err)
	}
	run, err := e.session.Begin(c.ctx, tempest.Prompt{Text: in.Prompt})
	if err != nil {
		return failure(err)
	}
	done := make(chan struct{})
	var result tempest.RunResult
	var waitErr error
	go func() { result, waitErr = run.Wait(); close(done) }()
	progress := progressWriter{server: s, call: c}
	if id, err := run.Started(c.ctx); err == nil {
		progress.send("Session "+e.session.ID()+", run "+id+" started", true)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	pending := ""
	for {
		select {
		case update, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			if update.Kind == "tempest/gap" {
				pending = "updates skipped; run continues"
				updates = nil
			} else if message := progressMessage(update); message != "" {
				pending = message
			}
		case <-ticker.C:
			if pending != "" {
				progress.send(pending, false)
				pending = ""
			}
		case <-done:
			if result.RunID == "" {
				if waitErr != nil {
					return failure(waitErr)
				}
				if result.Err != nil {
					return failure(result.Err)
				}
				return failure(fmt.Errorf("run ended before durable start"))
			}
			progress.send("Run finished: "+string(result.StopReason), true)
			return makeResult(s.opts.SessionDir, e.session, run, result)
		}
	}
}

func (s *server) lookup(ctx context.Context, id string) (string, *journal.Loaded, error) {
	var found string
	var loaded *journal.Loaded
	err := filepath.WalkDir(s.opts.SessionDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), "_"+id+".jsonl") {
			return nil
		}
		value, err := journal.Load(ctx, path)
		if err != nil {
			return err
		}
		if value.Header.ID == id {
			found = path
			loaded = value
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	if loaded == nil {
		return "", nil, journal.ErrSessionNotFound
	}
	return found, loaded, nil
}

func (s *server) reap(ctx context.Context) {
	ticker := time.NewTicker(min(time.Minute, s.opts.IdleTimeout))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var expired []*tempest.Session
			s.mu.Lock()
			for id, e := range s.sessions {
				if !e.busy && e.session != nil && time.Since(e.last) >= s.opts.IdleTimeout {
					delete(s.sessions, id)
					expired = append(expired, e.session)
				}
			}
			s.mu.Unlock()
			for _, sess := range expired {
				closeCtx, cancel := context.WithTimeout(ctx, s.opts.ShutdownTimeout)
				_ = sess.Close(closeCtx)
				cancel()
			}
		}
	}
}

type progressWriter struct {
	server *server
	call   *call
	last   time.Time
	count  int
}

func (p *progressWriter) send(message string, flush bool) {
	if len(p.call.token) == 0 {
		return
	}
	if delay := time.Until(p.last.Add(100 * time.Millisecond)); delay > 0 {
		if !flush {
			return
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-p.call.ctx.Done():
			return
		}
	}
	p.server.mu.Lock()
	defer p.server.mu.Unlock()
	if p.call.cancelled || p.call.ctx.Err() != nil {
		return
	}
	p.count++
	ctx, cancel := context.WithTimeout(p.call.ctx, 10*time.Millisecond)
	defer cancel()
	if p.server.conn.Notify(ctx, "notifications/progress", map[string]any{"progressToken": p.call.token, "progress": p.count, "message": message}) == nil {
		p.last = time.Now()
	}
}

func progressMessage(update tempest.Update) string {
	switch update.Kind {
	case "tempest/run":
		return "Run state changed"
	case "tool_call", "tool_call_update":
		var p struct {
			ToolCallID string `json:"toolCallId"`
			Status     string `json:"status"`
		}
		if json.Unmarshal(update.Update, &p) == nil && p.Status != "" {
			return "Tool call " + safeLabel(p.ToolCallID) + ": " + p.Status
		}
	case "tempest/turn":
		return "Model turn in progress"
	case "agent_message_chunk":
		return "Assistant response in progress"
	}
	return ""
}
