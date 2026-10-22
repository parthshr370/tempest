// Package acp maps ACP and Tempest methods onto durable sessions.
package acp

import (
	"context"
	"sync"
	"time"

	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest"
)

// Timer lets tests expire permission asks without sleeping.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// Clock creates permission timers.
type Clock interface{ NewTimer(time.Duration) Timer }
type realClock struct{}
type realTimer struct{ *time.Timer }

func (realClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }
func (t realTimer) C() <-chan time.Time          { return t.Timer.C }

type pendingAsk struct {
	ask    tempest.Ask
	ctx    context.Context
	answer chan tempest.Choice
	sent   map[*rpc.Conn]bool
}

// Broker routes asks to prompt owners and explicitly opted-in watchers.
// Disconnects only remove approvers; they never decide an ask.
type Broker struct {
	mu        sync.Mutex
	approvers map[string]map[*rpc.Conn]int
	pending   map[string]map[string]*pendingAsk
	timeout   time.Duration
	clock     Clock
}

// NewBroker uses a five-minute timeout when timeout is zero.
func NewBroker(timeout time.Duration, clock Clock) *Broker {
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	if clock == nil {
		clock = realClock{}
	}
	return &Broker{approvers: make(map[string]map[*rpc.Conn]int), pending: make(map[string]map[string]*pendingAsk), timeout: timeout, clock: clock}
}

func (b *Broker) add(id string, c *rpc.Conn) func() {
	b.mu.Lock()
	if b.approvers[id] == nil {
		b.approvers[id] = make(map[*rpc.Conn]int)
	}
	b.approvers[id][c]++
	for _, p := range b.pending[id] {
		b.sendLocked(c, p)
	}
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.approvers[id][c]--
			if b.approvers[id][c] <= 0 {
				delete(b.approvers[id], c)
			}
			if len(b.approvers[id]) == 0 {
				delete(b.approvers, id)
			}
		})
	}
}
func (b *Broker) authorized(id string, c *rpc.Conn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.approvers[id][c] > 0
}

// Approve implements tempest.Prompter. Session owns the hub ask and final arbitration.
func (b *Broker) Approve(ctx context.Context, ask tempest.Ask) (tempest.Choice, error) {
	askCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := &pendingAsk{ask: ask, ctx: askCtx, answer: make(chan tempest.Choice, 1), sent: make(map[*rpc.Conn]bool)}
	timer := b.clock.NewTimer(b.timeout)
	defer timer.Stop()
	b.mu.Lock()
	if b.pending[ask.SessionID] == nil {
		b.pending[ask.SessionID] = make(map[string]*pendingAsk)
	}
	b.pending[ask.SessionID][ask.ToolCallID] = p
	for conn := range b.approvers[ask.SessionID] {
		b.sendLocked(conn, p)
	}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.pending[ask.SessionID], ask.ToolCallID)
		if len(b.pending[ask.SessionID]) == 0 {
			delete(b.pending, ask.SessionID)
		}
	}()
	select {
	case choice := <-p.answer:
		return choice, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C():
		return "", context.DeadlineExceeded
	}
}
func (b *Broker) sendLocked(c *rpc.Conn, p *pendingAsk) {
	if p.sent[c] {
		return
	}
	p.sent[c] = true
	go func() {
		options := p.ask.Options
		if len(options) == 0 {
			options = tempest.PermissionChoices()
		}
		params := map[string]any{"sessionId": p.ask.SessionID, "toolCall": map[string]any{"toolCallId": p.ask.ToolCallID, "title": p.ask.Title, "kind": p.ask.Kind, "status": "pending", "rawInput": p.ask.RawInput}, "options": options}
		if p.ask.ChildSessionID != "" {
			params["_meta"] = map[string]any{"tempest": map[string]string{"jobId": p.ask.JobID, "childSessionId": p.ask.ChildSessionID, "childToolCallId": p.ask.ChildToolCallID, "cwd": p.ask.Cwd}}
		}
		var result struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		}
		if c.Call(p.ctx, "session/request_permission", params, &result) != nil {
			return
		}
		var choice tempest.Choice
		switch result.Outcome.Outcome {
		case "selected":
			if !validChoice(result.Outcome.OptionID) {
				return
			}
			choice = tempest.Choice(result.Outcome.OptionID)
			if p.ask.ChildSessionID != "" && choice != "allow_once" && choice != "reject_once" {
				return
			}
		case "cancelled":
			choice = ""
		default:
			return
		}
		b.mu.Lock()
		allowed := b.approvers[p.ask.SessionID][c] > 0
		b.mu.Unlock()
		if allowed {
			select {
			case p.answer <- choice:
			case <-p.ctx.Done():
			default:
			}
		}
	}()
}
func validChoice(s string) bool {
	return s == "allow_once" || s == "allow_always" || s == "reject_once" || s == "reject_always"
}
