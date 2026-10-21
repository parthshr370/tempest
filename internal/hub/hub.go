package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"go.harness.dev/harness/internal/agent"
	types "go.harness.dev/harness/internal/engine/types"
)

// Mapper runs under the hub lock, so its per-session counters need no other lock.
type Mapper func(agent.AgentEvent) []Update

// Hub owns the reduced view and atomically attaches bounded subscribers.
type Hub struct {
	mu        sync.Mutex
	view      Snapshot
	history   []Update
	streaming []Update
	subs      map[uint64]chan Update
	next      uint64
	mapper    Mapper
	closed    bool
}

// New seeds a hub without consulting the agent's independently locked state.
func New(seed Snapshot, mapper Mapper) *Hub {
	if seed.Run.Status == "" {
		seed.Run.Status = "idle"
	}
	h := &Hub{view: seed, subs: make(map[uint64]chan Update), mapper: mapper}
	h.history = cloneUpdates(seed.History)
	h.view.History = nil
	return h
}

// Listener folds and publishes events and never returns an error.
func (h *Hub) Listener() agent.AgentEventSink {
	return func(_ context.Context, ev agent.AgentEvent) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.closed {
			return nil
		}
		if h.mapper != nil {
			for _, u := range h.mapper(ev) {
				h.emit(u)
			}
		}
		return nil
	}
}

// Emit publishes a hub-originated event.
func (h *Hub) Emit(u Update) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.emit(u)
	}
}

// AddAsk publishes a pending approval and makes it visible in the same snapshot.
func (h *Hub) AddAsk(ask Ask) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.view.PendingAsks = append(h.view.PendingAsks, cloneAsk(ask))
	raw, _ := json.Marshal(map[string]any{"sessionUpdate": "tool_call", "toolCallId": ask.ToolCallID, "title": ask.Title, "kind": ask.Kind, "status": "pending", "rawInput": ask.RawInput})
	h.emit(Update{Kind: "tool_call", Update: raw})
}

// RemoveAsk removes a resolved or cancelled approval.
func (h *Hub) RemoveAsk(id string) { h.mu.Lock(); defer h.mu.Unlock(); h.removeAsk(id) }
func (h *Hub) removeAsk(id string) {
	for i := range h.view.PendingAsks {
		if h.view.PendingAsks[i].ToolCallID == id {
			h.view.PendingAsks = append(h.view.PendingAsks[:i], h.view.PendingAsks[i+1:]...)
			return
		}
	}
}
func (h *Hub) emit(u Update) {
	h.view.Seq++
	u.Seq = h.view.Seq
	u.SessionID = h.view.SessionID
	if u.At.IsZero() {
		u.At = time.Now()
	}
	u.Update = append(json.RawMessage(nil), u.Update...)
	h.fold(u)
	for id, ch := range h.subs {
		select {
		case ch <- cloneUpdate(u):
		default:
			// Drop queued data to reserve one terminal gap, even for a one-slot buffer.
			// The receiver may already have consumed a prefix; it must re-snapshot.
			for len(ch) > 0 {
				select {
				case <-ch:
				default:
				}
			}
			raw, _ := json.Marshal(map[string]any{"lastSeq": u.Seq})
			ch <- Update{Seq: u.Seq, At: u.At, SessionID: u.SessionID, Kind: "tempest/gap", Update: raw}
			close(ch)
			delete(h.subs, id)
		}
	}
}
func (h *Hub) fold(u Update) {
	var p struct {
		SessionUpdate string          `json:"sessionUpdate"`
		MessageID     string          `json:"messageId"`
		Content       json.RawMessage `json:"content"`
		Status        string          `json:"status"`
		RunID         string          `json:"runId"`
		Model         string          `json:"model"`
		StopReason    string          `json:"stopReason"`
		Error         string          `json:"error"`
		Turns         int             `json:"turns"`
		Index         int             `json:"index"`
		Phase         string          `json:"phase"`
		ToolCallID    string          `json:"toolCallId"`
		Title         string          `json:"title"`
		Kind          ToolKind        `json:"kind"`
		RawInput      json.RawMessage `json:"rawInput"`
		RawOutput     json.RawMessage `json:"rawOutput"`
		Used          int             `json:"used"`
		Size          int             `json:"size"`
		Usage         *UsageState     `json:"usageState"`
		Meta          struct {
			Tempest struct {
				Usage *UsageState `json:"usageState"`
			} `json:"tempest"`
		} `json:"_meta"`
	}
	if json.Unmarshal(u.Update, &p) != nil {
		return
	}
	switch u.Kind {
	case "tempest/run":
		if p.Status == "started" {
			h.view.Run = RunState{Status: "running", RunID: p.RunID, StartedAt: u.At}
			h.view.Tools = nil
			h.view.Interrupted = nil
		} else if p.Status == "finished" {
			h.view.LastRun = &RunSummary{RunID: p.RunID, StopReason: p.StopReason, Error: p.Error, Finished: true, FinishedAt: u.At}
			h.view.Run.Status = "idle"
			h.view.Run.Turns = p.Turns
			h.view.PendingAsks = nil
			h.view.Streaming = nil
			h.streaming = nil
		}
	case "tempest/turn":
		h.view.Run.Turns = p.Index
	case "agent_message_chunk", "agent_thought_chunk":
		if h.view.Streaming == nil || h.view.Streaming.MessageID != p.MessageID {
			h.view.Streaming = &StreamingMessage{MessageID: p.MessageID}
		}
		var content struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(p.Content, &content)
		if u.Kind == "agent_message_chunk" {
			h.view.Streaming.Text += content.Text
		} else {
			h.view.Streaming.Thought += content.Text
		}
		h.streaming = append(h.streaming, cloneUpdate(u))
	case "tempest/message_end":
		h.history = append(h.history, h.streaming...)
		h.streaming = nil
		h.view.Streaming = nil
		h.history = append(h.history, cloneUpdate(u))
	case "user_message_chunk":
		h.history = append(h.history, cloneUpdate(u))
	case "tool_call", "tool_call_update":
		found := -1
		for i := range h.view.Tools {
			if h.view.Tools[i].ToolCallID == p.ToolCallID {
				found = i
				break
			}
		}
		if found < 0 {
			h.view.Tools = append(h.view.Tools, ToolState{ToolCallID: p.ToolCallID})
			found = len(h.view.Tools) - 1
		}
		t := &h.view.Tools[found]
		if p.Title != "" {
			t.Title = p.Title
		}
		if p.Kind != "" {
			t.Kind = p.Kind
		}
		if p.Status != "" {
			t.Status = p.Status
		}
		if p.RawInput != nil {
			t.RawInput = p.RawInput
		}
		if p.RawOutput != nil {
			t.RawOutput = p.RawOutput
		}
		if p.Content != nil {
			t.Content = p.Content
		}
		var meta struct {
			Meta struct {
				Tempest struct {
					Permission json.RawMessage `json:"permission"`
				} `json:"tempest"`
			} `json:"_meta"`
		}
		_ = json.Unmarshal(u.Update, &meta)
		if len(meta.Meta.Tempest.Permission) > 0 {
			h.removeAsk(p.ToolCallID)
		}
		h.history = append(h.history, cloneUpdate(u))
	case "usage_update", "tempest/usage":
		if p.Usage != nil {
			h.view.Usage = *p.Usage
		}
		if p.Meta.Tempest.Usage != nil {
			h.view.Usage = *p.Meta.Tempest.Usage
		}
		if u.Kind == "usage_update" {
			h.view.Usage.ContextUsed = p.Used
			h.view.Usage.ContextSize = p.Size
		}
	}
}

// Watch captures the view and registers the live queue under one lock.
func (h *Hub) Watch(buf int) (Snapshot, <-chan Update, func()) {
	if buf <= 0 {
		buf = 256
	}
	h.mu.Lock()
	snap := h.snapshot()
	ch := make(chan Update, buf)
	h.next++
	id := h.next
	if h.closed {
		close(ch)
	} else {
		h.subs[id] = ch
	}
	h.mu.Unlock()
	var once sync.Once
	return snap, ch, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if _, ok := h.subs[id]; ok {
				delete(h.subs, id)
				close(ch)
			}
		})
	}
}

// Snapshot returns a detached view with at most 200 transcript updates.
func (h *Hub) Snapshot() Snapshot { h.mu.Lock(); defer h.mu.Unlock(); return h.snapshot() }
func (h *Hub) snapshot() Snapshot {
	s := h.view
	s.HistoryLen = len(h.history)
	start := max(0, len(h.history)-200)
	s.History = cloneUpdates(h.history[start:])
	if s.Streaming != nil {
		v := *s.Streaming
		s.Streaming = &v
	}
	if s.LastRun != nil {
		v := *s.LastRun
		s.LastRun = &v
	}
	s.Tools = append([]ToolState{}, s.Tools...)
	for i := range s.Tools {
		s.Tools[i].RawInput = append(json.RawMessage(nil), s.Tools[i].RawInput...)
		s.Tools[i].RawOutput = append(json.RawMessage(nil), s.Tools[i].RawOutput...)
		s.Tools[i].Content = append(json.RawMessage(nil), s.Tools[i].Content...)
	}
	s.PendingAsks = append([]Ask{}, s.PendingAsks...)
	for i := range s.PendingAsks {
		s.PendingAsks[i] = cloneAsk(s.PendingAsks[i])
	}
	s.Interrupted = append(s.Interrupted[:0:0], s.Interrupted...)
	for i := range s.Interrupted {
		s.Interrupted[i].Args = append(json.RawMessage(nil), s.Interrupted[i].Args...)
	}
	s.Usage.Total = cloneUsage(s.Usage.Total)
	s.Usage.LastCall = cloneUsage(s.Usage.LastCall)
	s.Diagnostics = append([]string{}, s.Diagnostics...)
	return s
}

// History pages the full transcript, rejecting offsets outside the current epoch.
func (h *Hub) History(epoch, offset, limit int) ([]Update, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if epoch != h.view.HistoryEpoch {
		return nil, ErrStaleEpoch
	}
	if offset < 0 || limit < 0 || offset > len(h.history) {
		return nil, fmt.Errorf("invalid history range")
	}
	return cloneUpdates(h.history[offset : offset+min(limit, len(h.history)-offset)]), nil
}

// ReplaceHistory folds a replacement transcript atomically without replaying it
// to live subscribers. Its old epoch can no longer page the replaced history.
func (h *Hub) ReplaceHistory(rebuild func() []Update) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.history = nil
	h.streaming = nil
	h.view.Streaming = nil
	h.view.Tools = nil
	h.view.HistoryEpoch++
	for _, u := range rebuild() {
		u.Seq = h.view.Seq
		u.SessionID = h.view.SessionID
		h.fold(u)
	}
}

// Configure updates settings without inventing an agent event.
func (h *Hub) Configure(b agent.Budget, mode string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.view.Budget = b
	h.view.PermissionMode = mode
}

// Close ends all live streams after the session has joined its run.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for id, ch := range h.subs {
		close(ch)
		delete(h.subs, id)
	}
}
func cloneUpdate(u Update) Update { u.Update = append(json.RawMessage(nil), u.Update...); return u }
func cloneUpdates(us []Update) []Update {
	out := make([]Update, len(us))
	for i, u := range us {
		out[i] = cloneUpdate(u)
	}
	return out
}
func cloneAsk(a Ask) Ask {
	a.RawInput = append(json.RawMessage(nil), a.RawInput...)
	a.Options = append([]Option(nil), a.Options...)
	return a
}

func cloneUsage(u types.Usage) types.Usage {
	if u.CacheWrite1h != nil {
		v := *u.CacheWrite1h
		u.CacheWrite1h = &v
	}
	if u.Reasoning != nil {
		v := *u.Reasoning
		u.Reasoning = &v
	}
	return u
}
