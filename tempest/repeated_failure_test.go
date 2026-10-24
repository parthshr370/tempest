package tempest

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
)

func stuckWriteStdin(count int) []faux.ResponseStep {
	steps := make([]faux.ResponseStep, count)
	for index := range steps {
		steps[index] = faux.Respond(faux.ToolCall("write_stdin", json.RawMessage(`{"job_id":"x"}`), "call"))
	}
	return steps
}

// TestRepeatedFailureStopsAcrossDurableResume: a run that repeats one failing
// call is stopped with repeated_failure, and the streak is read back from the
// journal, so a resumed Continue stops after the calls that remain.
func TestRepeatedFailureStopsAcrossDurableResume(t *testing.T) {
	ctx := t.Context()
	provider := faux.New(faux.Options{})
	owner := openFaux(t, provider, nil)
	before := agent.RepeatedFailureLimit - 2
	if err := owner.SetBudget(Budget{MaxTurns: before}); err != nil {
		t.Fatal(err)
	}
	provider.SetResponses(stuckWriteStdin(before)...)
	result, err := owner.Prompt(ctx, Prompt{Text: "type into the job"})
	if err != nil || result.StopReason != agent.StopMaxTurns {
		t.Fatalf("first run=%+v %v, want max_turns", result, err)
	}
	first, ok := owner.Messages()[2].(types.ToolResultMessage)
	if !ok || !first.IsError || !strings.Contains(first.Content[0].Text, "no background jobs") || !strings.Contains(first.Content[0].Text, "stdin:true") {
		t.Fatalf("first write_stdin result=%#v, want no jobs and how to start one", owner.Messages()[2])
	}
	path := owner.core.store.Path()
	cwd := owner.Snapshot().Cwd
	if err = owner.Close(ctx); err != nil {
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
	if err = resumed.SetBudget(Budget{MaxTurns: 40}); err != nil {
		t.Fatal(err)
	}
	provider.SetResponses(append(stuckWriteStdin(agent.RepeatedFailureLimit), faux.Respond(faux.Text("never")))...)
	result, err = resumed.Continue(ctx)
	if want := agent.RepeatedFailureLimit - before; err != nil || result.StopReason != agent.StopRepeatedFailure || result.Turns != want || result.Err == nil {
		t.Fatalf("resumed run=%+v %v, want repeated_failure after %d calls", result, err, want)
	}
	loaded, err := journal.Load(ctx, path)
	if err != nil || loaded.LastRun == nil || !loaded.LastRun.Finished || loaded.LastRun.StopReason != string(agent.StopRepeatedFailure) {
		t.Fatalf("journal last run=%+v %v", loaded.LastRun, err)
	}
}
