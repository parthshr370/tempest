package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
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
