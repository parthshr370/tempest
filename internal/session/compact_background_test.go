package session

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/compaction"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/taskstate"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

func backgroundFixture(t *testing.T, store *journal.Store, loaded *journal.Loaded, streamFn agent.StreamFn) (*compactionRunner, *taskstate.Runtime) {
	t.Helper()
	provider := faux.New(faux.Options{})
	model := provider.Model()
	model.ContextWindow = 100000
	runner := newCompactionRunner(model, streamFn)
	runner.settings = compaction.CompactionSettings{Enabled: true, ReserveTokens: 1000, KeepRecentTokens: 20, SoftThresholdTokens: 1}
	runner.configureJournal(store, loaded, agent.NewAgent(agent.AgentOptions{}))
	runtime, err := taskstate.New(t.Context(), taskstate.Options{SessionID: store.ID(), Store: store, Resolve: func(string) (taskstate.Definition, error) { return NativeCompactionDefinition(), nil }, Invoke: func(ctx context.Context, _ taskstate.Definition, task extension.Task, abort bool) (extension.TaskDecision, error) {
		return runner.InvokeTask(ctx, task, abort)
	}})
	if err != nil {
		t.Fatal(err)
	}
	runner.BindTasks(runtime, nil)
	t.Cleanup(func() {
		if err := runtime.Suspend(context.Background()); err != nil {
			t.Error(err)
		}
		if err := runner.Close(); err != nil {
			t.Error(err)
		}
	})
	return runner, runtime
}

func summaryMessages() []types.Message {
	return []types.Message{cUser(strings.Repeat("old first ", 100)), cAssistant(strings.Repeat("old answer ", 100)), cUser(strings.Repeat("second ", 100)), cAssistant(strings.Repeat("second answer ", 100)), cUser(strings.Repeat("recent question ", 10)), cAssistant("recent answer")}
}

func TestBackgroundCompactionPreservesTailAndPlacesOnce(t *testing.T) {
	store := queueStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.Text("paid summary")))
	streamFn := func(ctx context.Context, model types.Model, c types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return provider.StreamSimple(ctx, model, c, options)
	}
	runner, _ := backgroundFixture(t, store, nil, streamFn)
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
	tail := []types.Message{cUser("tail appended while summary waits"), cAssistant("tail answer")}
	current := append(append([]types.Message(nil), messages...), tail...)
	for _, message := range tail {
		if err := store.AppendMessage(t.Context(), message); err != nil {
			t.Fatal(err)
		}
	}
	finished := make(chan error, 1)
	go func() {
		_, changed, err := runner.backgroundTurn(t.Context(), agent.ShouldStopAfterTurnContext{Context: agent.AgentContext{Messages: current}})
		if changed && err == nil {
			err = context.Canceled
		}
		finished <- err
	}()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("foreground blocked below hard threshold")
	}
	releaseOnce.Do(func() { close(release) })
	task, err = runner.Wait(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	replacement, changed, err := runner.Place(t.Context(), current, task)
	if err != nil || !changed {
		t.Fatal(changed, err, task)
	}
	if !reflect.DeepEqual(replacement[len(replacement)-len(tail):], tail) {
		t.Fatal("appended tail changed")
	}
	loaded, err := store.Load(t.Context())
	if err != nil || !reflect.DeepEqual(loaded.Messages, replacement) {
		t.Fatal("placement isn't durable", err)
	}
	if _, changed, err := runner.Place(t.Context(), replacement, task); err != nil || changed {
		t.Fatal("cut applied twice", changed, err)
	}
	records, err := store.Read(t.Context(), durable.Query{Kind: durable.EntryKind, Namespace: "_tempest/journal", Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	cuts := 0
	for _, record := range records.Records {
		var envelope struct{ Type string }
		if err := json.Unmarshal(record.Value, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Type == "compaction" {
			cuts++
		}
	}
	if cuts != 1 {
		t.Fatal("placements", cuts)
	}
}

func TestBackgroundCompactionStaleAfterResetOrRewind(t *testing.T) {
	for _, control := range []string{"reset", "rewind"} {
		t.Run(control, func(t *testing.T) {
			store := queueStore(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			provider := faux.New(faux.Options{})
			provider.SetResponses(faux.Respond(faux.Text("stale paid summary")))
			runner, _ := backgroundFixture(t, store, nil, func(ctx context.Context, model types.Model, c types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
				return provider.StreamSimple(ctx, model, c, options)
			})
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
			if control == "reset" {
				err = store.Append(t.Context(), journal.ContextHeadEntry{Reason: "reset"})
			} else {
				var entry journal.RewindEntry
				entry, err = journal.NewRewindEntry("checkpoint", "", nil)
				if err == nil {
					err = store.Append(t.Context(), entry)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			current := []types.Message{cUser("new epoch")}
			if err := store.AppendMessage(t.Context(), current[0]); err != nil {
				t.Fatal(err)
			}
			releaseOnce.Do(func() { close(release) })
			task, err = runner.Wait(t.Context(), task.ID)
			if err != nil {
				t.Fatal(err)
			}
			replacement, changed, err := runner.Place(t.Context(), current, task)
			if err != nil || changed || !reflect.DeepEqual(replacement, current) {
				t.Fatal("stale summary crossed context", changed, err)
			}
			loaded, err := store.Load(t.Context())
			if err != nil || !reflect.DeepEqual(loaded.Messages, current) {
				t.Fatal(loaded, err)
			}
		})
	}
}

func TestNativeCompactionRestartReusesPaidPhase(t *testing.T) {
	store := queueStore(t)
	provider := faux.New(faux.Options{})
	var calls atomic.Int32
	streamFn := func(ctx context.Context, model types.Model, c types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
		calls.Add(1)
		return provider.StreamSimple(ctx, model, c, options)
	}
	provider.SetResponses(faux.Respond(faux.Text("durable paid summary")))
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
	if _, err := runner.InvokeTask(t.Context(), task, false); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Suspend(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	resumed, _ := backgroundFixture(t, store, loaded, streamFn)
	if err := resumed.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	task, err = resumed.Wait(t.Context(), task.ID)
	if err != nil || task.Status != "completed" || calls.Load() != 1 {
		t.Fatal("paid summary repeated", task, err, calls.Load())
	}
}
