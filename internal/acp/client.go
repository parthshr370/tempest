package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/build"
	"go.harness.dev/harness/internal/hub"
	"go.harness.dev/harness/internal/mcpclient"
	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest"
	"go.harness.dev/harness/tempest/durable"
)

// SessionInfo is a journal listing entry.
type SessionInfo struct {
	SessionID string              `json:"sessionId"`
	Cwd       string              `json:"cwd"`
	Title     string              `json:"title"`
	UpdatedAt time.Time           `json:"updatedAt"`
	LastRun   *journal.RunSummary `json:"lastRun,omitempty"`
}

// Backend keeps registry ownership separate from per-connection protocol state.
type Backend interface {
	New(context.Context, string, *tempest.NewSession) (*tempest.Session, error)
	Resume(context.Context, string, string, []tempest.MCPServerConfig) (*tempest.Session, error)
	Acquire(string) (*tempest.Session, func(), error)
	CloseSession(context.Context, string) error
	List(context.Context, string, string) ([]SessionInfo, string, error)
	RunContext() context.Context
	Defaults() tempest.NewSession
}

type watch struct {
	stop    func()
	mu      sync.Mutex
	cond    *sync.Cond
	seq     uint64
	stopped bool
}

// Client holds initialization and subscriptions for exactly one connection.
type Client struct {
	conn        *rpc.Conn
	backend     Backend
	broker      *Broker
	mu          sync.Mutex
	watchMu     sync.Mutex
	initialized bool
	notices     bool
	watches     map[string]*watch
	closed      bool
}

// NewClient binds a connection to the process-wide registry and permission broker.
func NewClient(c *rpc.Conn, backend Backend, broker *Broker) *Client {
	return &Client{conn: c, backend: backend, broker: broker, watches: make(map[string]*watch)}
}

// Serve disconnects subscriptions without cancelling session-owned runs.
func (c *Client) Serve() error { defer c.Close(); return c.conn.ServeOrdered(c.admit) }

// Close removes this connection's watchers.
func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	for id, w := range c.watches {
		w.stop()
		delete(c.watches, id)
	}
	c.mu.Unlock()
}

func decode(raw json.RawMessage, v any) *rpc.Error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, v) != nil {
		return rpc.NewError(-32602, "invalid_params", "invalid parameters")
	}
	return nil
}
func failure(err error) *rpc.Error {
	var wire *rpc.Error
	var startup *mcpclient.StartupError
	if errors.As(err, &startup) {
		return rpc.NewError(-32013, "mcp_startup", fmt.Sprintf("MCP server %s %s failed", startup.Server, startup.Phase))
	}
	if errors.As(err, &wire) {
		return wire
	}
	switch {
	case errors.Is(err, tempest.ErrQueueConflict):
		return rpc.NewError(-32015, "queue_conflict", err.Error())
	case errors.Is(err, durable.ErrConflict):
		return rpc.NewError(-32024, "durable_conflict", err.Error())
	case errors.Is(err, tempest.ErrHostLimit):
		return rpc.NewError(-32025, "host_limit", err.Error())
	case errors.Is(err, tempest.ErrQueueFull):
		return rpc.NewError(-32014, "queue_full", err.Error())
	case errors.Is(err, tempest.ErrQueueNotFound):
		return rpc.NewError(-32016, "queue_not_found", err.Error())
	case errors.Is(err, tempest.ErrQueueTooLate):
		return rpc.NewError(-32017, "queue_too_late", err.Error())
	case errors.Is(err, tempest.ErrJobNotFound):
		return rpc.NewError(-32018, "job_not_found", err.Error())
	case errors.Is(err, tempest.ErrJobLimit):
		return rpc.NewError(-32019, "job_limit", err.Error())
	case errors.Is(err, tempest.ErrJobNotCleanupable):
		return rpc.NewError(-32020, "job_not_cleanupable", err.Error())
	case errors.Is(err, tempest.ErrRecoveryRequired):
		return rpc.NewError(-32021, "recovery_required", err.Error())
	case errors.Is(err, tempest.ErrInvalidQueueInput), errors.Is(err, tempest.ErrInvalidJobInput), errors.Is(err, tempest.ErrInvalidOutputSchema):
		return rpc.NewError(-32602, "invalid_params", err.Error())
	case errors.Is(err, tempest.ErrMCPConfigConflict):
		return rpc.NewError(-32012, "mcp_config_conflict", err.Error())
	case errors.Is(err, agent.ErrUnknownPrice):
		return rpc.NewError(-32005, "budget_invalid", err.Error())
	case errors.Is(err, tempest.ErrRunActive):
		return rpc.NewError(-32004, "run_active", err.Error())
	case errors.Is(err, tempest.ErrSessionLocked):
		return rpc.NewError(-32003, "session_locked", err.Error())
	case errors.Is(err, tempest.ErrStaleEpoch):
		return rpc.NewError(-32010, "stale_epoch", err.Error())
	case errors.Is(err, tempest.ErrNoPendingAsk):
		return rpc.NewError(-32011, "no_pending_ask", err.Error())
	case errors.Is(err, os.ErrNotExist), errors.Is(err, tempest.ErrClosed):
		return rpc.NewError(-32001, "session_not_found", err.Error())
	default:
		return rpc.NewError(-32603, "internal_error", err.Error())
	}
}

// admit orders durable queue admission with run registration and cancellation.
// Provider, wait, log and git work remain deferred.
func (c *Client) admit(ctx context.Context, method string, raw json.RawMessage) (any, *rpc.Error) {
	switch method {
	case "initialize", "session/prompt", "tempest/continue", "session/cancel", "tempest/steer", "tempest/follow_up", "tempest/cancel_queued", "_tempest/durable/submit", "_tempest/durable/withdraw", "_tempest/durable/commit":
		return c.Handle(ctx, method, raw)
	default:
		return rpc.Deferred(func() (any, *rpc.Error) { return c.Handle(ctx, method, raw) }), nil
	}
}

// Handle implements the ACP v1 method table and Tempest extensions.
func (c *Client) Handle(ctx context.Context, method string, raw json.RawMessage) (any, *rpc.Error) {
	if method == "initialize" {
		var p struct {
			ProtocolVersion    int `json:"protocolVersion"`
			ClientCapabilities struct {
				Session struct {
					Notices bool `json:"notices"`
				} `json:"session"`
			} `json:"clientCapabilities"`
		}
		if e := decode(raw, &p); e != nil {
			return nil, e
		}
		if p.ProtocolVersion < 1 {
			return nil, rpc.NewError(-32602, "invalid_params", "protocolVersion must be at least 1")
		}
		c.mu.Lock()
		c.initialized = true
		c.notices = p.ClientCapabilities.Session.Notices
		c.mu.Unlock()
		return map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"_meta": map[string]any{"tempest": map[string]any{"durable": map[string]any{"version": 1, "methods": []string{"_tempest/durable/submit", "_tempest/durable/get_submission", "_tempest/durable/withdraw", "_tempest/durable/commit", "_tempest/durable/read"}, "notifications": []string{"_tempest/durable/update"}}}}, "loadSession": true, "mcpCapabilities": map[string]any{"http": true, "sse": false}, "sessionCapabilities": map[string]any{"list": map[string]any{}, "resume": map[string]any{}, "close": map[string]any{}}, "promptCapabilities": map[string]any{"image": true, "embeddedContext": true}}, "agentInfo": map[string]any{"name": "tempest", "version": build.Current().Version}}, nil
	}
	c.mu.Lock()
	initialized := c.initialized
	c.mu.Unlock()
	if !initialized {
		return nil, rpc.NewError(-32002, "not_initialized", "initialize must come first")
	}
	switch method {
	case "_tempest/durable/submit", "_tempest/durable/get_submission", "_tempest/durable/withdraw", "_tempest/durable/commit", "_tempest/durable/read":
		return c.handleDurable(ctx, method, raw)
	case "tempest/steer", "tempest/follow_up", "tempest/cancel_queued", "tempest/jobs", "tempest/wait", "tempest/cancel_job", "tempest/logs", "tempest/cleanup_job":
		return c.handleWork(ctx, method, raw)
	}
	switch method {
	case "session/new", "session/load", "session/resume":
		return c.open(ctx, method, raw)
	case "session/list":
		var p struct{ Cwd, Cursor string }
		if e := decode(raw, &p); e != nil {
			return nil, e
		}
		list, next, err := c.backend.List(ctx, p.Cwd, p.Cursor)
		if err != nil {
			return nil, failure(err)
		}
		result := map[string]any{"sessions": list}
		if next != "" {
			result["nextCursor"] = next
		}
		return result, nil
	}
	switch method {
	case "session/prompt", "session/cancel", "session/close", "tempest/watch", "tempest/unwatch", "tempest/get_state", "tempest/history", "tempest/continue", "tempest/permission", "tempest/set_budget", "tempest/set_permission_mode", "tempest/checkpoints", "tempest/checkpoint", "tempest/rewind":
	default:
		return nil, rpc.NewError(-32601, "method_not_found", "unknown method: "+method)
	}
	var p struct {
		SessionID            string         `json:"sessionId"`
		Prompt               []contentBlock `json:"prompt"`
		Approver             bool           `json:"approver"`
		Buffer               int            `json:"buffer"`
		HistoryTail          *int           `json:"historyTail"`
		Epoch, Offset, Limit int
		ToolCallID           string   `json:"toolCallId"`
		OptionID             string   `json:"optionId"`
		MaxTurns             *int     `json:"maxTurns"`
		MaxCostUSD           *float64 `json:"maxCostUsd"`
		MaxWallSeconds       *float64 `json:"maxWallSeconds"`
		Mode                 string   `json:"mode"`
		Checkpoint           string   `json:"checkpoint"`
		Goal                 string   `json:"goal"`
		Report               string   `json:"report"`
		Force                bool     `json:"force"`
		Meta                 struct {
			Tempest struct {
				// OutputSchema is the session/prompt run's final-answer
				// JSON Schema; the response's _meta.tempest.output carries
				// the validated answer.
				OutputSchema json.RawMessage `json:"outputSchema"`
				RequestID    string          `json:"requestId"`
			} `json:"tempest"`
		} `json:"_meta"`
	}
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	if p.SessionID == "" {
		return nil, rpc.NewError(-32602, "invalid_params", "sessionId is required")
	}
	if method == "session/close" {
		if err := c.backend.CloseSession(ctx, p.SessionID); err != nil {
			return nil, failure(err)
		}
		c.unwatch(p.SessionID)
		return struct{}{}, nil
	}
	s, release, err := c.backend.Acquire(p.SessionID)
	if err != nil {
		return nil, failure(err)
	}
	retainLease := false
	defer func() {
		if !retainLease {
			release()
		}
	}()
	switch method {
	case "tempest/checkpoints":
		return struct {
			Checkpoints []tempest.Checkpoint `json:"checkpoints"`
		}{s.Checkpoints()}, nil
	case "tempest/checkpoint":
		info, err := s.CreateCheckpoint(ctx, p.Goal)
		if err != nil {
			return nil, failure(err)
		}
		return info, nil
	case "tempest/rewind":
		if err := s.Rewind(ctx, tempest.RewindOptions{Checkpoint: p.Checkpoint, Report: p.Report, Force: p.Force}); err != nil {
			return nil, failure(err)
		}
		return struct{}{}, nil
	case "session/prompt", "tempest/continue":
		var prompt tempest.Prompt
		if method == "session/prompt" {
			var e *rpc.Error
			prompt, e = convertPrompt(p.Prompt)
			if e != nil {
				return nil, e
			}
			prompt.OutputSchema = p.Meta.Tempest.OutputSchema
		}
		if method == "session/prompt" && p.Meta.Tempest.RequestID != "" {
			remove := c.broker.add(s.ID(), c.conn)
			submission, err := s.Submit(ctx, tempest.Input{RequestID: p.Meta.Tempest.RequestID, Prompt: prompt, Placement: tempest.PlacementInput})
			if err != nil {
				remove()
				return nil, failure(err)
			}
			if submission.State == "accepted" {
				if err := s.ResumePending(ctx); err != nil {
					remove()
					return nil, failure(err)
				}
			}
			retainLease = true
			return rpc.Deferred(func() (any, *rpc.Error) {
				defer release()
				defer remove()
				settled, err := s.WaitSubmission(ctx, submission.RequestID)
				if err != nil {
					return nil, failure(err)
				}
				c.flushWatch(ctx, s.ID(), s.Snapshot().Seq)
				return promptCompletion(settled.Result, nil)
			}), nil
		}
		remove := c.broker.add(s.ID(), c.conn)
		var run *tempest.Run
		if method == "session/prompt" {
			run, err = s.Begin(c.backend.RunContext(), prompt)
		} else {
			run, err = s.BeginContinue(c.backend.RunContext())
		}
		if err != nil {
			remove()
			return nil, failure(err)
		}
		retainLease = true
		return rpc.Deferred(func() (any, *rpc.Error) {
			defer release()
			defer remove()
			ownerDone := make(chan struct{})
			defer close(ownerDone)
			go func() {
				select {
				case <-c.conn.Done():
					remove()
				case <-ownerDone:
				}
			}()
			result, err := run.Wait()
			if result.RunID != "" {
				c.flushWatch(ctx, s.ID(), s.Snapshot().Seq)
			}
			return promptCompletion(result, err)
		}), nil
	case "session/cancel":
		s.Cancel()
		return struct{}{}, nil
	case "tempest/watch":
		if p.Buffer < 0 || p.Buffer > 65536 || (p.HistoryTail != nil && (*p.HistoryTail < 0 || *p.HistoryTail > 10000)) {
			return nil, rpc.NewError(-32602, "invalid_params", "invalid watch bounds")
		}
		snap, after, e := c.startWatch(ctx, s, p.Approver, p.Buffer, p.HistoryTail, false)
		if e != nil {
			return nil, e
		}
		return rpc.Response{Value: map[string]any{"snapshot": snap}, After: after}, nil
	case "tempest/unwatch":
		c.unwatch(s.ID())
		return struct{}{}, nil
	case "tempest/get_state":
		return map[string]any{"snapshot": s.Snapshot()}, nil
	case "tempest/history":
		if p.Offset < 0 || p.Limit < 0 || p.Limit > 10000 {
			return nil, rpc.NewError(-32602, "invalid_params", "invalid history bounds")
		}
		updates, err := s.History(p.Epoch, p.Offset, p.Limit)
		if err != nil {
			return nil, failure(err)
		}
		return map[string]any{"updates": updates, "epoch": p.Epoch, "total": s.Snapshot().HistoryLen}, nil
	case "tempest/permission":
		if !c.broker.authorized(s.ID(), c.conn) {
			return nil, rpc.NewError(-32006, "unauthorized", "connection is not an approver")
		}
		if !validChoice(p.OptionID) {
			return nil, rpc.NewError(-32602, "invalid_params", "invalid optionId")
		}
		if err := s.Answer(p.ToolCallID, tempest.Choice(p.OptionID)); err != nil {
			return nil, failure(err)
		}
		return struct{}{}, nil
	case "tempest/set_budget":
		budget := s.Snapshot().Budget
		if p.MaxTurns != nil {
			budget.MaxTurns = *p.MaxTurns
		}
		if p.MaxCostUSD != nil {
			budget.MaxCostUSD = *p.MaxCostUSD
		}
		if p.MaxWallSeconds != nil {
			seconds := *p.MaxWallSeconds
			if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > float64(math.MaxInt64)/float64(time.Second) {
				return nil, rpc.NewError(-32005, "budget_invalid", "invalid maxWallSeconds")
			}
			budget.MaxWallTime = time.Duration(seconds * float64(time.Second))
		}
		if err := s.SetBudget(budget); err != nil {
			return nil, rpc.NewError(-32005, "budget_invalid", err.Error())
		}
		return map[string]any{"budget": hub.NewWireBudget(budget)}, nil
	case "tempest/set_permission_mode":
		mode, err := permission.ParseMode(p.Mode)
		if err != nil {
			return nil, rpc.NewError(-32602, "invalid_params", err.Error())
		}
		if err = s.SetPermissionMode(mode); err != nil {
			return nil, failure(err)
		}
		return map[string]any{"mode": mode}, nil
	}
	panic("unhandled method")
}

type acpMCPServers []json.RawMessage

func (servers *acpMCPServers) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("mcpServers must be an array")
	}
	type wireList acpMCPServers
	return json.Unmarshal(raw, (*wireList)(servers))
}

type openParams struct {
	SessionID  string        `json:"sessionId"`
	Cwd        string        `json:"cwd"`
	MCPServers acpMCPServers `json:"mcpServers"`
	Meta       struct {
		Tempest struct {
			Model          string `json:"model"`
			PermissionMode string `json:"permissionMode"`
			Budget         *struct {
				MaxTurns       int     `json:"maxTurns"`
				MaxCostUSD     float64 `json:"maxCostUsd"`
				MaxWallSeconds float64 `json:"maxWallSeconds"`
			} `json:"budget"`
		} `json:"tempest"`
	} `json:"_meta"`
}

func (c *Client) open(ctx context.Context, method string, raw json.RawMessage) (any, *rpc.Error) {
	var p openParams
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	servers, conversionErr := convertMCPServers(p.MCPServers)
	if conversionErr != nil {
		return nil, conversionErr
	}
	if p.Cwd == "" || !filepath.IsAbs(p.Cwd) {
		return nil, rpc.NewError(-32602, "invalid_params", "cwd must be absolute")
	}
	if e := checkWorkspace(p.Cwd); e != nil {
		return nil, e
	}
	var s *tempest.Session
	var err error
	if method == "session/new" {
		defaults := c.backend.Defaults()
		n := &defaults
		if p.MCPServers != nil {
			n.MCPServers = servers
		}
		if p.Meta.Tempest.Model != "" {
			n.Model = p.Meta.Tempest.Model
		}
		if p.Meta.Tempest.PermissionMode != "" {
			mode, e := permission.ParseMode(p.Meta.Tempest.PermissionMode)
			if e != nil {
				return nil, rpc.NewError(-32602, "invalid_params", e.Error())
			}
			n.PermissionMode = mode
		}
		if b := p.Meta.Tempest.Budget; b != nil {
			if b.MaxTurns < 0 || b.MaxCostUSD < 0 || b.MaxWallSeconds < 0 || b.MaxWallSeconds > float64(math.MaxInt64)/float64(time.Second) {
				return nil, rpc.NewError(-32005, "budget_invalid", "invalid budget")
			}
			n.Budget = tempest.Budget{MaxTurns: b.MaxTurns, MaxCostUSD: b.MaxCostUSD, MaxWallTime: time.Duration(b.MaxWallSeconds * float64(time.Second))}
		}
		s, err = c.backend.New(ctx, p.Cwd, n)
	} else {
		if p.SessionID == "" {
			return nil, rpc.NewError(-32602, "invalid_params", "sessionId is required")
		}
		s, err = c.backend.Resume(ctx, p.Cwd, p.SessionID, servers)
	}
	if err != nil {
		return nil, failure(err)
	}
	if method == "session/new" {
		return map[string]any{"sessionId": s.ID()}, nil
	}
	if method == "session/resume" {
		return struct{}{}, nil
	}
	_, after, e := c.startWatch(ctx, s, false, 256, nil, true)
	if e != nil {
		return nil, e
	}
	// Replay precedes the response; live updates stay queued until after it.
	return rpc.Response{Value: struct{}{}, After: after}, nil
}

// checkWorkspace rejects a cwd that a session could not run tools in.
func checkWorkspace(cwd string) *rpc.Error {
	info, err := os.Stat(cwd)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return rpc.NewError(-32602, "invalid_params", fmt.Sprintf("cwd %s does not exist", cwd))
	case err != nil:
		return rpc.NewError(-32602, "invalid_params", fmt.Sprintf("cwd is not accessible: %v", err))
	case !info.IsDir():
		return rpc.NewError(-32602, "invalid_params", fmt.Sprintf("cwd %s is not a directory", cwd))
	}
	return nil
}

func convertMCPServers(raw []json.RawMessage) ([]tempest.MCPServerConfig, *rpc.Error) {
	if raw == nil {
		return nil, nil
	}
	out := make([]tempest.MCPServerConfig, 0, len(raw))
	for _, item := range raw {
		var tag struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(item, &tag) != nil {
			return nil, rpc.NewError(-32602, "invalid_params", "invalid MCP server")
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(item, &fields) != nil {
			return nil, rpc.NewError(-32602, "invalid_params", "invalid MCP server")
		}
		if _, present := fields["type"]; present && tag.Type == "" {
			return nil, rpc.NewError(-32602, "invalid_params", "ACP stdio omits the type discriminator")
		}
		if tag.Type == "sse" {
			return nil, rpc.NewError(-32601, "unsupported", "SSE MCP transport is unsupported")
		}
		if tag.Type != "" && tag.Type != "http" {
			return nil, rpc.NewError(-32602, "invalid_params", "invalid ACP MCP discriminator")
		}
		type pair struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		var native struct {
			Type    string   `json:"type,omitempty"`
			Name    string   `json:"name"`
			Command string   `json:"command,omitempty"`
			Args    []string `json:"args,omitempty"`
			Env     []pair   `json:"env,omitempty"`
			URL     string   `json:"url,omitempty"`
			Headers []pair   `json:"headers,omitempty"`
		}
		d := json.NewDecoder(bytes.NewReader(item))
		d.DisallowUnknownFields()
		if d.Decode(&native) != nil {
			return nil, rpc.NewError(-32602, "invalid_params", "invalid ACP MCP fields")
		}
		var extra json.RawMessage
		if d.Decode(&extra) != io.EOF {
			return nil, rpc.NewError(-32602, "invalid_params", "trailing MCP data")
		}
		s := tempest.MCPServerConfig{Name: native.Name, Command: native.Command, Args: native.Args, URL: native.URL, Permission: "prompt", Transport: "stdio"}
		if native.Type == "http" {
			s.Transport = "http"
		}
		if (s.Transport == "stdio" && (native.URL != "" || native.Headers != nil)) || (s.Transport == "http" && (native.Command != "" || native.Args != nil || native.Env != nil)) {
			return nil, rpc.NewError(-32602, "invalid_params", "mixed ACP MCP variants")
		}
		s.Env = make(map[string]string)
		s.Headers = make(map[string]string)
		for _, group := range []struct {
			pairs  []pair
			target map[string]string
			fold   bool
		}{{native.Env, s.Env, false}, {native.Headers, s.Headers, true}} {
			seen := map[string]bool{}
			for _, p := range group.pairs {
				key := p.Name
				if group.fold {
					key = strings.ToLower(key)
				}
				if seen[key] {
					return nil, rpc.NewError(-32602, "invalid_params", "duplicate MCP environment/header key")
				}
				seen[key] = true
				group.target[p.Name] = p.Value
			}
		}
		out = append(out, s)
	}
	normalized, err := mcpconfig.Normalize(out)
	if err != nil {
		return nil, rpc.NewError(-32602, "invalid_params", err.Error())
	}
	return normalized, nil
}

type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
	Resource *struct {
		URI  string  `json:"uri"`
		Text *string `json:"text"`
	} `json:"resource"`
}

func convertPrompt(blocks []contentBlock) (tempest.Prompt, *rpc.Error) {
	var p tempest.Prompt
	var text []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text = append(text, b.Text)
		case "image":
			if b.Data == "" || !strings.HasPrefix(b.MimeType, "image/") {
				return p, rpc.NewError(-32602, "invalid_params", "image requires data and an image mimeType")
			}
			p.Media = append(p.Media, tempest.Media{Data: b.Data, MimeType: b.MimeType})
		case "resource":
			if b.Resource == nil || b.Resource.Text == nil {
				return p, rpc.NewError(-32602, "invalid_params", "resource must contain text")
			}
			text = append(text, fmt.Sprintf("```%s\n%s\n```", b.Resource.URI, *b.Resource.Text))
		default:
			return p, rpc.NewError(-32602, "invalid_params", "unsupported content block: "+b.Type)
		}
	}
	p.Text = strings.Join(text, "\n\n")
	raw, _ := json.Marshal(blocks)
	digest := sha256.Sum256(raw)
	p.ContentIdentity, _ = json.Marshal(fmt.Sprintf("sha256:%x", digest))
	return p, nil
}
