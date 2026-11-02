package session

import (
	"context"
	"os"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
)

// TestRunLimitsHoldAcrossCrashResume: the three-call batch's tool calls and
// tokens are journaled before the crash, and the resumed logical run counts
// them exactly once against its caps.
func TestRunLimitsHoldAcrossCrashResume(t *testing.T) {
	ctx := context.Background()
	store, err := journal.Create(ctx, t.TempDir(), "/workspace/project", journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	path := store.Path()
	ledger := newToolLedger(path)
	f := faux.New(faux.Options{})
	f.SetResponses(faux.RespondMessage(types.AssistantMessage{Content: crashCalls, StopReason: types.StopToolUse}), faux.Respond(faux.Text("never")))
	agt := newCrashAgent(f, ledger, agent.ExecSequential, nil)
	agt.SetBudget(agent.Budget{MaxToolCalls: 10})
	detach := AttachJournal(agt, store)
	// The process dies once the batch's results are journaled, before
	// run_finished, so a resumed run continues the same logical run.
	agt.Subscribe(func(_ context.Context, ev agent.AgentEvent) error {
		if ev.Type == agent.EventTurnEnd && len(ev.ToolResults) > 0 {
			if err := store.Close(); err != nil {
				t.Errorf("close store at crash: %v", err)
			}
			return errCrashed
		}
		return nil
	})
	if _, err := agt.PromptText(ctx, "do the three things"); err != nil {
		t.Fatal(err)
	}
	detach()
	crashed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	logicalUsage := func(t *testing.T) types.Usage {
		t.Helper()
		loaded, err := journal.Load(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		usage, _, _ := loaded.LogicalUsage(loaded.LastRun.LogicalRunID)
		return usage
	}
	spent := logicalUsage(t)
	if spent.ToolCalls != len(crashCalls) || spent.Output == 0 {
		t.Fatalf("journaled usage = %+v, want %d tool calls and the first response's tokens", spent, len(crashCalls))
	}

	nextBatch := types.AssistantMessage{Content: []types.ContentBlock{crashCalls[0], crashCalls[0]}, StopReason: types.StopToolUse}
	nextBatch.Content[0].ID, nextBatch.Content[1].ID = "call-again-1", "call-again-2"
	// resumeWith resumes the crashed journal, restored for each case, on a
	// provider that asks for two more calls.
	resumeWith := func(t *testing.T, budget agent.Budget) (agent.RunResult, *faux.Faux) {
		t.Helper()
		if err := os.WriteFile(path, crashed, 0o600); err != nil {
			t.Fatal(err)
		}
		store, loaded, err := journal.OpenForResume(ctx, path, journal.Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		f := faux.New(faux.Options{})
		f.SetResponses(faux.RespondMessage(nextBatch), faux.Respond(faux.Text("done")))
		agt := newCrashAgent(f, ledger, agent.ExecSequential, loaded.Messages)
		agt.SetBudget(budget)
		detach := AttachJournal(agt, store)
		defer detach()
		result, err := agt.Continue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return result, f
	}

	t.Run("token cap reached by journaled usage", func(t *testing.T) {
		result, f := resumeWith(t, agent.Budget{MaxOutputTokens: spent.Output})
		if result.StopReason != agent.StopMaxOutputTokens || f.CallCount() != 0 || result.Usage.Output != spent.Output {
			t.Fatalf("result = %+v calls = %d, want max_output_tokens before any request", result, f.CallCount())
		}
	})
	t.Run("token cap one above journaled usage", func(t *testing.T) {
		result, f := resumeWith(t, agent.Budget{MaxOutputTokens: spent.Output + 1})
		if result.StopReason != agent.StopMaxOutputTokens || f.CallCount() != 1 {
			t.Fatalf("result = %+v calls = %d, want one more request", result, f.CallCount())
		}
	})
	t.Run("tool calls counted once", func(t *testing.T) {
		before, _ := ledger.snapshot()
		// 3 recovered + 2 asked = 5 > 4: the batch is refused.
		result, f := resumeWith(t, agent.Budget{MaxToolCalls: 4})
		after, _ := ledger.snapshot()
		if result.StopReason != agent.StopMaxToolCalls || f.CallCount() != 1 || after["call-again-1"] != before["call-again-1"] || result.Usage.ToolCalls != len(crashCalls) {
			t.Fatalf("result = %+v calls = %d, want the batch refused against 3 recovered calls", result, f.CallCount())
		}
		if got := logicalUsage(t).ToolCalls; got != len(crashCalls) {
			t.Fatalf("journaled tool calls after the refusal = %d, want %d", got, len(crashCalls))
		}
	})
	t.Run("tool calls fit exactly", func(t *testing.T) {
		result, f := resumeWith(t, agent.Budget{MaxToolCalls: 5})
		if result.StopReason != agent.StopCompleted || f.CallCount() != 2 || result.Usage.ToolCalls != 5 || logicalUsage(t).ToolCalls != 5 {
			t.Fatalf("result = %+v calls = %d, want both calls run and the run completed", result, f.CallCount())
		}
	})
}
