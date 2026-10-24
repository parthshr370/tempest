package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hooks"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/work"
)

func TestPreCompactReusesHookReceiptAndNeverRetriesUnknownOutcome(t *testing.T) {
	cwd := t.TempDir()
	runner, err := hooks.New(hooks.Settings{Hooks: map[string][]hooks.Group{"PreCompact": {{Hooks: []hooks.Command{{Type: "command", Command: `printf x >> calls; printf '%s' '{"hookSpecificOutput":{"additionalContext":"compact advice"}}'`}}}}}}, hooks.Input{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	store := queueStore(t)
	f := faux.New(faux.Options{})
	a := agent.NewAgent(agent.AgentOptions{})
	r := newCompactionRunner(f.Model(), f.StreamSimple)
	r.configureJournal(store, nil, a)
	r.hooks = runner
	source := strings.Repeat("a", 64)
	first, err := r.preCompactHook(context.Background(), source)
	if err != nil || len(first.Context) != 1 || first.Context[0] != "compact advice" {
		t.Fatalf("%+v %v", first, err)
	}
	loaded, err := journal.Load(context.Background(), store.Path())
	if err != nil {
		t.Fatal(err)
	}
	resumed := newCompactionRunner(f.Model(), f.StreamSimple)
	resumed.configureJournal(store, loaded, a)
	resumed.hooks = runner
	second, err := resumed.preCompactHook(context.Background(), source)
	if err != nil || len(second.Context) != 1 || second.Context[0] != "compact advice" {
		t.Fatalf("%+v %v", second, err)
	}
	unknown := strings.Repeat("b", 64)
	if err := store.Append(context.Background(), journal.HookRecord{Event: "PreCompact", Key: "precompact:" + unknown, Status: "started"}); err != nil {
		t.Fatal(err)
	}
	loaded, err = journal.Load(context.Background(), store.Path())
	if err != nil {
		t.Fatal(err)
	}
	resumed.configureJournal(store, loaded, a)
	if _, err := resumed.preCompactHook(context.Background(), unknown); err == nil || !strings.Contains(err.Error(), "outcome_unknown") {
		t.Fatalf("unknown hook was retried: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cwd, "calls"))
	if err != nil || string(data) != "x" {
		t.Fatalf("compaction hook reran: %s %v", data, err)
	}
}

func TestQueuedPromptHookReceiptSurvivesRecoveryBeforeConsumption(t *testing.T) {
	for _, settled := range []bool{true, false} {
		t.Run(map[bool]string{true: "settled", false: "unknown"}[settled], func(t *testing.T) {
			ctx := context.Background()
			cwd := t.TempDir()
			store := queueStore(t)
			queues := NewDurableQueues(store, nil, nil)
			item, err := queues.Enqueue(ctx, work.QueueInput{Kind: work.Steer, Key: "prompt", Text: "queued prompt"})
			if err != nil {
				t.Fatal(err)
			}
			key := "prompt:" + item.ID
			settings := hooks.Settings{Hooks: map[string][]hooks.Group{"UserPromptSubmit": {{Hooks: []hooks.Command{{Type: "command", Command: `printf x >> calls; printf '%s' '{"hookSpecificOutput":{"additionalContext":"queue advice"}}'`}}}}}}
			runner, err := hooks.New(settings, hooks.Input{Cwd: cwd})
			if err != nil {
				t.Fatal(err)
			}
			runner.Once = newHookCache(store, nil, nil).run
			if settled {
				if _, err := runner.Run(ctx, hooks.Input{Key: key, Event: "UserPromptSubmit", Prompt: "queued prompt"}); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := store.Append(ctx, journal.HookRecord{Event: "UserPromptSubmit", Key: key, Status: "started"}); err != nil {
					t.Fatal(err)
				}
			}
			loaded, err := journal.Load(ctx, store.Path())
			if err != nil {
				t.Fatal(err)
			}
			recoveredQueues := NewDurableQueues(store, loaded, nil)
			recoveredRunner, err := hooks.New(settings, hooks.Input{Cwd: cwd})
			if err != nil {
				t.Fatal(err)
			}
			f := faux.New(faux.Options{})
			f.SetResponses(func(c types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
				raw, _ := json.Marshal(c.Messages)
				if !strings.Contains(string(raw), "queue advice") {
					t.Errorf("receipt context lost: %s", raw)
				}
				return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("done")}, StopReason: types.StopStop}, nil
			})
			a := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: f.Model()}, StreamFn: f.StreamSimple, QueueSource: recoveredQueues, Hooks: recoveredRunner})
			recoveredRunner.Once = newHookCache(store, loaded, a).run
			detach := AttachJournalWithOptions(a, store, RecordOptions{Queues: recoveredQueues})
			defer detach()
			result, err := a.Continue(ctx)
			if err != nil {
				t.Fatal(err)
			}
			data, readErr := os.ReadFile(filepath.Join(cwd, "calls"))
			if settled {
				if result.Err != nil || string(data) != "x" || readErr != nil {
					t.Fatalf("settled hook repeated or context lost: result=%+v calls=%s err=%v", result, data, readErr)
				}
			} else {
				if result.Err != nil || result.StopReason != agent.StopCompleted || f.CallCount() != 0 || !os.IsNotExist(readErr) {
					t.Fatalf("unknown hook reran or blocked run: result=%+v calls=%s err=%v", result, data, readErr)
				}
				reloaded, err := journal.Load(ctx, store.Path())
				if err != nil || len(recoveredQueues.Snapshot()) != 0 || !reloaded.QueueConsumed[item.ID] {
					t.Fatalf("interrupted prompt not consumed: loaded=%+v err=%v", reloaded, err)
				}
				raw, _ := json.Marshal(reloaded.Messages)
				if strings.Contains(string(raw), "queued prompt") || strings.Contains(string(raw), "outcome unknown") {
					t.Fatalf("rejected prompt leaked into context: %s", raw)
				}
				f.SetResponses(faux.Respond(faux.Text("next prompt works")))
				next, err := a.PromptText(ctx, "fresh prompt")
				if err != nil || next.Err != nil || f.CallCount() != 1 {
					t.Fatalf("session poisoned: %+v err=%v", next, err)
				}
			}
		})
	}
}

func TestQueuedPromptCancellationConsumesWithoutSubmission(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(map[bool]string{true: "before", false: "during"}[before], func(t *testing.T) {
			cwd := t.TempDir()
			store := queueStore(t)
			queue := NewDurableQueues(store, nil, nil)
			item, err := queue.Enqueue(context.Background(), work.QueueInput{Kind: work.Steer, Text: "must not reach provider"})
			if err != nil {
				t.Fatal(err)
			}
			runner, err := hooks.New(hooks.Settings{Hooks: map[string][]hooks.Group{"UserPromptSubmit": {{Hooks: []hooks.Command{{Type: "command", Command: "touch started; sleep 10"}}}}}}, hooks.Input{Cwd: cwd})
			if err != nil {
				t.Fatal(err)
			}
			f := faux.New(faux.Options{})
			f.SetResponses(faux.Respond(faux.Text("done")))
			a := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: f.Model()}, StreamFn: f.StreamSimple, QueueSource: queue, Hooks: runner})
			runner.Once = newHookCache(store, nil, a).run
			detach := AttachJournalWithOptions(a, store, RecordOptions{Queues: queue})
			defer detach()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if before {
				cache := newHookCache(store, nil, a)
				runner.Once = func(callCtx context.Context, key, event string, call func(context.Context) (hooks.Result, error)) (hooks.Result, error) {
					cancel()
					return cache.run(callCtx, key, event, call)
				}
			} else {
				go func() {
					for {
						if _, err := os.Stat(filepath.Join(cwd, "started")); err == nil {
							cancel()
							return
						}
						select {
						case <-ctx.Done():
							return
						case <-time.After(time.Millisecond):
						}
					}
				}()
			}
			result, err := a.Continue(ctx)
			if err != nil || result.Err != nil || result.StopReason != agent.StopAborted || f.CallCount() != 0 {
				t.Fatalf("cancelled result=%+v err=%v", result, err)
			}
			loaded, err := journal.Load(context.Background(), store.Path())
			if err != nil || !loaded.QueueConsumed[item.ID] || len(queue.Snapshot()) != 0 {
				t.Fatalf("cancel left poison queue: %+v %v", loaded, err)
			}
			if before {
				if _, ok := loaded.HookRuns["prompt:"+item.ID]; ok {
					t.Fatal("recorded started before a command was ready")
				}
			}
		})
	}
}
