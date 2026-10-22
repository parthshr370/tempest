// Package a2a projects Tempest runs onto the A2A 1.0 JSON-RPC binding.
package a2a

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	model "github.com/a2aproject/a2a-go/v2/a2a"
	"go.harness.dev/harness/internal/build"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest"
)

const (
	maxRequestBytes    = 1 << 20
	maxRetainedTasks   = 10000
	completedRetention = 24 * time.Hour
)

// Backend keeps session writers and accepted runs owned by the host.
type Backend interface {
	New(context.Context, string, *tempest.NewSession) (*tempest.Session, error)
	Resume(context.Context, string, string, []tempest.MCPServerConfig) (*tempest.Session, error)
	Acquire(string) (*tempest.Session, func(), error)
	RunContext() context.Context
	Defaults() tempest.NewSession
}

// Options fixes the advertised address and host-selected workspace.
type Options struct {
	BaseURL, Cwd string
	Defaults     tempest.NewSession
}

// Server retains process-local task projections, not a second durable session store.
type Server struct {
	backend   Backend
	opts      Options
	card      *model.AgentCard
	mu        sync.Mutex
	tasks     map[model.TaskID]*task
	pending   int
	serial    uint64
	closed    bool
	observers sync.WaitGroup
	streams   sync.WaitGroup
	cursorKey [32]byte
	now       func() time.Time
	capacity  int
	retention time.Duration
}

// New validates operator-controlled addresses before accepting remote requests.
func New(backend Backend, opts Options) (*Server, error) {
	u, err := url.Parse(opts.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || strings.Contains(u.Hostname(), "*") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(opts.BaseURL, "/") {
		return nil, errors.New("A2A base URL must be an absolute HTTP(S) URL without credentials, query, fragment or trailing slash")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsUnspecified() {
		return nil, errors.New("A2A base URL cannot advertise a wildcard address")
	}
	if !filepath.IsAbs(opts.Cwd) {
		return nil, errors.New("A2A workspace must be absolute")
	}
	s := &Server{backend: backend, opts: opts, tasks: make(map[model.TaskID]*task), now: time.Now, capacity: maxRetainedTasks, retention: completedRetention}
	if _, err := rand.Read(s.cursorKey[:]); err != nil {
		return nil, err
	}
	s.card = &model.AgentCard{
		Name:                "Tempest",
		Description:         "A durable coding agent running in an operator-selected workspace.",
		Version:             build.Current().Version,
		SupportedInterfaces: []*model.AgentInterface{model.NewAgentInterface(opts.BaseURL+"/a2a", model.TransportProtocolJSONRPC)},
		Capabilities:        model.AgentCapabilities{Streaming: true},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Skills:              []model.AgentSkill{{ID: "coding", Name: "Coding", Description: "Inspect and change the configured workspace under host permission policy.", Tags: []string{"coding"}}},
	}
	return s, nil
}

// Card returns a copy for the HTTP host's security declaration.
func (s *Server) Card() *model.AgentCard { copy := *s.card; return &copy }

// Handler serves the A2A JSON-RPC endpoint. Authentication belongs to the HTTP host.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.handle) }

// Close prevents new tasks and streams, then joins observers and stream writers.
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.observers.Wait()
		s.streams.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type rpcError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    []errorInfo `json:"data,omitempty"`
}
type errorInfo struct {
	Type     string            `json:"@type"`
	Reason   string            `json:"reason"`
	Domain   string            `json:"domain"`
	Metadata map[string]string `json:"metadata,omitempty"`
}
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func protocolError(code int, reason string, metadata map[string]string) *rpcError {
	var name, protocolReason string
	switch code {
	case -32001:
		name, protocolReason = "TaskNotFoundError", "TASK_NOT_FOUND"
	case -32002:
		name, protocolReason = "TaskNotCancelableError", "TASK_NOT_CANCELABLE"
	case -32003:
		name, protocolReason = "PushNotificationNotSupportedError", "PUSH_NOTIFICATION_NOT_SUPPORTED"
	case -32004:
		name, protocolReason = "UnsupportedOperationError", "UNSUPPORTED_OPERATION"
	case -32005:
		name, protocolReason = "ContentTypeNotSupportedError", "CONTENT_TYPE_NOT_SUPPORTED"
	case -32006:
		name, protocolReason = "InvalidAgentResponseError", "INVALID_AGENT_RESPONSE"
	case -32008:
		name, protocolReason = "ExtensionSupportRequiredError", "EXTENSION_SUPPORT_REQUIRED"
	case -32009:
		name, protocolReason = "VersionNotSupportedError", "VERSION_NOT_SUPPORTED"
	}
	if reason != "" {
		if metadata == nil {
			metadata = make(map[string]string, 1)
		}
		metadata["reason"] = reason
	}
	return &rpcError{Code: code, Message: name, Data: []errorInfo{{Type: "type.googleapis.com/google.rpc.ErrorInfo", Reason: protocolReason, Domain: "a2a-protocol.org", Metadata: metadata}}}
}
func invalid() *rpcError { return &rpcError{Code: -32602, Message: "Invalid params"} }
func writeResponse(w http.ResponseWriter, id json.RawMessage, result any, e *rpcError) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	var encodedResult json.RawMessage
	if e == nil && result != nil {
		var err error
		encodedResult, err = json.Marshal(result)
		if err != nil {
			encodedResult = nil
			e = protocolError(-32006, "invalid_projection", nil)
		}
	}
	// Both raw fields are validated JSON, and the remaining fields are plain
	// protocol scalars. Client disconnects only end delivery, not the run.
	raw, _ := json.Marshal(response{JSONRPC: "2.0", ID: id, Result: encodedResult, Error: e})
	_, _ = w.Write(append(raw, '\n'))
}

var versionPattern = regexp.MustCompile(`^1\.0(?:\.[0-9]+)?$`)

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", 405)
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		http.Error(w, "application/json required", 415)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "request too large", 413)
		} else {
			http.Error(w, "cannot read request", 400)
		}
		return
	}
	var req request
	if !json.Valid(raw) {
		// A valid first value followed by trailing bytes is an invalid envelope.
		dec := json.NewDecoder(bytes.NewReader(raw))
		var first, second json.RawMessage
		if dec.Decode(&first) == nil && dec.Decode(&second) != io.EOF {
			writeResponse(w, nil, nil, &rpcError{Code: -32600, Message: "Invalid Request"})
		} else {
			writeResponse(w, nil, nil, &rpcError{Code: -32700, Message: "Parse error"})
		}
		return
	}
	if json.Unmarshal(raw, &req) != nil || req.JSONRPC != "2.0" || req.Method == "" || !validID(req.ID) || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		writeResponse(w, nil, nil, &rpcError{Code: -32600, Message: "Invalid Request"})
		return
	}
	if !versionPattern.MatchString(r.Header.Get("A2A-Version")) {
		writeResponse(w, req.ID, nil, protocolError(-32009, "unsupported_version", nil))
		return
	}
	if len(req.Params) == 0 || bytes.Equal(req.Params, []byte("null")) || bytes.TrimSpace(req.Params)[0] != '{' {
		writeResponse(w, req.ID, nil, invalid())
		return
	}
	var common struct {
		Tenant string `json:"tenant"`
	}
	if json.Unmarshal(req.Params, &common) != nil {
		writeResponse(w, req.ID, nil, invalid())
		return
	}
	if common.Tenant != "" {
		writeResponse(w, req.ID, nil, protocolError(-32004, "tenant", nil))
		return
	}
	switch req.Method {
	case "SendMessage", "SendStreamingMessage":
		p, prompt, e := decodeSend(req.Params)
		if e != nil {
			writeResponse(w, req.ID, nil, e)
			return
		}
		t, e := s.send(r.Context(), p, prompt)
		if e != nil {
			writeResponse(w, req.ID, nil, e)
			return
		}
		history := (*int)(nil)
		if p.Config != nil {
			history = p.Config.HistoryLength
		}
		if req.Method == "SendStreamingMessage" {
			s.stream(w, r, req.ID, t, history, false)
			return
		}
		if p.Config == nil || !p.Config.ReturnImmediately {
			if err := t.waitInterrupted(r.Context()); err != nil {
				return
			}
		}
		writeResponse(w, req.ID, struct {
			Task *model.Task `json:"task"`
		}{t.snapshot(history, true)}, nil)
	case "GetTask", "CancelTask", "SubscribeToTask":
		var p struct {
			ID            model.TaskID `json:"id"`
			HistoryLength *int         `json:"historyLength"`
		}
		if json.Unmarshal(req.Params, &p) != nil || p.ID == "" || !validHistory(p.HistoryLength) {
			writeResponse(w, req.ID, nil, invalid())
			return
		}
		t := s.lookup(p.ID)
		if t == nil {
			writeResponse(w, req.ID, nil, protocolError(-32001, "", nil))
			return
		}
		if req.Method == "SubscribeToTask" {
			s.stream(w, r, req.ID, t, nil, true)
			return
		}
		if req.Method == "CancelTask" {
			t.mu.Lock()
			terminal := t.value.Status.State.Terminal()
			run := t.run
			t.mu.Unlock()
			if terminal {
				writeResponse(w, req.ID, nil, protocolError(-32002, "", nil))
				return
			}
			run.Cancel()
			select {
			case <-t.done:
			case <-r.Context().Done():
				return
			}
			task := t.snapshot(p.HistoryLength, true)
			if task.Status.State != model.TaskStateCanceled {
				writeResponse(w, req.ID, nil, protocolError(-32002, "", nil))
				return
			}
		}
		writeResponse(w, req.ID, t.snapshot(p.HistoryLength, true), nil)
	case "ListTasks":
		result, e := s.list(req.Params)
		writeResponse(w, req.ID, result, e)
	case "CreateTaskPushNotificationConfig", "GetTaskPushNotificationConfig", "ListTaskPushNotificationConfigs", "DeleteTaskPushNotificationConfig":
		writeResponse(w, req.ID, nil, protocolError(-32003, "", nil))
	case "GetExtendedAgentCard":
		writeResponse(w, req.ID, nil, protocolError(-32004, "extended_card", nil))
	default:
		writeResponse(w, req.ID, nil, &rpcError{Code: -32601, Message: "Method not found"})
	}
}
func validID(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false
	}
	if raw[0] == '"' {
		var v string
		return json.Unmarshal(raw, &v) == nil
	}
	var n json.Number
	return json.Unmarshal(raw, &n) == nil && raw[0] != '{' && raw[0] != '[' && raw[0] != 't' && raw[0] != 'f'
}
func validHistory(n *int) bool { return n == nil || *n >= 0 }
func decodeSend(raw json.RawMessage) (*model.SendMessageRequest, string, *rpcError) {
	// Inspect union presence before SDK decoding: null and empty URL are still
	// well-formed unsupported content, never approval instructions.
	var envelope struct {
		Message *struct {
			Parts []json.RawMessage `json:"parts"`
		} `json:"message"`
		Configuration map[string]json.RawMessage `json:"configuration"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Message == nil {
		return nil, "", invalid()
	}
	if _, ok := envelope.Configuration["taskPushNotificationConfig"]; ok {
		return nil, "", protocolError(-32003, "", nil)
	}
	for _, part := range envelope.Message.Parts {
		var fields map[string]json.RawMessage
		if json.Unmarshal(part, &fields) != nil || fields == nil {
			return nil, "", invalid()
		}
		n := 0
		kind := ""
		for _, key := range []string{"text", "raw", "url", "data"} {
			if _, ok := fields[key]; ok {
				n++
				kind = key
			}
		}
		if n != 1 {
			return nil, "", invalid()
		}
		if kind == "raw" {
			var value []byte
			if bytes.Equal(fields[kind], []byte("null")) || json.Unmarshal(fields[kind], &value) != nil {
				return nil, "", invalid()
			}
		}
		if kind == "url" {
			var value string
			if bytes.Equal(fields[kind], []byte("null")) || json.Unmarshal(fields[kind], &value) != nil {
				return nil, "", invalid()
			}
		}
		if kind != "text" {
			return nil, "", protocolError(-32005, "input_content", nil)
		}
	}
	var p model.SendMessageRequest
	if json.Unmarshal(raw, &p) != nil || p.Message == nil || p.Message.ID == "" || p.Message.Role != model.MessageRoleUser || len(p.Message.Parts) == 0 {
		return nil, "", invalid()
	}
	if p.Config != nil {
		if !validHistory(p.Config.HistoryLength) {
			return nil, "", invalid()
		}
		if len(p.Config.AcceptedOutputModes) > 0 {
			ok := false
			for _, mode := range p.Config.AcceptedOutputModes {
				ok = ok || mode == "text/plain"
			}
			if !ok {
				return nil, "", protocolError(-32005, "output_content", nil)
			}
		}
	}
	var parts []string
	for _, part := range p.Message.Parts {
		if part == nil {
			return nil, "", invalid()
		}
		if part.MediaType != "" && part.MediaType != "text/plain" {
			return nil, "", protocolError(-32005, "input_media", nil)
		}
		parts = append(parts, part.Text())
	}
	prompt := strings.Join(parts, "\n\n")
	if strings.TrimSpace(prompt) == "" {
		return nil, "", invalid()
	}
	return &p, prompt, nil
}
func (s *Server) lookup(id model.TaskID) *task {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	return s.tasks[id]
}
func (s *Server) expireLocked() {
	now := s.now()
	for id, t := range s.tasks {
		t.mu.Lock()
		expired := t.value.Status.State.Terminal() && now.Sub(*t.value.Status.Timestamp) >= s.retention
		t.mu.Unlock()
		if expired {
			delete(s.tasks, id)
		}
	}
}
func (s *Server) send(ctx context.Context, p *model.SendMessageRequest, prompt string) (*task, *rpcError) {
	if p.Message.TaskID != "" {
		t := s.lookup(p.Message.TaskID)
		if t == nil {
			return nil, protocolError(-32001, "", nil)
		}
		v := t.snapshot(nil, false)
		if p.Message.ContextID != "" && p.Message.ContextID != v.ContextID {
			return nil, invalid()
		}
		reason := "task_busy"
		if v.Status.State == model.TaskStateInputRequired {
			reason = "permission_via_acp"
		}
		return nil, protocolError(-32004, reason, map[string]string{"sessionId": v.ContextID, "taskId": string(v.ID)})
	}
	s.mu.Lock()
	s.expireLocked()
	if s.closed {
		s.mu.Unlock()
		return nil, protocolError(-32004, "shutdown", nil)
	}
	if len(s.tasks)+s.pending >= s.capacity {
		s.mu.Unlock()
		return nil, protocolError(-32004, "capacity", nil)
	}
	s.pending++
	s.observers.Add(1)
	s.mu.Unlock()
	retained := false
	defer func() {
		if !retained {
			s.mu.Lock()
			s.pending--
			s.mu.Unlock()
			s.observers.Done()
		}
	}()
	var session *tempest.Session
	var release func()
	var err error
	if p.Message.ContextID == "" {
		defaults := s.backend.Defaults()
		session, err = s.backend.New(ctx, s.opts.Cwd, &defaults)
	} else {
		session, release, err = s.backend.Acquire(p.Message.ContextID)
		if errors.Is(err, journal.ErrSessionNotFound) {
			session, err = s.backend.Resume(ctx, s.opts.Cwd, p.Message.ContextID, nil)
		}
	}
	if err != nil {
		return nil, admissionError(err)
	}
	if release == nil {
		session, release, err = s.backend.Acquire(session.ID())
		if err != nil {
			return nil, admissionError(err)
		}
	}
	snap, updates, stop := session.Watch(s.backend.RunContext(), tempest.WatchOptions{Buffer: taskQueueSize})
	run, err := session.Begin(s.backend.RunContext(), tempest.Prompt{Text: prompt})
	if err != nil {
		stop()
		release()
		return nil, admissionError(err)
	}
	// Once Begin accepts, connection cancellation only stops waiting. Started
	// and the observer belong to the server, including pre-start failures.
	id, err := run.Started(context.Background())
	if err != nil {
		stop()
		release()
		return nil, admissionError(err)
	}
	now := s.now().UTC()
	msg := &model.Message{ID: p.Message.ID, ContextID: session.ID(), TaskID: model.TaskID(id), Role: model.MessageRoleUser, Parts: model.ContentParts{model.NewTextPart(prompt)}}
	t := &task{
		run: run, session: session, done: make(chan struct{}), changed: make(chan struct{}),
		subscribers: make(map[*subscriber]struct{}),
		value:       model.Task{ID: model.TaskID(id), ContextID: session.ID(), Status: model.TaskStatus{State: model.TaskStateWorking, Timestamp: &now}, History: []*model.Message{msg}},
	}
	t.publishMetadataLocked()
	s.mu.Lock()
	s.pending--
	s.serial++
	t.serial = s.serial
	s.tasks[t.value.ID] = t
	s.mu.Unlock()
	retained = true
	go func() {
		defer s.observers.Done()
		defer stop()
		defer release()
		t.observe(snap, updates, stop, s.backend.RunContext())
	}()
	return t, nil
}
func admissionError(err error) *rpcError {
	if errors.Is(err, journal.ErrSessionNotFound) {
		return invalid()
	}
	if errors.Is(err, tempest.ErrRunActive) || errors.Is(err, tempest.ErrSessionLocked) || errors.Is(err, tempest.ErrMCPConfigConflict) || errors.Is(err, tempest.ErrClosed) {
		return protocolError(-32004, "session_unavailable", nil)
	}
	return &rpcError{Code: -32603, Message: "Session setup failed"}
}
