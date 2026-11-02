package session

import (
	"context"
	"os"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/retry"
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

// A failed attempt that reported usage without a message_start is
// journaled as an errored message with its own usage receipt, charged once,
// and counts against the caps before the request is sent again.
func TestFailedAttemptWithoutStartIsJournaledOnce(t *testing.T) {
	for _, test := range []struct {
		name      string
		budget    agent.Budget
		wantStop  agent.StopReason
		wantSent  int
		wantInput int
	}{
		{"uncapped", agent.Budget{}, agent.StopCompleted, 2, 200},
		{"cap reached by the failed attempt", agent.Budget{MaxInputTokens: 100}, agent.StopMaxInputTokens, 1, 100},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := queueStore(t)
			var sent atomic.Int32
			provider := func(_ context.Context, model types.Model, _ types.Context, _ *types.SimpleStreamOptions) *stream.AssistantStream {
				out := stream.NewAssistantStream(model.API, model.Provider, model.ID)
				if sent.Add(1) == 1 {
					out.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopError, Err: &types.AssistantMessage{API: model.API, Provider: model.Provider, Model: model.ID, StopReason: types.StopError, ErrorCode: string(retry.CodeStreamInterrupted), ErrorMessage: "stream dropped", Usage: types.Usage{Input: 100, TotalTokens: 100}}})
				} else {
					answer := types.AssistantMessage{API: model.API, Provider: model.Provider, Model: model.ID, Content: []types.ContentBlock{types.NewText("done")}, StopReason: types.StopStop, Usage: types.Usage{Input: 100, Output: 5, TotalTokens: 105}}
					out.Push(types.StreamEvent{Type: types.EvStart, Partial: &answer})
					out.Push(types.StreamEvent{Type: types.EvDone, Reason: types.StopStop, Message: &answer})
				}
				out.End()
				return out
			}
			policy := retry.DefaultConfig()
			policy.MaxAttempts = 2
			policy.Sleeper = noRetryDelay{}
			retrier, err := retry.New(retry.StreamFunc(provider), policy)
			if err != nil {
				t.Fatal(err)
			}
			model := types.Model{API: "test", Provider: "test", ID: "m", ContextWindow: 1000}
			a := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: model}, StreamFn: retrier.Stream, SessionID: store.ID(), Budget: test.budget})
			defer AttachJournal(a, store)()
			result, err := a.PromptText(ctx, "go")
			if err != nil || result.Err != nil || result.StopReason != test.wantStop || int(sent.Load()) != test.wantSent {
				t.Fatalf("result = %+v after %d attempts, err = %v; want %q after %d", result, sent.Load(), err, test.wantStop, test.wantSent)
			}
			loaded, err := journal.Load(ctx, store.Path())
			if err != nil {
				t.Fatal(err)
			}
			assistants := 0
			for _, message := range loaded.Messages {
				if _, ok := message.(types.AssistantMessage); ok {
					assistants++
				}
			}
			if loaded.UsageTotal.Input != test.wantInput || result.Usage.Input != test.wantInput || assistants != test.wantSent {
				t.Fatalf("journaled input = %d, run input = %d, assistant messages = %d; want %d, %d, %d", loaded.UsageTotal.Input, result.Usage.Input, assistants, test.wantInput, test.wantInput, test.wantSent)
			}
		})
	}
}
