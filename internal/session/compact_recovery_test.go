package session

import (
	"context"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest/durable"
)

func TestFailedOrSuspendedSummaryReleasesCutAcrossRestart(t *testing.T) {
	for _, failure := range []string{"provider", "suspend", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			store := queueStore(t)
			entered := make(chan struct{}, 1)
			provider := faux.New(faux.Options{})
			provider.SetResponses(faux.Respond(faux.Text("fresh summary")))
			var calls atomic.Int32
			streamFn := func(ctx context.Context, model types.Model, c types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
				if calls.Add(1) == 1 {
					entered <- struct{}{}
					if failure != "provider" {
						<-ctx.Done()
					}
					out := stream.NewAssistantStream(model.API, model.Provider, model.ID)
					out.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopError, Err: &types.AssistantMessage{StopReason: types.StopError, ErrorMessage: "summary unavailable", Usage: types.Usage{Input: 7, TotalTokens: 7}}})
					out.End()
					return out
				}
				return provider.StreamSimple(ctx, model, c, options)
			}
			runner, runtime := backgroundFixture(t, store, nil, streamFn)
			messages := summaryMessages()
			for _, message := range messages {
				if err := store.AppendMessage(t.Context(), message); err != nil {
					t.Fatal(err)
				}
			}
			task, err := runner.Request(t.Context(), messages)
			if err != nil {
				t.Fatal(err)
			}
			runner.Start(task.ID)
			<-entered
			if failure == "provider" {
				if _, err := runner.Wait(t.Context(), task.ID); err == nil {
					t.Fatal("provider failure hidden")
				}
			} else if failure == "suspend" {
				if err := runtime.Suspend(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := runner.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if err := runtime.Suspend(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := runner.Close(); err != nil {
				t.Fatal(err)
			}
			path := store.Path()
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, loaded, err := journal.OpenForResume(t.Context(), path, journal.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if loaded.UsageTotal.TotalTokens != 7 {
				t.Fatal("interrupted summary lost known usage", loaded.UsageTotal)
			}
			for _, part := range loaded.CompactionParts {
				if failure != "provider" && part.Finished != nil {
					t.Fatal("cancellation stored as final failure", part.Finished)
				}
			}
			resumed, _ := backgroundFixture(t, reopened, loaded, streamFn)
			if err := resumed.Recover(t.Context()); err != nil {
				t.Fatal(err)
			}
			if failure != "provider" {
				if _, err := resumed.Wait(t.Context(), task.ID); err == nil || !strings.Contains(err.Error(), "outcome_unknown") {
					t.Fatal("unknown cut must not be repaid", err)
				}
			}
			if calls.Load() != 1 {
				t.Fatal("same cut paid again", calls.Load())
			}
			old, err := resumed.background.tasks.Get(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, changed, err := resumed.Place(t.Context(), messages, old); err != nil || changed {
				t.Fatal("failed placement poisoned context", changed, err)
			}
			if disposed, err := resumed.disposed(t.Context(), task.ID); err != nil || !disposed {
				t.Fatal("missing durable disposition", disposed, err)
			}
			if err := resumed.Recover(t.Context()); err != nil {
				t.Fatal(err)
			}
			resumed.background.mu.Lock()
			active := resumed.background.active
			resumed.background.mu.Unlock()
			if active != "" {
				t.Fatal("retired cut recovered", active)
			}
			grown := append(append([]types.Message(nil), messages...), cUser("more input"), cAssistant("more answer"))
			for _, message := range grown[len(messages):] {
				if err := reopened.AppendMessage(t.Context(), message); err != nil {
					t.Fatal(err)
				}
			}
			next, err := resumed.Request(t.Context(), grown)
			if err != nil || next.ID == task.ID {
				t.Fatal("dead cut coalesced", next, err)
			}
			next, err = resumed.Wait(t.Context(), next.ID)
			if err != nil || next.Status != "completed" {
				t.Fatal(next, err)
			}
			if _, changed, err := resumed.Place(t.Context(), grown, next); err != nil || !changed || calls.Load() != 2 {
				t.Fatal("new cut didn't recover", changed, err, calls.Load())
			}
		})
	}
}

func TestAbortedCompactionPlacementIsHarmless(t *testing.T) {
	store := queueStore(t)
	provider := faux.New(faux.Options{})
	runner, runtime := backgroundFixture(t, store, nil, provider.StreamSimple)
	messages := summaryMessages()
	task, err := runner.Request(t.Context(), messages)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Abort(t.Context(), task.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Wait(t.Context(), task.ID); err == nil {
		t.Fatal("abort must be visible to waiter")
	}
	task, err = runtime.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		got, changed, err := runner.Place(t.Context(), messages, task)
		if err != nil || changed || !reflect.DeepEqual(got, messages) {
			t.Fatal(changed, err)
		}
	}
	runner.settings.Enabled = false
	if _, _, err := runner.backgroundTurn(t.Context(), agent.ShouldStopAfterTurnContext{Context: agent.AgentContext{Messages: messages}}); err != nil {
		t.Fatal(err)
	}
}

func TestDisabledOverflowDoesNotClaimOrSummarize(t *testing.T) {
	store := queueStore(t)
	provider := faux.New(faux.Options{})
	runner, _ := backgroundFixture(t, store, nil, provider.StreamSimple)
	runner.settings.Enabled = false
	if err := store.AppendMessage(t.Context(), cUser("input")); err != nil {
		t.Fatal(err)
	}
	got, changed, err := runner.recoverOverflow(t.Context(), summaryMessages())
	if err != nil || changed || len(got) == 0 || provider.CallCount() != 0 {
		t.Fatal(changed, err)
	}
	page, err := store.Read(t.Context(), durable.Query{Kind: durable.DocumentKind, Namespace: "_tempest/overflow"})
	if err != nil || len(page.Records) != 0 {
		t.Fatal("disabled overflow consumed retry", page, err)
	}
}
