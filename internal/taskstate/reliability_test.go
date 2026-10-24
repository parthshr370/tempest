package taskstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

func TestRejectedDecisionsParkOriginalPhase(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		decision extension.TaskDecision
		reason   string
	}{
		{"missing_wait", extension.TaskDecision{Kind: "wait", Wait: []string{"missing"}}, "missing wait"},
		{"self_wait", extension.TaskDecision{Kind: "wait", Wait: []string{"root"}}, "cycle"},
		{"existing_child", extension.TaskDecision{Kind: "advance", Children: []extension.TaskRequest{{ID: "root", Definition: "demo/flow", Input: json.RawMessage(`{}`)}}}, "child ID"},
		{"duplicate_child", extension.TaskDecision{Kind: "advance", Children: []extension.TaskRequest{{ID: "child", Definition: "demo/flow", Input: json.RawMessage(`{}`)}, {ID: "child", Definition: "demo/flow", Input: json.RawMessage(`{}`)}}}, "child ID"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			calls := 0
			definition := taskDefinition(func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
				calls++
				decision := scenario.decision
				decision.Phase, decision.Checkpoint = "rejected", json.RawMessage(`{"rejected":true}`)
				return decision, nil
			})
			runtime := newTestRuntime(t, durable.NewMemory(), nil, map[string]extension.TaskDefinition{"demo/flow": definition}, nil)
			admit(t, runtime, "root", "demo/flow")
			for range 3 {
				if err := runtime.Resume(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			task, err := runtime.Get("root")
			if err != nil || task.Status != "parked" || task.Crashes != 0 || calls != 1 || !strings.Contains(task.Reason, scenario.reason) || task.Phase != "run" || string(task.Checkpoint) != `{}` {
				t.Fatalf("calls=%d task=%+v err=%v", calls, task, err)
			}
		})
	}
}

func TestSubmissionKeyConflictParksWithoutCrash(t *testing.T) {
	calls := 0
	definition := taskDefinition(func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		calls++
		return extension.TaskDecision{Kind: "advance", Submissions: []extension.TaskSubmission{{Key: "delivery", Text: fmt.Sprint(calls)}}}, nil
	})
	runtime := newTestRuntime(t, durable.NewMemory(), nil, map[string]extension.TaskDefinition{"demo/flow": definition}, nil)
	admit(t, runtime, "root", "demo/flow")
	if err := runtime.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	task, _ := runtime.Get("root")
	if task.Status != "parked" || task.Crashes != 0 || calls != 2 || !strings.Contains(task.Reason, "submission key") {
		t.Fatalf("calls=%d task=%+v", calls, task)
	}
}

func TestDecisionCASReleasesAttempt(t *testing.T) {
	store := durable.NewMemory()
	calls := 0
	definition := taskDefinition(func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		calls++
		revision := uint64(1)
		if calls > 1 {
			revision = 0
		}
		return extension.TaskDecision{Kind: "complete", Commit: durable.Commit{Expected: []durable.RevisionCheck{{SessionID: "session", Kind: durable.DocumentKind, Namespace: "app", ID: "doc", Revision: revision}}, Changes: []durable.Change{{SessionID: "session", Kind: durable.DocumentKind, Namespace: "app", ID: "doc", Version: 1, Value: json.RawMessage(`{}`)}}}}, nil
	})
	runtime := newTestRuntime(t, store, nil, map[string]extension.TaskDefinition{"demo/flow": definition}, nil)
	resolve := runtime.options.Resolve
	runtime.options.Resolve = func(name string) (Definition, error) {
		definition, err := resolve(name)
		definition.Namespaces = []string{"app"}
		return definition, err
	}
	admit(t, runtime, "root", "demo/flow")
	if err := runtime.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	task, _ := runtime.Get("root")
	if task.Status != "suspended" || task.Crashes != 0 || calls != 1 {
		t.Fatalf("calls=%d task=%+v", calls, task)
	}
	if err := runtime.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	task, _ = runtime.Get("root")
	if task.Status != "completed" || task.Crashes != 0 || calls != 2 {
		t.Fatalf("calls=%d task=%+v", calls, task)
	}
}

func TestCallbackCommitKeepsPhaseAndVerifiesReplay(t *testing.T) {
	store := durable.NewMemory()
	calls := 0
	var runtime *Runtime
	definition := taskDefinition(func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		return extension.TaskDecision{}, nil
	})
	var first durable.Receipt
	runtime = newTestRuntime(t, store, nil, map[string]extension.TaskDefinition{"demo/flow": definition}, func(ctx context.Context, _ Definition, task extension.Task, _ bool) (extension.TaskDecision, error) {
		calls++
		batch := durable.Commit{ID: "callback", Changes: []durable.Change{{SessionID: "session", Kind: durable.DocumentKind, Namespace: "app", ID: "doc", Version: 1, Value: json.RawMessage(`{"saved":true}`)}}}
		receipt, err := runtime.FencedCommit(ctx, task, batch)
		if err != nil {
			return extension.TaskDecision{}, err
		}
		if calls == 1 {
			first = receipt
		}
		if receipt != first {
			return extension.TaskDecision{}, errors.New("callback replay changed receipt")
		}
		current, err := runtime.Get(task.ID)
		if err != nil || current.Revision != task.Revision {
			return extension.TaskDecision{}, errors.New("callback advanced task revision")
		}
		if calls == 1 {
			return extension.TaskDecision{Kind: "advance", Phase: "finish"}, nil
		}
		batch.Changes[0].Value = json.RawMessage(`{"changed":true}`)
		if _, err := runtime.FencedCommit(ctx, task, batch); !errors.Is(err, durable.ErrConflict) {
			return extension.TaskDecision{}, fmt.Errorf("changed callback accepted: %v", err)
		}
		return extension.TaskDecision{Kind: "complete"}, nil
	})
	admit(t, runtime, "root", "demo/flow")
	if err := runtime.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	task, _ := runtime.Get("root")
	if task.Status != "completed" || calls != 2 {
		t.Fatalf("calls=%d task=%+v", calls, task)
	}
}

func TestRepeatedAbortPreservesCompensationBackoff(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	deadline := now.Add(time.Hour)
	var phases []string
	definition := taskDefinition(func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		return extension.TaskDecision{Kind: "sleep", WakeAt: &deadline}, nil
	})
	definition.Abort = func(_ context.Context, task extension.Task, _ extension.Invocation) (extension.TaskDecision, error) {
		phases = append(phases, task.Phase)
		if task.Phase == "abort" {
			return extension.TaskDecision{Kind: "sleep", Phase: "refund", WakeAt: &deadline, Checkpoint: json.RawMessage(`{"issued":true}`)}, nil
		}
		return extension.TaskDecision{Kind: "complete"}, nil
	}
	runtime := newTestRuntime(t, durable.NewMemory(), func() time.Time { return now }, map[string]extension.TaskDefinition{"demo/flow": definition}, nil)
	admit(t, runtime, "root", "demo/flow")
	if err := runtime.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Abort(t.Context(), "root", "cancelled"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, _ := runtime.Get("root")
	if err := runtime.Abort(t.Context(), "root", "cancel_all"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, _ := runtime.Get("root")
	if after.Revision != before.Revision || after.Phase != "refund" || after.WakeAt == nil || !after.WakeAt.Equal(deadline) || len(phases) != 1 {
		t.Fatalf("before=%+v after=%+v phases=%v", before, after, phases)
	}
	now = deadline
	if err := runtime.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, _ = runtime.Get("root")
	if after.Status != "aborted" || len(phases) != 2 || phases[1] != "refund" {
		t.Fatalf("task=%+v phases=%v", after, phases)
	}
}

func TestRetryOwnCleanupButNotCompletedOwner(t *testing.T) {
	failCleanup := true
	deadline := time.Now().Add(time.Hour)
	parentCalls := 0
	parent := taskDefinition(func(_ context.Context, task extension.Task, _ extension.Invocation) (extension.TaskDecision, error) {
		parentCalls++
		if task.Phase == "run" {
			return extension.TaskDecision{Kind: "wait", Phase: "charge", Wait: []string{"child"}, Children: []extension.TaskRequest{{ID: "child", Definition: "demo/child", Input: json.RawMessage(`{}`)}}}, nil
		}
		return extension.TaskDecision{Kind: "complete"}, nil
	})
	child := taskDefinition(func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		return extension.TaskDecision{Kind: "sleep", WakeAt: &deadline}, nil
	})
	child.Abort = func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		if failCleanup {
			return extension.TaskDecision{Kind: "complete", Error: "refund failed"}, nil
		}
		return extension.TaskDecision{Kind: "complete"}, nil
	}
	runtime := newTestRuntime(t, durable.NewMemory(), nil, map[string]extension.TaskDefinition{"demo/parent": parent, "demo/child": child}, nil)
	admit(t, runtime, "parent", "demo/parent")
	if err := runtime.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Abort(t.Context(), "child", "cancelled"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	owner, _ := runtime.Get("parent")
	if owner.Status != "cleanup_faulted" || parentCalls != 2 {
		t.Fatalf("owner=%+v calls=%d", owner, parentCalls)
	}
	if err := runtime.Retry(t.Context(), "parent"); err == nil {
		t.Fatal("retried completed owner")
	}
	failCleanup = false
	if err := runtime.Retry(t.Context(), "child"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	cleaned, _ := runtime.Get("child")
	if cleaned.Status != "aborted" || parentCalls != 2 {
		t.Fatalf("child=%+v parent calls=%d", cleaned, parentCalls)
	}
}

func TestPausedBlocksForegroundAndStopRunOnlyRecordsIntent(t *testing.T) {
	calls := 0
	definition := taskDefinition(func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		calls++
		return extension.TaskDecision{Kind: "complete"}, nil
	})
	definition.Abort = definition.Run
	runtime := newTestRuntime(t, durable.NewMemory(), nil, map[string]extension.TaskDefinition{"demo/flow": definition}, nil)
	if _, err := runtime.Create(t.Context(), extension.TaskRequest{ID: "root", Definition: "demo/flow", Input: json.RawMessage(`{}`), RunID: "run"}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.StopRun(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("StopRun executed compensation")
	}
	if err := runtime.Suspend(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.ResumeRun(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("foreground scope bypassed suspension")
	}
}

func TestSplitAbortIsImmediatelyDue(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	deadline := now.Add(time.Hour)
	for _, status := range []string{"sleeping", "waiting", "parked"} {
		wake, present := Alarm([]extension.Task{{ID: "owner", Status: "abort_requested"}, {ID: "child", Owner: "owner", Status: status, WakeAt: &deadline}}, now)
		if !present || !wake.Equal(now) {
			t.Fatalf("%s: wake=%v present=%v", status, wake, present)
		}
	}
}

func TestMigrationAndNativeJoinDoNotHoldGraphLock(t *testing.T) {
	for _, scenario := range []string{"migration", "native_join"} {
		t.Run(scenario, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			definition := taskDefinition(func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
				return extension.TaskDecision{Kind: "complete"}, nil
			})
			definitions := map[string]extension.TaskDefinition{"demo/flow": definition}
			runtime := newTestRuntime(t, durable.NewMemory(), nil, definitions, nil)
			admit(t, runtime, "root", "demo/flow")
			if scenario == "migration" {
				definition.Version = 2
				definition.Migrate = func(uint32, json.RawMessage) (json.RawMessage, error) {
					close(entered)
					<-release
					return json.RawMessage(`{}`), nil
				}
				definitions["demo/flow"] = definition
			} else {
				runtime.options.AbortOwned = func(context.Context, string) error { close(entered); <-release; return nil }
				if err := runtime.Abort(t.Context(), "root", "cancelled"); err != nil {
					t.Fatal(err)
				}
			}
			finished := make(chan error, 1)
			go func() { finished <- runtime.Resume(t.Context()) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("operation not entered")
			}
			read := make(chan struct{})
			go func() { runtime.Snapshot(); close(read) }()
			select {
			case <-read:
			case <-time.After(time.Second):
				t.Fatal("extension operation held graph lock")
			}
			releaseOnce.Do(func() { close(release) })
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
		})
	}
}

type lostAckAndLookup struct {
	Store
	lost, lookupFailed bool
}

func (store *lostAckAndLookup) Commit(ctx context.Context, batch durable.Commit) (durable.Receipt, error) {
	receipt, err := store.Store.Commit(ctx, batch)
	if err == nil && !store.lost && strings.HasPrefix(batch.ID, "task:decision:") {
		store.lost = true
		return durable.Receipt{}, errors.New("lost acknowledgment")
	}
	return receipt, err
}
func (store *lostAckAndLookup) Read(ctx context.Context, query durable.Query) (durable.Page, error) {
	if query.CommitID != "" && store.lost && !store.lookupFailed {
		store.lookupFailed = true
		return durable.Page{}, errors.New("lookup unavailable")
	}
	return store.Store.Read(ctx, query)
}

func TestLostAcknowledgmentAndLookupRecoverWithoutReopen(t *testing.T) {
	store := &lostAckAndLookup{Store: durable.NewMemory()}
	calls := 0
	definition := taskDefinition(func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		calls++
		return extension.TaskDecision{Kind: "complete"}, nil
	})
	runtime := newTestRuntime(t, store, nil, map[string]extension.TaskDefinition{"demo/flow": definition}, nil)
	admit(t, runtime, "root", "demo/flow")
	if err := runtime.Resume(t.Context()); err == nil {
		t.Fatal("lookup outage not reported")
	}
	if err := runtime.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	task, _ := runtime.Get("root")
	if task.Status != "completed" || calls != 1 {
		t.Fatalf("calls=%d task=%+v", calls, task)
	}
}

func TestDrainBudgetYieldsAndTerminalHistoryStaysInStore(t *testing.T) {
	calls := 0
	definition := taskDefinition(func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		calls++
		if calls < 1100 {
			return extension.TaskDecision{Kind: "advance"}, nil
		}
		return extension.TaskDecision{Kind: "complete"}, nil
	})
	runtime := newTestRuntime(t, durable.NewMemory(), nil, map[string]extension.TaskDefinition{"demo/flow": definition}, nil)
	admit(t, runtime, "root", "demo/flow")
	for calls < 1100 {
		if err := runtime.Resume(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err := runtime.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(runtime.tasks) != 0 || len(runtime.Snapshot()) != 0 {
		t.Fatal("terminal task retained in working set")
	}
	task, err := runtime.Get("root")
	if err != nil || task.Status != "completed" || task.Crashes != 0 {
		t.Fatalf("task=%+v err=%v", task, err)
	}
	if _, err := runtime.Create(t.Context(), extension.TaskRequest{ID: "root", Definition: "demo/flow", Input: json.RawMessage(`{}`), Background: true}); err != nil {
		t.Fatal(err)
	}
	reopened := newTestRuntime(t, runtime.options.Store, nil, map[string]extension.TaskDefinition{"demo/flow": definition}, nil)
	if len(reopened.tasks) != 0 {
		t.Fatal("recovery loaded terminal history into working set")
	}
}

func TestRepeatedAbortDoesNotCancelLiveCompensation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	cancelled := make(chan struct{}, 1)
	deadline := time.Now().Add(time.Hour)
	definition := taskDefinition(func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		return extension.TaskDecision{Kind: "sleep", WakeAt: &deadline}, nil
	})
	definition.Abort = func(ctx context.Context, _ extension.Task, _ extension.Invocation) (extension.TaskDecision, error) {
		close(entered)
		select {
		case <-ctx.Done():
			cancelled <- struct{}{}
			return extension.TaskDecision{}, ctx.Err()
		case <-release:
			return extension.TaskDecision{Kind: "complete"}, nil
		}
	}
	runtime := newTestRuntime(t, durable.NewMemory(), nil, map[string]extension.TaskDefinition{"demo/flow": definition}, nil)
	admit(t, runtime, "root", "demo/flow")
	if err := runtime.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Abort(t.Context(), "root", "first"); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- runtime.Resume(t.Context()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("compensation didn't start")
	}
	retryCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	retryErr := runtime.Abort(retryCtx, "root", "second")
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if retryErr != nil {
		t.Fatal(retryErr)
	}
	select {
	case <-cancelled:
		t.Fatal("second abort cancelled live compensation")
	default:
	}
}

func TestHistoricalTasksDoNotEnterValidationWorkingSet(t *testing.T) {
	const historicalTasks = 5000
	const batchSize = 1000
	store := durable.NewMemory()
	for batch := range historicalTasks / batchSize {
		commit := durable.Commit{ID: fmt.Sprintf("history:%d", batch)}
		for index := range batchSize {
			id := fmt.Sprintf("finished:%d", batch*batchSize+index)
			task := extension.Task{ID: id, SessionID: "session", Definition: "demo/flow", Version: 1, CodeRevision: "v1", Phase: "run", Status: "completed", Input: json.RawMessage(`{}`), Checkpoint: json.RawMessage(`{}`), Background: true, Revision: 1}
			raw, err := json.Marshal(task)
			if err != nil {
				t.Fatal(err)
			}
			commit.Changes = append(commit.Changes, durable.Change{SessionID: "session", Kind: durable.TaskKind, Namespace: Namespace, ID: id, Version: 1, Value: raw})
		}
		if _, err := store.Commit(t.Context(), commit); err != nil {
			t.Fatal(err)
		}
	}
	definition := taskDefinition(func(_ context.Context, task extension.Task, _ extension.Invocation) (extension.TaskDecision, error) {
		if task.Phase == "done" {
			return extension.TaskDecision{Kind: "complete"}, nil
		}
		return extension.TaskDecision{Kind: "wait", Wait: []string{"finished:4999"}, Phase: "done"}, nil
	})
	runtime := newTestRuntime(t, store, nil, map[string]extension.TaskDefinition{"demo/flow": definition}, nil)
	admit(t, runtime, "active", "demo/flow")
	if len(runtime.tasks) != 1 {
		t.Fatalf("working set=%d", len(runtime.tasks))
	}
	graph, err := runtime.graph(t.Context(), nil)
	if err != nil || len(graph) != 1 {
		t.Fatalf("validation graph=%d err=%v", len(graph), err)
	}
	dependency, err := runtime.Get("finished:4999")
	if err != nil || dependency.Status != "completed" {
		t.Fatalf("dependency=%+v err=%v", dependency, err)
	}
	if len(runtime.tasks) != 1 {
		t.Fatal("detail read retained historical task")
	}
	if err := runtime.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(runtime.tasks) != 0 {
		t.Fatal("wait on historical dependency didn't settle")
	}
}
