package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
)

func repeatedToolCalls(tool, args string, count int) []faux.ResponseStep {
	steps := make([]faux.ResponseStep, count)
	for index := range steps {
		steps[index] = faux.Respond(faux.ToolCall(tool, json.RawMessage(args), "call"))
	}
	return steps
}

// loopNotices counts the tool results that end with a loop nudge and with a
// loop stop notice.
func loopNotices(messages []types.Message) (nudges, stops int) {
	for _, message := range messages {
		result, ok := message.(types.ToolResultMessage)
		if !ok || len(result.Content) == 0 {
			continue
		}
		last := result.Content[len(result.Content)-1].Text
		switch {
		case strings.HasPrefix(last, "[Loop notice from the host]"):
			nudges++
		case strings.HasPrefix(last, "[Loop stop from the host]"):
			stops++
		}
	}
	return nudges, stops
}

// TestLoopGuardStopsAcrossDurableResume: a model re-reading an unchanged
// file is nudged on the third identical result; the session is closed and
// reopened from its journal, and the resumed Continue stops the loop after
// the repeats that remain, without nudging again.
func TestLoopGuardStopsAcrossDurableResume(t *testing.T) {
	ctx := t.Context()
	provider := faux.New(faux.Options{})
	owner := openFaux(t, provider, nil)
	cwd := owner.Snapshot().Cwd
	if err := os.WriteFile(filepath.Join(cwd, "notes.txt"), []byte("unchanged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := owner.SetBudget(Budget{MaxTurns: 4}); err != nil {
		t.Fatal(err)
	}
	_, updates, stopWatch := owner.Watch(ctx, WatchOptions{Buffer: 512})
	defer stopWatch()
	provider.SetResponses(repeatedToolCalls("read", `{"path":"notes.txt"}`, 4)...)
	result, err := owner.Prompt(ctx, Prompt{Text: "read the notes"})
	if err != nil || result.StopReason != agent.StopMaxTurns {
		t.Fatalf("first run=%+v %v, want max_turns", result, err)
	}
	if nudges, stops := loopNotices(owner.Messages()); nudges != 1 || stops != 0 {
		t.Fatalf("first run notices: %d nudges, %d stops; want one nudge", nudges, stops)
	}
	path := owner.core.store.Path()
	if err = owner.Close(ctx); err != nil {
		t.Fatal(err)
	}
	var guard []map[string]any
	for update := range updates {
		if update.Kind == "tempest/loop_guard" {
			var payload map[string]any
			if err := json.Unmarshal(update.Update, &payload); err != nil {
				t.Fatal(err)
			}
			guard = append(guard, payload)
		}
	}
	if len(guard) != 1 || guard[0]["action"] != "nudge" || guard[0]["pattern"] != "repeated_result" {
		t.Fatalf("tempest/loop_guard updates = %v, want one repeated_result nudge", guard)
	}

	resumed, err := Open(ctx, Options{SessionDir: filepath.Dir(path), Cwd: cwd, Resume: &ResumeSession{Path: path}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resumed.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	provider.SetResponses(append(repeatedToolCalls("read", `{"path":"notes.txt"}`, 3), faux.Respond(faux.Text("never")))...)
	result, err = resumed.Continue(ctx)
	var loop *agent.LoopError
	if err != nil || result.StopReason != agent.StopLoopDetected || result.Turns != 2 || !errors.As(result.Err, &loop) || loop.Pattern != agent.LoopRepeatedResult {
		t.Fatalf("resumed run=%+v %v, want loop_detected (repeated_result) after 2 calls", result, err)
	}
	if nudges, stops := loopNotices(resumed.Messages()); nudges != 1 || stops != 1 {
		t.Fatalf("resumed notices: %d nudges, %d stops; want the first nudge and one stop", nudges, stops)
	}
	loaded, err := journal.Load(ctx, path)
	if err != nil || loaded.LastRun == nil || !loaded.LastRun.Finished || loaded.LastRun.StopReason != string(agent.StopLoopDetected) {
		t.Fatalf("journal last run=%+v %v", loaded.LastRun, err)
	}
}

// TestLoopGuardLetsJobToolsPoll: the job tools are polling tools, so asking
// for the same job list again and again, with the same answer, is never a
// loop.
func TestLoopGuardLetsJobToolsPoll(t *testing.T) {
	ctx := t.Context()
	provider := faux.New(faux.Options{})
	owner := openFaux(t, provider, nil)
	provider.SetResponses(append(repeatedToolCalls("jobs", `{}`, 6), faux.Respond(faux.Text("done")))...)
	result, err := owner.Prompt(ctx, Prompt{Text: "watch the jobs"})
	if err != nil || result.StopReason != agent.StopCompleted || result.Turns != 7 {
		t.Fatalf("run=%+v %v, want completed after 7 turns", result, err)
	}
	if nudges, stops := loopNotices(owner.Messages()); nudges != 0 || stops != 0 {
		t.Fatalf("notices: %d nudges, %d stops; want none for polling", nudges, stops)
	}
}

// TestHostNoticesReachToolCallUpdates: a guard judges a batch after its
// results' tool_execution_end went out, so the notice it adds reaches
// clients as a further tool_call_update for that call carrying the full
// content. Both guards' notices do; a result without one sends no extra
// update.
func TestHostNoticesReachToolCallUpdates(t *testing.T) {
	ctx := t.Context()
	provider := faux.New(faux.Options{})
	owner := openFaux(t, provider, nil)
	cwd := owner.Snapshot().Cwd
	if err := os.WriteFile(filepath.Join(cwd, "notes.txt"), []byte("unchanged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, updates, stopWatch := owner.Watch(ctx, WatchOptions{Buffer: 512})
	defer stopWatch()
	script := append(repeatedToolCalls("read", `{"path":"notes.txt"}`, 3), repeatedToolCalls("read", `{"path":"missing.txt"}`, 2)...)
	provider.SetResponses(append(script, faux.Respond(faux.Text("done")))...)
	if result, err := owner.Prompt(ctx, Prompt{Text: "read the notes"}); err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("run=%+v %v", result, err)
	}
	if err := owner.Close(ctx); err != nil {
		t.Fatal(err)
	}
	completed := map[string]int{}
	var noticed []string
	for update := range updates {
		if update.Kind != "tool_call_update" {
			continue
		}
		var payload struct {
			Status  string `json:"status"`
			Content []struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"content"`
		}
		if err := json.Unmarshal(update.Update, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Status == "completed" || payload.Status == "failed" {
			completed[payload.Status]++
		}
		if n := len(payload.Content); n > 1 {
			noticed = append(noticed, payload.Status+": "+payload.Content[n-1].Content.Text)
		}
	}
	if len(noticed) != 2 || !strings.HasPrefix(noticed[0], "completed: [Loop notice from the host]") || !strings.HasPrefix(noticed[1], "failed: [Repeated failure notice from the host]") {
		t.Fatalf("updates carrying a notice = %q, want the loop notice then the failure notice", noticed)
	}
	// Five ends, plus one update per notice.
	if completed["completed"] != 4 || completed["failed"] != 3 {
		t.Fatalf("completed/failed tool_call_updates = %v", completed)
	}
}

// toolCallStep scripts one reply making one call with a unique id.
func toolCallStep(t *testing.T, tool string, args any, id int) faux.ResponseStep {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return faux.Respond(faux.ToolCall(tool, raw, fmt.Sprintf("call-%d", id)))
}

// approvingSession opens a session whose prompter allows every call, with
// room for long loops.
func approvingSession(t *testing.T, provider *faux.Faux) *Session {
	t.Helper()
	owner := openFaux(t, provider, promptFunc(func(context.Context, Ask) (Choice, error) { return permission.AllowOnce, nil }))
	if err := owner.SetBudget(Budget{MaxTurns: 60}); err != nil {
		t.Fatal(err)
	}
	return owner
}

// TestLoopGuardLetsSilentShellEditsFinish: the review's trace through the
// real bash and read tools. Each identical command appends the next number
// and prints nothing, then the unchanged reference file is read. The reads
// are not repeats, since the command may have changed the workspace; the
// command's own unchanged output gets the polling notice on its tenth run,
// and all twelve items are written.
func TestLoopGuardLetsSilentShellEditsFinish(t *testing.T) {
	provider := faux.New(faux.Options{})
	owner := approvingSession(t, provider)
	cwd := owner.Snapshot().Cwd
	for name, content := range map[string]string{"reference.txt": "reference: keep every input line\n", "data.txt": ""} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	command := `n=$(wc -l < data.txt); printf '%s\n' "$((n+1))" >> data.txt`
	for index := range 12 {
		provider.AppendResponses(toolCallStep(t, "bash", map[string]any{"command": command}, 2*index), toolCallStep(t, "read", map[string]any{"path": "reference.txt"}, 2*index+1))
	}
	provider.AppendResponses(faux.Respond(faux.Text("done")))
	result, err := owner.Prompt(t.Context(), Prompt{Text: "Append the next numbered item, consulting reference.txt after each, for twelve items."})
	if err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("run=%+v %v, want completed", result, err)
	}
	data, err := os.ReadFile(filepath.Join(cwd, "data.txt"))
	if err != nil || string(data) != "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n" {
		t.Fatalf("data.txt = %q %v, want twelve items", data, err)
	}
	runs := 0
	for _, message := range owner.Messages() {
		if result, ok := message.(types.ToolResultMessage); ok && result.ToolName == "bash" {
			runs++
			if got := agent.HostNoticed(result.Content); got != (runs == 10) {
				t.Fatalf("bash run %d noticed=%v, want the polling notice on run 10 only", runs, got)
			}
		}
	}
	if nudges, stops := loopNotices(owner.Messages()); nudges != 1 || stops != 0 {
		t.Fatalf("notices: %d nudges, %d stops; want one polling nudge", nudges, stops)
	}
}

// TestLoopGuardStopsRealUndoRedo: undo/redo through the real bash and write
// tools. Each leg answers the same every time it is redone (write's answer
// includes the content hash of what it wrote), so the polling bound nudges
// and then stops.
func TestLoopGuardStopsRealUndoRedo(t *testing.T) {
	legs := map[string]func(value string) map[string]any{
		"bash": func(value string) map[string]any {
			return map[string]any{"command": fmt.Sprintf("printf '%s' > toggle.txt", value)}
		},
		"write": func(value string) map[string]any { return map[string]any{"path": "toggle.txt", "content": value} },
	}
	for tool, args := range legs {
		t.Run(tool, func(t *testing.T) {
			provider := faux.New(faux.Options{})
			owner := approvingSession(t, provider)
			for index := range 44 {
				value := "on"
				if index%2 != 0 {
					value = "off"
				}
				provider.AppendResponses(toolCallStep(t, tool, args(value), index))
			}
			provider.AppendResponses(faux.Respond(faux.Text("never")))
			result, err := owner.Prompt(t.Context(), Prompt{Text: "toggle the setting"})
			var loop *agent.LoopError
			if err != nil || result.StopReason != agent.StopLoopDetected || !errors.As(result.Err, &loop) || loop.Pattern != agent.LoopRepeatedPoll {
				t.Fatalf("run=%+v %v, want loop_detected (repeated_poll)", result, err)
			}
			if nudges, stops := loopNotices(owner.Messages()); nudges != 1 || stops != 1 {
				t.Fatalf("notices: %d nudges, %d stops; want a nudge then a stop", nudges, stops)
			}
		})
	}
}

// TestRewindReplaysToolCallsWithTheirNotices: a guard's notice reaches
// clients in a tool_call_update; the history replayed from the journal and
// the history rebuilt by a rewind announce every call with its tool_call
// before any update for it, and carry the notice once.
func TestRewindReplaysToolCallsWithTheirNotices(t *testing.T) {
	ctx := t.Context()
	provider := faux.New(faux.Options{})
	owner := openFaux(t, provider, nil)
	cwd := owner.Snapshot().Cwd
	if err := os.WriteFile(filepath.Join(cwd, "reference.txt"), []byte("UNIQUE_REFERENCE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for index := range 3 {
		provider.AppendResponses(toolCallStep(t, "read", map[string]any{"path": "reference.txt"}, index))
	}
	provider.AppendResponses(faux.Respond(faux.Text("done")))
	if result, err := owner.Prompt(ctx, Prompt{Text: "read the reference"}); err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("run=%+v %v", result, err)
	}
	wellFormed := func(t *testing.T, s *Session) {
		t.Helper()
		snap := s.Snapshot()
		history, err := s.History(snap.HistoryEpoch, 0, snap.HistoryLen)
		if err != nil {
			t.Fatal(err)
		}
		announced := map[string]bool{}
		noticed := 0
		for _, u := range history {
			var payload struct {
				ToolCallID string `json:"toolCallId"`
			}
			if err := json.Unmarshal(u.Update, &payload); err != nil {
				t.Fatal(err)
			}
			switch u.Kind {
			case "tool_call":
				announced[payload.ToolCallID] = true
			case "tool_call_update":
				if !announced[payload.ToolCallID] {
					t.Fatalf("tool_call_update for %s before its tool_call: %s", payload.ToolCallID, u.Update)
				}
				if strings.Contains(string(u.Update), "[Loop notice from the host]") {
					noticed++
				}
			}
		}
		if len(announced) != 3 || noticed != 1 {
			t.Fatalf("history announces %d calls and carries the notice in %d updates; want 3 and 1", len(announced), noticed)
		}
	}
	t.Run("live", func(t *testing.T) { wellFormed(t, owner) })
	path := owner.core.store.Path()
	if err := owner.Close(ctx); err != nil {
		t.Fatal(err)
	}
	resumed, err := Open(ctx, Options{SessionDir: filepath.Dir(path), Cwd: cwd, Resume: &ResumeSession{Path: path}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resumed.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	t.Run("journal replay", func(t *testing.T) { wellFormed(t, resumed) })
	checkpoint, err := resumed.CreateCheckpoint(ctx, "keep the noticed read")
	if err != nil {
		t.Fatal(err)
	}
	provider.SetResponses(faux.Respond(faux.Text("later work")))
	if _, err := resumed.Prompt(ctx, Prompt{Text: "later prompt"}); err != nil {
		t.Fatal(err)
	}
	if err := resumed.Rewind(ctx, RewindOptions{Checkpoint: checkpoint.ID}); err != nil {
		t.Fatal(err)
	}
	t.Run("rewind", func(t *testing.T) { wellFormed(t, resumed) })
}
