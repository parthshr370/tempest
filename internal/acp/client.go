package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/build"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest"
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
	Resume(context.Context, string, string) (*tempest.Session, error)
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
func (c *Client) Serve() error { defer c.Close(); return c.conn.Serve(c.Handle) }

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
	if errors.As(err, &wire) {
		return wire
	}
	switch {
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
		return map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true, "sessionCapabilities": map[string]any{"list": map[string]any{}, "resume": map[string]any{}, "close": map[string]any{}}, "promptCapabilities": map[string]any{"image": true, "embeddedContext": true}}, "agentInfo": map[string]any{"name": "tempest", "version": build.Current().Version}}, nil
	}
	c.mu.Lock()
	initialized := c.initialized
	c.mu.Unlock()
	if !initialized {
		return nil, rpc.NewError(-32002, "not_initialized", "initialize must come first")
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
	case "session/prompt", "session/cancel", "session/close", "tempest/watch", "tempest/unwatch", "tempest/get_state", "tempest/history", "tempest/continue", "tempest/permission", "tempest/set_budget", "tempest/set_permission_mode":
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
	defer release()
	switch method {
	case "session/prompt", "tempest/continue":
		var prompt tempest.Prompt
		if method == "session/prompt" {
			var e *rpc.Error
			prompt, e = convertPrompt(p.Prompt)
			if e != nil {
				return nil, e
			}
		}
		remove := c.broker.add(s.ID(), c.conn)
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
		var result tempest.RunResult
		if method == "session/prompt" {
			result, err = s.Prompt(c.backend.RunContext(), prompt)
		} else {
			result, err = s.Continue(c.backend.RunContext())
		}
		if result.RunID != "" {
			c.flushWatch(ctx, s.ID(), s.Snapshot().Seq)
		}
		if result.StopReason == "error" {
			message := "run failed"
			if result.Err != nil {
				message = result.Err.Error()
			}
			return nil, rpc.NewError(-32009, "run_error", message)
		}
		if err != nil {
			return nil, failure(err)
		}
		reason := "end_turn"
		switch result.StopReason {
		case "aborted":
			reason = "cancelled"
		case "max_turns", "max_cost", "max_wall_time":
			reason = "max_turn_requests"
		}
		var cost any
		if !result.Usage.Cost.Unknown {
			cost = result.Usage.Cost.Total
		}
		return map[string]any{"stopReason": reason, "_meta": map[string]any{"tempest": map[string]any{"stopReason": result.StopReason, "runId": result.RunID, "turns": result.Turns, "usage": result.Usage, "costUsd": cost, "elapsedMs": result.ElapsedMS}}}, nil
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
		return map[string]any{"budget": budget}, nil
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

type openParams struct {
	SessionID  string            `json:"sessionId"`
	Cwd        string            `json:"cwd"`
	MCPServers []json.RawMessage `json:"mcpServers"`
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
	if len(p.MCPServers) > 0 {
		return nil, rpc.NewError(-32601, "unsupported", "unsupported: mcpServers")
	}
	if p.Cwd == "" || !filepath.IsAbs(p.Cwd) {
		return nil, rpc.NewError(-32602, "invalid_params", "cwd must be absolute")
	}
	var s *tempest.Session
	var err error
	if method == "session/new" {
		defaults := c.backend.Defaults()
		n := &defaults
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
		s, err = c.backend.Resume(ctx, p.Cwd, p.SessionID)
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
	return p, nil
}
