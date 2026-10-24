package tempest

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
)

func conversationSummaryHistory() []types.Message {
	return []types.Message{
		types.UserMessage{Content: types.StringContent(strings.Repeat("old question ", 100)), Timestamp: 1},
		types.AssistantMessage{Content: []types.ContentBlock{types.NewText(strings.Repeat("old answer ", 100))}, Timestamp: 2},
		types.UserMessage{Content: types.StringContent(strings.Repeat("second question ", 100)), Timestamp: 3},
		types.AssistantMessage{Content: []types.ContentBlock{types.NewText("second answer")}, Timestamp: 4},
		types.UserMessage{Content: types.StringContent("continue here"), Timestamp: 5},
	}
}

func TestNativeBackgroundSummaryChargesLiveRootBudget(t *testing.T) {
	for _, test := range []struct {
		name   string
		budget float64
		status string
		calls  int
		cost   float64
	}{{"charge_live_root", 1, "completed", 2, 1.1}, {"deny_exhausted_root", .3, "parked", 1, .4}} {
		t.Run(test.name, func(t *testing.T) {
			provider := faux.New(faux.Options{})
			provider.SetResponses(
				faux.RespondMessage(types.AssistantMessage{Content: []types.ContentBlock{types.NewToolCall("held", "hold", json.RawMessage(`{}`))}, StopReason: types.StopToolUse, Usage: types.Usage{Cost: types.Cost{Total: .4}}}),
				faux.RespondMessage(types.AssistantMessage{Content: []types.ContentBlock{types.NewText("paid summary")}, StopReason: types.StopStop, Usage: types.Usage{Cost: types.Cost{Total: .7}}}),
				faux.Respond(faux.Text("must not spend again")),
			)
			streamFn := func(ctx context.Context, model types.Model, c types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
				upstream := provider.StreamSimple(ctx, model, c, options)
				out := stream.NewAssistantStream(model.API, model.Provider, model.ID)
				go func() {
					for range upstream.Events() {
					}
					message, err := upstream.Result(ctx)
					if err != nil {
						out.Push(types.StreamEvent{Type: types.EvError, Err: &types.AssistantMessage{StopReason: types.StopError, ErrorMessage: err.Error()}})
						return
					}
					message.Usage.Cost = types.Cost{Total: .4}
					if strings.Contains(c.SystemPrompt, "summar") {
						message.Usage.Cost.Total = .7
					}
					out.Push(types.StreamEvent{Type: types.EvStart, Partial: message})
					out.Push(types.StreamEvent{Type: types.EvDone, Reason: message.StopReason, Message: message})
				}()
				return out
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			hold := Tool{Tool: types.Tool{Name: "hold", Parameters: json.RawMessage(`{"type":"object"}`)}, Execute: func(ctx context.Context, _ string, _ json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
				return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("held result")}}, nil
			}}
			owner, err := Open(t.Context(), Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), New: &NewSession{Tools: []Tool{hold}, PermissionMode: permission.ModeBypass, Budget: Budget{MaxCostUSD: test.budget}}, Stack: &StackConfig{Model: provider.Model(), StreamFn: streamFn, Compaction: &CompactionSettings{KeepRecentTokens: 20}}})
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close(context.Background())
			owner.core.agent.SetMessages(conversationSummaryHistory())
			run, err := owner.Begin(t.Context(), Prompt{Text: "run held tool"})
			if err != nil {
				t.Fatal(err)
			}
			<-entered
			task, err := owner.Compact(t.Context(), CompactOptions{Wait: true})
			if test.status == "parked" {
				if err == nil || !strings.Contains(err.Error(), "provider budget: max_cost") {
					t.Fatal("summary bypassed exhausted live budget", task, err)
				}
			} else if err != nil || task.Status != test.status {
				t.Fatal(task, err)
			}
			beforeFinish, err := owner.core.store.Load(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			logicalUsage, _, _ := beforeFinish.LogicalUsage(owner.core.agent.RunState().LogicalRunID)
			if math.Abs(logicalUsage.Cost.Total-test.cost) > 1e-9 {
				t.Fatal("a crash before run_finished would forget the summary spend", logicalUsage)
			}
			once.Do(func() { close(release) })
			result, err := run.Wait()
			if err != nil || result.Err != nil || result.StopReason != agent.StopMaxCost || provider.CallCount() != test.calls || math.Abs(result.Usage.Cost.Total-test.cost) > 1e-9 {
				t.Fatalf("result=%+v err=%v calls=%d", result, err, provider.CallCount())
			}
			loaded, err := owner.core.store.Load(t.Context())
			if err != nil || math.Abs(loaded.UsageTotal.Cost.Total-test.cost) > 1e-9 {
				t.Fatal("summary receipt wasn't counted once", loaded, err)
			}
		})
	}
}

func TestContinueAndResumePendingDoNotWaitForNativeSummary(t *testing.T) {
	for _, operation := range []string{"resume_pending", "begin_continue"} {
		t.Run(operation, func(t *testing.T) {
			provider := faux.New(faux.Options{})
			provider.SetResponses(faux.Respond(faux.Text("foreground complete")), faux.Respond(faux.Text("paid summary")))
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			streamFn := func(ctx context.Context, model types.Model, c types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
				if strings.Contains(c.SystemPrompt, "summar") {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
				return provider.StreamSimple(ctx, model, c, options)
			}
			owner, err := Open(t.Context(), Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), New: &NewSession{}, Stack: &StackConfig{Model: provider.Model(), StreamFn: streamFn, Compaction: &CompactionSettings{KeepRecentTokens: 20}}})
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close(context.Background())
			messages := conversationSummaryHistory()
			owner.core.agent.SetMessages(messages)
			if _, err := owner.core.compaction.Request(t.Context(), messages); err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() {
				if operation == "resume_pending" {
					finished <- owner.ResumePending(t.Context())
					return
				}
				run, err := owner.BeginContinue(t.Context())
				if err == nil {
					result, waitErr := run.Wait()
					err = waitErr
					if err == nil {
						err = result.Err
					}
				}
				finished <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("summary wasn't resumed")
			}
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal(operation + " waited for held background summary")
			}
			once.Do(func() { close(release) })
		})
	}
}
