package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.harness.dev/harness/internal/agent"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/durable"
)

// Mapper runs under the hub lock, so its per-session counters need no other lock.
type Mapper func(agent.AgentEvent) []Update

// Hub owns the reduced view and atomically attaches bounded subscribers.
type Hub struct {
	mu        sync.Mutex
	view      Snapshot
	history   []Update
	streaming []Update
	subs      map[uint64]*subscription
	next      uint64
	mapper    Mapper
	closed    bool
	jobs      map[string]work.Job
}

const (
	maxSubscriberBytes      = 4 << 20
	maxSnapshotHistoryBytes = 512 << 10
	updateEnvelopeBytes     = 256
)

type subscription struct {
	ch          chan Update
	sizes       []int
	head, count int
	bytes       int
}

// New seeds a hub without consulting the agent's independently locked state.
func New(seed Snapshot, mapper Mapper) *Hub {
	if seed.Run.Status == "" {
		seed.Run.Status = "idle"
	}
	h := &Hub{view: seed, subs: make(map[uint64]*subscription), mapper: mapper}
	h.jobs = make(map[string]work.Job)
	for _, job := range seed.Jobs {
		h.jobs[job.ID] = SummarizeJob(job)
	}
	h.refreshJobs()
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

// SetDurable publishes a bounded committed projection and its watermark together.
func (h *Hub) SetDurable(seq uint64, documents, submissions durable.Page) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || seq <= h.view.DurableSeq {
		return
	}
	h.view.DurableSeq = seq
	h.view.Documents = cloneRecords(documents.Records)
	h.view.Submissions = cloneRecords(submissions.Records)
	h.view.DocumentsCursor = documents.NextCursor
	h.view.SubmissionsCursor = submissions.NextCursor
	raw, _ := json.Marshal(struct {
		Seq               uint64           `json:"durableSeq"`
		Documents         []durable.Record `json:"documents"`
		Submissions       []durable.Record `json:"submissions"`
		DocumentsCursor   string           `json:"documentsCursor,omitempty"`
		SubmissionsCursor string           `json:"submissionsCursor,omitempty"`
	}{seq, h.view.Documents, h.view.Submissions, documents.NextCursor, submissions.NextCursor})
	h.emit(Update{Kind: "tempest/durable", Update: raw})
}

func cloneRecords(records []durable.Record) []durable.Record {
	out := slices.Clone(records)
	for i := range out {
		out[i].Value = append(json.RawMessage(nil), out[i].Value...)
	}
	return out
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
	for id, sub := range h.subs {
		ch := sub.ch
		consumed := sub.count - len(ch)
		for range consumed {
			sub.bytes -= sub.sizes[sub.head]
			sub.head = (sub.head + 1) % len(sub.sizes)
			sub.count--
		}
		size := len(u.Update) + len(u.Kind) + len(u.SessionID) + updateEnvelopeBytes
		if size <= maxSubscriberBytes-sub.bytes && len(ch) < cap(ch) {
			ch <- cloneUpdate(u)
			sub.sizes[(sub.head+sub.count)%len(sub.sizes)] = size
			sub.count++
			sub.bytes += size
			continue
		}
		// A byte or message limit is a terminal gap, never a partial update.
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
func (h *Hub) fold(u Update) {
	var p struct {
		SessionUpdate  string          `json:"sessionUpdate"`
		MessageID      string          `json:"messageId"`
		Content        json.RawMessage `json:"content"`
		Status         string          `json:"status"`
		RunID          string          `json:"runId"`
		LogicalRunID   string          `json:"logicalRunId"`
		ContinuationOf string          `json:"continuationOf"`
		Action         string          `json:"action"`
		Item           *work.QueueItem `json:"item"`
		Job            *work.Job       `json:"job"`
		Model          string          `json:"model"`
		StopReason     string          `json:"stopReason"`
		Error          string          `json:"error"`
		Turns          int             `json:"turns"`
		Index          int             `json:"index"`
		Phase          string          `json:"phase"`
		ToolCallID     string          `json:"toolCallId"`
		Title          string          `json:"title"`
		Kind           ToolKind        `json:"kind"`
		RawInput       json.RawMessage `json:"rawInput"`
		RawOutput      json.RawMessage `json:"rawOutput"`
		Used           int             `json:"used"`
		Size           int             `json:"size"`
		Usage          *UsageState     `json:"usageState"`
		Meta           struct {
			Tempest struct {
				Usage *UsageState `json:"usageState"`
			} `json:"tempest"`
		} `json:"_meta"`
	}
	if json.Unmarshal(u.Update, &p) != nil {
		return
	}
	switch u.Kind {
	case "tempest/queue":
		if p.Item == nil {
			break
		}
		if p.Action == "enqueued" {
			for _, item := range h.view.Queues {
				if item.ID == p.Item.ID {
					return
				}
			}
			h.view.Queues = append(h.view.Queues, *p.Item)
		} else {
			for i, item := range h.view.Queues {
				if item.ID == p.Item.ID {
					h.view.Queues = append(h.view.Queues[:i], h.view.Queues[i+1:]...)
					break
				}
			}
		}
	case "tempest/job":
		if p.Job != nil {
			h.jobs[p.Job.ID] = SummarizeJob(*p.Job)
			h.refreshJobs()
		}
	case "tempest/run":
		if p.Status == "started" {
			h.view.Run = RunState{Status: "running", RunID: p.RunID, LogicalRunID: p.LogicalRunID, ContinuationOf: p.ContinuationOf, StartedAt: u.At}
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
	return h.watch(buf, false)
}

// WatchReplay captures the complete replay under the subscription lock. Replay
// delivery gets its own queue allowance, so it doesn't consume live slack.
func (h *Hub) WatchReplay(buf int) (Snapshot, <-chan Update, func()) {
	return h.watch(buf, true)
}

func (h *Hub) watch(buf int, replay bool) (Snapshot, <-chan Update, func()) {
	if buf <= 0 {
		buf = 256
	}
	h.mu.Lock()
	snap := h.snapshot(replay)
	if replay {
		buf += min(len(snap.History), maxSubscriberBytes/128)
	}
	buf = max(1, min(buf, maxSubscriberBytes/128))
	ch := make(chan Update, buf)
	h.next++
	id := h.next
	if h.closed {
		close(ch)
	} else {
		h.subs[id] = &subscription{ch: ch, sizes: make([]int, buf)}
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

// Snapshot returns a detached view with a byte-bounded tail of at most 200 updates.
func (h *Hub) Snapshot() Snapshot { h.mu.Lock(); defer h.mu.Unlock(); return h.snapshot(false) }
func (h *Hub) snapshot(replay bool) Snapshot {
	s := h.view
	s.Documents = cloneRecords(s.Documents)
	s.Submissions = cloneRecords(s.Submissions)
	s.HistoryLen = len(h.history)
	start := max(0, len(h.history)-200)
	streaming := 0
	if replay {
		start = 0
		streaming = len(h.streaming)
	} else {
		bytes := 0
		for i := len(h.history) - 1; i >= start; i-- {
			update := h.history[i]
			size := len(update.Update) + len(update.Kind) + len(update.SessionID) + updateEnvelopeBytes
			if size > maxSnapshotHistoryBytes-bytes {
				start = i + 1
				break
			}
			bytes += size
		}
	}
	s.History = make([]Update, 0, len(h.history)-start+streaming)
	for _, update := range h.history[start:] {
		s.History = append(s.History, cloneUpdate(update))
	}
	if replay {
		for _, update := range h.streaming {
			s.History = append(s.History, cloneUpdate(update))
		}
	}
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
	s.Queues = append([]work.QueueItem{}, s.Queues...)
	s.Jobs = append([]work.Job{}, s.Jobs...)
	for i := range s.Jobs {
		s.Jobs[i].Usage = cloneUsage(s.Jobs[i].Usage)
	}
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
	for id, sub := range h.subs {
		close(sub.ch)
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

// SummarizeJob removes private paths and large results from shared list views.
func SummarizeJob(j work.Job) work.Job {
	j.Cwd = ""
	j.Result = nil
	j.Patch = nil
	j.Output = nil
	j.Value = nil
	j.ValueInResult = false
	if len(j.ResultText) > 512 {
		n := 512
		for n > 0 && !utf8.RuneStart(j.ResultText[n]) {
			n--
		}
		j.ResultText = j.ResultText[:n]
	}
	return j
}
func (h *Hub) refreshJobs() {
	const recentJobs = 20
	latest := make([]work.Job, 0, recentJobs+8)
	active := make([]work.Job, 0, 8)
	for _, job := range h.jobs {
		switch job.State {
		case "starting", "running", "recovering":
			active = append(active, job)
		}
		if len(latest) == recentJobs && compareRecentJobs(job, latest[recentJobs-1]) >= 0 {
			continue
		}
		if len(latest) < recentJobs {
			latest = append(latest, job)
		} else {
			latest[recentJobs-1] = job
		}
		slices.SortFunc(latest, compareRecentJobs)
	}
	for _, job := range active {
		if !slices.ContainsFunc(latest, func(summary work.Job) bool { return summary.ID == job.ID }) {
			latest = append(latest, job)
		}
	}
	slices.SortFunc(latest, compareRecentJobs)
	h.view.Jobs = latest
	h.view.JobsTruncated = len(latest) < len(h.jobs)
	// The recent/active selection isn't a contiguous id page. Start ordinary
	// pagination at the empty cursor so older jobs can't be skipped.
	h.view.JobsCursor = ""
}
func compareRecentJobs(first, second work.Job) int {
	if order := second.StartedAt.Compare(first.StartedAt); order != 0 {
		return order
	}
	return strings.Compare(second.ID, first.ID)
}
