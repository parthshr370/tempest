package a2a

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	model "github.com/a2aproject/a2a-go/v2/a2a"
	"go.harness.dev/harness/tempest"
)

func projection(id string, state model.TaskState, at time.Time) *task {
	return &task{value: model.Task{ID: model.TaskID(id), ContextID: "context", Status: model.TaskStatus{State: state, Timestamp: &at}, Metadata: map[string]any{"tempest": map[string]any{"runId": id, "sessionId": "context"}}}, done: make(chan struct{}), changed: make(chan struct{}), subscribers: make(map[*subscriber]struct{})}
}
func TestTerminalStateAndMetadata(t *testing.T) {
	for _, test := range []struct {
		reason tempest.StopReason
		state  model.TaskState
	}{{"completed", model.TaskStateCompleted}, {"aborted", model.TaskStateCanceled}, {"max_turns", model.TaskStateFailed}, {"max_cost", model.TaskStateFailed}, {"max_wall_time", model.TaskStateFailed}, {"error", model.TaskStateFailed}} {
		t.Run(string(test.reason), func(t *testing.T) {
			at := time.Now().UTC()
			task := projection("run", model.TaskStateWorking, at)
			task.terminalAt = at
			result := tempest.RunResult{RunID: "run", StopReason: test.reason, Turns: 3, ElapsedMS: 12}
			result.Usage.Cost.Unknown = true
			if test.reason == "error" {
				result.Err = errors.New("secret provider Authorization header")
			}
			task.finish(result, "partial answer")
			v := task.snapshot(nil, true)
			if v.Status.State != test.state || !v.Status.Timestamp.Equal(at) || v.Artifacts[0].Parts[0].Text() != "partial answer" {
				t.Fatalf("%+v", v)
			}
			raw, _ := json.Marshal(v)
			if strings.Contains(string(raw), "Authorization") {
				t.Fatal("provider credentials leaked")
			}
			if !strings.Contains(string(raw), `"costUsd":null`) || !strings.Contains(string(raw), `"turns":3`) {
				t.Fatal(string(raw))
			}
		})
	}
}
func TestArtifactUTF8ChunksAndExplicitLimit(t *testing.T) {
	for _, text := range []string{strings.Repeat("界", 50000), strings.Repeat("界", 400000), ""} {
		task := projection("run", model.TaskStateWorking, time.Now().UTC())
		sub := &subscriber{events: make(chan model.Event, 256)}
		task.subscribers[sub] = struct{}{}
		task.finish(tempest.RunResult{StopReason: "completed"}, text)
		var joined strings.Builder
		count := 0
		last := false
		terminal := false
		for event := range sub.events {
			switch e := event.(type) {
			case *model.TaskArtifactUpdateEvent:
				chunk := e.Artifact.Parts[0].Text()
				if len(chunk) > 64<<10 || !utf8.ValidString(chunk) || e.Append != (count > 0) || last || terminal {
					t.Fatal("bad artifact chunk")
				}
				count++
				joined.WriteString(chunk)
				last = e.LastChunk
			case *model.TaskStatusUpdateEvent:
				if text != "" && !last {
					t.Fatal("terminal before last chunk")
				}
				terminal = true
			}
		}
		expected := text
		if len(expected) > 1<<20 {
			expected = bounded(text, (1<<20)-len(truncationMarker)) + truncationMarker
		}
		if joined.String() != expected || len(joined.String()) > 1<<20 {
			t.Fatal("artifact reassembly/limit")
		}
		v := task.snapshot(nil, true)
		if text != "" && v.Artifacts[0].Parts[0].Text() != expected {
			t.Fatal("GetTask materialization")
		}
		if text == "" && len(v.Artifacts) != 0 {
			t.Fatal("fabricated empty artifact")
		}
		if len(text) > 1<<20 {
			raw, _ := json.Marshal(v.Metadata)
			if !strings.Contains(string(raw), `"artifactTruncated":true`) {
				t.Fatal(string(raw))
			}
		}
	}
}
func TestSlowSubscriberDoesNotChangeTaskOrPeers(t *testing.T) {
	task := projection("run", model.TaskStateWorking, time.Now().UTC())
	slow := &subscriber{events: make(chan model.Event, 256)}
	fast := &subscriber{events: make(chan model.Event, 256)}
	task.subscribers[slow] = struct{}{}
	task.subscribers[fast] = struct{}{}
	for range 300 {
		task.mu.Lock()
		task.statusLocked()
		task.mu.Unlock()
		<-fast.events
	}
	if !slow.gap || fast.gap || task.snapshot(nil, false).Status.State != model.TaskStateWorking {
		t.Fatal("slow stream affected task/peer")
	}
	task.finish(tempest.RunResult{StopReason: "completed"}, "answer")
	if task.snapshot(nil, true).Status.State != model.TaskStateCompleted {
		t.Fatal("task didn't complete")
	}
}
func TestRetentionRunningTasksAndWatermark(t *testing.T) {
	now := time.Now().UTC()
	s := &Server{tasks: map[model.TaskID]*task{}, now: func() time.Time { return now }, retention: 24 * time.Hour, capacity: 3}
	running := projection("running", model.TaskStateWorking, now.Add(-48*time.Hour))
	expired := projection("expired", model.TaskStateCompleted, now.Add(-25*time.Hour))
	fresh := projection("fresh", model.TaskStateCompleted, now.Add(-time.Hour))
	for i, task := range []*task{running, expired, fresh} {
		task.serial = uint64(i + 1)
		s.tasks[task.value.ID] = task
	}
	s.serial = 3
	s.expireLocked()
	if s.tasks["expired"] != nil || s.tasks["running"] == nil || s.tasks["fresh"] == nil {
		t.Fatal("retention evicted running or kept expired")
	}
	page, e := s.list(json.RawMessage(`{"pageSize":1}`))
	if e != nil || page.TotalSize != 2 || page.NextPageToken == "" {
		t.Fatal(page, e)
	}
	newer := projection("new", model.TaskStateCompleted, now)
	s.serial++
	newer.serial = s.serial
	s.tasks[newer.value.ID] = newer
	raw, _ := json.Marshal(map[string]any{"pageSize": 1, "pageToken": page.NextPageToken})
	next, e := s.list(raw)
	if e != nil || next.TotalSize != 2 || len(next.Tasks) != 1 || next.Tasks[0].ID != "running" || next.NextPageToken != "" {
		t.Fatal(next, e)
	}
	// Full, unexpired capacity rejects before the backend can create a session.
	p, prompt, e := decodeSend(json.RawMessage(`{"message":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"go"}]}}`))
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.send(t.Context(), p, prompt)
	if e == nil || e.Code != -32004 {
		t.Fatal("capacity not enforced", e)
	}
}
func TestPendingAskProjectionIsNotOrdinaryToolPending(t *testing.T) {
	task := projection("run", model.TaskStateWorking, time.Now().UTC())
	snap := tempest.Snapshot{Run: tempest.RunState{RunID: "run", Status: "running"}, Tools: []tempest.ToolState{{ToolCallID: "allowed", Status: "pending"}}}
	task.projectAsks(snap, time.Now())
	if task.snapshot(nil, false).Status.State != model.TaskStateWorking {
		t.Fatal("ordinary pending tool became approval")
	}
	snap.Seq = 2
	snap.PendingAsks = []tempest.Ask{{ToolCallID: "ask", Tool: "work", Title: "Work hidden", Reason: "Host policy: hidden", RawInput: json.RawMessage(`{"secret":"hidden"}`)}}
	task.projectAsks(snap, time.Now())
	v := task.snapshot(nil, false)
	raw, _ := json.Marshal(v)
	if v.Status.State != model.TaskStateInputRequired || strings.Contains(string(raw), "hidden") {
		t.Fatal(string(raw))
	}
	old := snap
	snap.Seq = 3
	snap.PendingAsks = nil
	task.projectAsks(snap, time.Now())
	if task.snapshot(nil, false).Status.State != model.TaskStateWorking {
		t.Fatal("resolved ask not working")
	}
	task.projectAsks(old, time.Now())
	if task.snapshot(nil, false).Status.State != model.TaskStateWorking {
		t.Fatal("stale snapshot resurrected resolved approval")
	}
}
