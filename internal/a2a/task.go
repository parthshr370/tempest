package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	model "github.com/a2aproject/a2a-go/v2/a2a"
	"go.harness.dev/harness/tempest"
)

const (
	taskQueueSize               = 256
	maxTaskHistory              = 100
	maxProgressBytes            = 4 << 10
	maxArtifactBytes            = 1 << 20
	artifactChunkBytes          = 64 << 10
	progressInterval            = 100 * time.Millisecond
	maxApprovalToolBytes        = 256
	maxApprovalDescriptionBytes = 512
)

type pendingAsk struct {
	ToolCallID string `json:"toolCallId"`
	Tool       string `json:"tool"`
	Title      string `json:"title"`
	Reason     string `json:"reason"`
}
type runMetadata struct {
	StopReason tempest.StopReason `json:"stopReason"`
	Turns      int                `json:"turns"`
	Usage      tempest.Usage      `json:"usage"`
	CostUSD    *float64           `json:"costUsd"`
	ElapsedMS  int64              `json:"elapsedMs"`
	Error      string             `json:"error,omitempty"`
}
type taskMetadata struct {
	RunID               string       `json:"runId"`
	SessionID           string       `json:"sessionId"`
	PendingAsks         []pendingAsk `json:"pendingAsks,omitempty"`
	ProgressInterrupted bool         `json:"progressInterrupted,omitempty"`
	ArtifactTruncated   bool         `json:"artifactTruncated,omitempty"`
	*runMetadata
}

type subscriber struct {
	events chan model.Event
	gap    bool
}
type task struct {
	mu           sync.Mutex
	value        model.Task
	metadata     taskMetadata
	serial       uint64
	askSeq       uint64
	run          *tempest.Run
	session      *tempest.Session
	done         chan struct{}
	changed      chan struct{}
	subscribers  map[*subscriber]struct{}
	lastProgress time.Time
	terminalAt   time.Time
}

func (t *task) refreshAsks() {
	t.mu.Lock()
	session := t.session
	t.mu.Unlock()
	if session != nil {
		t.projectAsks(session.Snapshot(), time.Now().UTC())
	}
}
func (t *task) snapshot(history *int, artifacts bool) *model.Task {
	t.refreshAsks()
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked(history, artifacts)
}
func (t *task) snapshotLocked(history *int, artifacts bool) *model.Task {
	v := t.value
	n := maxTaskHistory
	if history != nil {
		n = min(*history, maxTaskHistory)
	}
	n = min(n, len(v.History))
	// Published history and artifacts are immutable; finish replaces slices
	// rather than mutating entries that an HTTP encoder may still be reading.
	v.History = v.History[len(v.History)-n:]
	if !artifacts {
		v.Artifacts = nil
	}
	return &v
}
func (t *task) notifyLocked(e model.Event) {
	close(t.changed)
	t.changed = make(chan struct{})
	for sub := range t.subscribers {
		select {
		case sub.events <- e:
		default:
			sub.gap = true
			close(sub.events)
			delete(t.subscribers, sub)
		}
	}
}
func (t *task) statusLocked() {
	t.notifyLocked(&model.TaskStatusUpdateEvent{TaskID: t.value.ID, ContextID: t.value.ContextID, Status: t.value.Status, Metadata: t.value.Metadata})
}
func (t *task) waitInterrupted(ctx context.Context) error {
	for {
		t.refreshAsks()
		t.mu.Lock()
		ready := t.value.Status.State.Terminal() || t.value.Status.State == model.TaskStateInputRequired
		changed := t.changed
		t.mu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
func (t *task) observe(initial tempest.Snapshot, updates <-chan tempest.Update, stop func(), ctx context.Context) {
	defer func() { stop() }()
	resultCh := make(chan tempest.RunResult, 1)
	go func() { result, _ := t.run.Wait(); resultCh <- result }()
	t.projectAsks(initial, time.Now().UTC())
	for {
		select {
		case result := <-resultCh:
			// Wait is authoritative. Drain only already-queued events; a closed or
			// gapped Watch must never make terminal capture wait for another event.
			for updates != nil {
				select {
				case u, ok := <-updates:
					if !ok {
						updates = nil
					} else {
						t.consume(u)
					}
				default:
					updates = nil
				}
			}
			t.terminalAt = t.run.FinishedAt()
			t.finish(result, t.run.Text())
			return
		case u, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			if u.Kind == "tempest/gap" {
				stop()
				snap, next, nextStop := t.session.Watch(ctx, tempest.WatchOptions{Buffer: taskQueueSize})
				updates = next
				stop = nextStop
				t.markGap()
				t.projectAsks(snap, time.Now().UTC())
				continue
			}
			t.consume(u)
		}
	}
}
func (t *task) consume(u tempest.Update) {
	if u.Kind == "tempest/gap" {
		t.markGap()
		return
	}
	var p struct {
		Content struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(u.Update, &p) != nil {
		return
	}
	if u.Kind == "tool_call" || u.Kind == "tool_call_update" || u.Kind == "tempest/run" {
		t.projectAsks(t.session.Snapshot(), u.At)
	}
	if u.Kind == "agent_message_chunk" && p.Content.Text != "" {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.value.Status.State != model.TaskStateWorking || u.At.Sub(t.lastProgress) < progressInterval {
			return
		}
		t.lastProgress = u.At
		t.value.Status.Message = t.message("progress", bounded(p.Content.Text, maxProgressBytes))
		t.statusLocked()
	}
}
func (t *task) publishMetadataLocked() {
	t.metadata.RunID, t.metadata.SessionID = string(t.value.ID), t.value.ContextID
	// The SDK owns the extension container; its value remains our typed,
	// immutable copy rather than an untyped bag of task state.
	t.value.Metadata = map[string]any{"tempest": t.metadata}
}
func (t *task) markGap() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.metadata.ProgressInterrupted {
		return
	}
	t.metadata.ProgressInterrupted = true
	t.publishMetadataLocked()
	t.statusLocked()
}
func (t *task) projectAsks(snap tempest.Snapshot, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.value.Status.State.Terminal() || snap.Seq < t.askSeq {
		return
	}
	t.askSeq = snap.Seq
	if snap.Run.RunID != string(t.value.ID) {
		return
	}
	asks := append([]tempest.Ask(nil), snap.PendingAsks...)
	sort.Slice(asks, func(i, j int) bool { return asks[i].ToolCallID < asks[j].ToolCallID })
	var pending []pendingAsk
	for _, ask := range asks {
		title, reason := approvalSummary(ask)
		pending = append(pending, pendingAsk{ToolCallID: ask.ToolCallID, Tool: bounded(ask.Tool, maxApprovalToolBytes), Title: title, Reason: reason})
	}
	state := model.TaskStateWorking
	if len(pending) > 0 {
		state = model.TaskStateInputRequired
	}
	if t.value.Status.State == state && slices.Equal(t.metadata.PendingAsks, pending) {
		return
	}
	if len(pending) > 0 {
		t.value.Status.Message = t.message("approval", fmt.Sprintf("Host approval required. Answer through ACP /acp tempest/permission for session %s and tool call %s, or cancel this task.", t.value.ContextID, pending[0].ToolCallID))
	} else {
		t.value.Status.Message = nil
	}
	at = at.UTC()
	t.value.Status.State = state
	t.value.Status.Timestamp = &at
	t.metadata.PendingAsks = pending
	t.publishMetadataLocked()
	t.statusLocked()
}

func approvalSummary(ask tempest.Ask) (string, string) {
	title, reason := ask.Title, ask.Reason
	// Core titles and guard reasons can embed the whole shell command. Keep
	// the approval discoverable without copying arguments into remote status.
	decoder := json.NewDecoder(bytes.NewReader(ask.RawInput))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			title, reason = ask.Tool, "Host tool permission policy requires approval"
			break
		}
		value, isText := token.(string)
		if !isText || value == "" {
			continue
		}
		if strings.Contains(title, value) {
			title = ask.Tool
		}
		if strings.Contains(reason, value) {
			reason = "Host tool permission policy requires approval"
		}
	}
	if title == "" {
		title = ask.Tool
	}
	if reason == "" {
		reason = "Host tool permission policy requires approval"
	}
	return bounded(title, maxApprovalDescriptionBytes), bounded(reason, maxApprovalDescriptionBytes)
}
func (t *task) message(suffix, text string) *model.Message {
	return &model.Message{ID: string(t.value.ID) + "_" + suffix, ContextID: t.value.ContextID, TaskID: t.value.ID, Role: model.MessageRoleAgent, Parts: model.ContentParts{model.NewTextPart(text)}}
}
func terminalState(reason tempest.StopReason) model.TaskState {
	switch reason {
	case "completed":
		return model.TaskStateCompleted
	case "aborted":
		return model.TaskStateCanceled
	default:
		return model.TaskStateFailed
	}
}
func (t *task) finish(result tempest.RunResult, text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	at := t.terminalAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	t.metadata.PendingAsks = nil
	t.metadata.runMetadata = &runMetadata{StopReason: result.StopReason, Turns: result.Turns, Usage: result.Usage, ElapsedMS: result.ElapsedMS}
	var cost *float64
	if !result.Usage.Cost.Unknown {
		v := result.Usage.Cost.Total
		cost = &v
	}
	t.metadata.CostUSD = cost
	if result.Err != nil {
		t.metadata.Error = "Run failed; inspect the host journal for details"
	}
	if len(text) > maxArtifactBytes {
		text = bounded(text, maxArtifactBytes-len(truncationMarker)) + truncationMarker
		t.metadata.ArtifactTruncated = true
	}
	t.publishMetadataLocked()
	if text != "" {
		t.value.Artifacts = []*model.Artifact{{ID: "answer", Name: "Final assistant response", Parts: model.ContentParts{model.NewTextPart(text)}}}
		t.value.History = append(append([]*model.Message(nil), t.value.History...), t.message("answer", text))
		chunks := splitText(text, artifactChunkBytes)
		for i, chunk := range chunks {
			t.notifyLocked(&model.TaskArtifactUpdateEvent{TaskID: t.value.ID, ContextID: t.value.ContextID, Artifact: &model.Artifact{ID: "answer", Name: "Final assistant response", Parts: model.ContentParts{model.NewTextPart(chunk)}}, Append: i > 0, LastChunk: i == len(chunks)-1})
		}
	}
	t.value.Status = model.TaskStatus{State: terminalState(result.StopReason), Timestamp: &at}
	// The bounded projection no longer needs the session transcript or the
	// Run's unbounded terminal capture after the observer drains.
	t.run, t.session = nil, nil
	t.statusLocked()
	for sub := range t.subscribers {
		close(sub.events)
		delete(t.subscribers, sub)
	}
	close(t.done)
}

const truncationMarker = "\n[Tempest artifact truncated at 1 MiB; full text remains in the session journal]"

func bounded(text string, n int) string {
	if len(text) <= n {
		return text
	}
	end := n
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}
func splitText(text string, n int) []string {
	var parts []string
	for len(text) > n {
		chunk := bounded(text, n)
		parts = append(parts, chunk)
		text = text[len(chunk):]
	}
	if text != "" {
		parts = append(parts, text)
	}
	return parts
}
