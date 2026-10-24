package taskstate

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

var objectSchema = json.RawMessage(`{"type":"object"}`)

func newTestRuntime(t *testing.T, store Store, now func() time.Time, definitions map[string]extension.TaskDefinition, invoke func(context.Context, Definition, extension.Task, bool) (extension.TaskDecision, error)) *Runtime {
	t.Helper()
	if invoke == nil {
		invoke = func(ctx context.Context, definition Definition, task extension.Task, abort bool) (extension.TaskDecision, error) {
			if abort {
				if definition.Task.Abort == nil {
					return extension.TaskDecision{Kind: "complete"}, nil
				}
				return definition.Task.Abort(ctx, task, nil)
			}
			return definition.Task.Run(ctx, task, nil)
		}
	}
	runtime, err := New(context.Background(), Options{SessionID: "session", Store: store, Now: now, Resolve: func(name string) (Definition, error) {
		definition, ok := definitions[name]
		if !ok {
			return Definition{}, errors.New("missing definition")
		}
		return Definition{Task: definition, Revision: "v1", Release: func() {}}, nil
	}, Invoke: invoke})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}
func taskDefinition(run func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error)) extension.TaskDefinition {
	return extension.TaskDefinition{Name: "flow", Version: 1, Initial: json.RawMessage(`{}`), InputSchema: objectSchema, StateSchema: objectSchema, Run: run}
}
func admit(t *testing.T, runtime *Runtime, id, definition string) {
	t.Helper()
	if _, err := runtime.Create(context.Background(), extension.TaskRequest{ID: id, Definition: definition, Input: json.RawMessage(`{}`), Background: true}); err != nil {
		t.Fatal(err)
	}
}

func TestReminderRestartAtomicDeliveryAndAlarm(t *testing.T) {
	ctx := context.Background()
	store := durable.NewMemory()
	now := time.Unix(100, 0).UTC()
	deadline := now.Add(time.Hour)
	calls := 0
	definition := taskDefinition(func(_ context.Context, task extension.Task, _ extension.Invocation) (extension.TaskDecision, error) {
		calls++
		if task.Phase == "run" {
			return extension.TaskDecision{Kind: "sleep", Phase: "deliver", WakeAt: &deadline}, nil
		}
		return extension.TaskDecision{Kind: "complete", Submissions: []extension.TaskSubmission{{Key: "delivery", Text: "remember"}}}, nil
	})
	definitions := map[string]extension.TaskDefinition{"demo/flow": definition}
	runtime := newTestRuntime(t, store, func() time.Time { return now }, definitions, nil)
	admit(t, runtime, "reminder", "demo/flow")
	if err := runtime.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if len(runtime.live) != 0 {
		t.Fatal("sleep retained a live invocation")
	}
	runtime = newTestRuntime(t, store, func() time.Time { return now }, definitions, nil)
	if calls != 1 {
		t.Fatal("opening ran task")
	}
	if wake, ok := runtime.NextWake(); !ok || !wake.Equal(deadline) {
		t.Fatalf("wake=%v,%v", wake, ok)
	}
	if err := runtime.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("early reminder")
	}
	now = deadline
	if err := runtime.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	runtime = newTestRuntime(t, store, func() time.Time { return now }, definitions, nil)
	if err := runtime.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := store.Read(ctx, durable.Query{SessionID: "session", Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || calls != 2 {
		t.Fatalf("deliveries=%d calls=%d", len(page.Records), calls)
	}
	if _, ok := runtime.NextWake(); ok {
		t.Fatal("terminal task retained alarm")
	}
}

func TestOwnerWaitValidationFailFastAndBottomUpAbort(t *testing.T) {
	ctx := context.Background()
	var order []string
	var orderMu sync.Mutex
	definitions := map[string]extension.TaskDefinition{}
	parent := taskDefinition(func(_ context.Context, task extension.Task, _ extension.Invocation) (extension.TaskDecision, error) {
		return extension.TaskDecision{Kind: "wait", Phase: "done", Wait: []string{"a", "b"}, FailFast: true, Children: []extension.TaskRequest{{ID: "a", Definition: "demo/child", Input: json.RawMessage(`{}`)}, {ID: "b", Definition: "demo/child", Input: json.RawMessage(`{}`)}}}, nil
	})
	parent.Abort = func(_ context.Context, task extension.Task, _ extension.Invocation) (extension.TaskDecision, error) {
		orderMu.Lock()
		defer orderMu.Unlock()
		order = append(order, task.ID)
		return extension.TaskDecision{Kind: "complete"}, nil
	}
	child := taskDefinition(func(_ context.Context, task extension.Task, _ extension.Invocation) (extension.TaskDecision, error) {
		if task.ID == "a" {
			return extension.TaskDecision{Kind: "complete", Error: "charge refused"}, nil
		}
		deadline := time.Now().Add(time.Hour)
		return extension.TaskDecision{Kind: "sleep", WakeAt: &deadline}, nil
	})
	child.Abort = parent.Abort
	definitions["demo/parent"] = parent
	definitions["demo/child"] = child
	runtime := newTestRuntime(t, durable.NewMemory(), nil, definitions, nil)
	admit(t, runtime, "parent", "demo/parent")
	admit(t, runtime, "unrelated", "demo/child")
	if err := runtime.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 || order[2] != "parent" || !((order[0] == "a" && order[1] == "b") || (order[0] == "b" && order[1] == "a")) {
		t.Fatalf("cleanup order=%v", order)
	}
	unrelated, _ := runtime.Get("unrelated")
	if unrelated.AbortReason != "" {
		t.Fatal("fail-fast aborted unrelated work")
	}
	graphTask := func(id, session string) extension.Task {
		return extension.Task{ID: id, SessionID: session, Background: true, Version: 1, CodeRevision: "v1", Phase: "run", Status: "waiting", Input: json.RawMessage(`{}`), Checkpoint: json.RawMessage(`{}`)}
	}
	parentTask, childTask := graphTask("p", "s"), graphTask("c", "s")
	parentTask.Wait = []string{"c"}
	childTask.Owner, childTask.Wait = "p", []string{"p"}
	graph := map[string]extension.Task{"p": parentTask, "c": childTask}
	if err := validateGraph(graph); err == nil {
		t.Fatal("accepted wait/owner deadlock")
	}
	graph["c"] = graphTask("c", "foreign")
	if err := validateGraph(graph); err == nil {
		t.Fatal("accepted foreign wait")
	}
	graph["c"] = graphTask("c", "s")
	if err := validateGraph(graph); err != nil {
		t.Fatalf("all-settled unrelated wait rejected: %v", err)
	}
	owner := graph["p"]
	owner.FailFast = true
	graph["p"] = owner
	if err := validateGraph(graph); err == nil {
		t.Fatal("accepted fail-fast unrelated wait")
	}
	parentTask.Wait = nil
	parentTask.Owner = "c"
	childTask.Wait = nil
	graph["p"], graph["c"] = parentTask, childTask
	if err := validateGraph(graph); err == nil {
		t.Fatal("accepted owner cycle")
	}
}

func TestPhaseAttemptFenceAndBackgroundSuspend(t *testing.T) {
	ctx := context.Background()
	store := durable.NewMemory()
	entered := make(chan extension.Task, 1)
	returned := make(chan struct{})
	release := make(chan struct{})
	definition := taskDefinition(func(ctx context.Context, task extension.Task, _ extension.Invocation) (extension.TaskDecision, error) {
		entered <- task
		<-ctx.Done()
		close(returned)
		<-release
		return extension.TaskDecision{Kind: "complete"}, nil
	})
	runtime := newTestRuntime(t, store, nil, map[string]extension.TaskDefinition{"demo/flow": definition}, nil)
	admit(t, runtime, "background", "demo/flow")
	resumed := make(chan error, 1)
	go func() { resumed <- runtime.Resume(ctx) }()
	attempt := <-entered
	suspended := make(chan error, 1)
	go func() { suspended <- runtime.Suspend(ctx) }()
	<-returned
	_, err := runtime.FencedCommit(ctx, attempt, durable.Commit{ID: "late", Changes: []durable.Change{{SessionID: "session", Kind: durable.DocumentKind, Namespace: "app", ID: "late", Version: 1, Value: json.RawMessage(`{}`)}}})
	// Cancellation fences the grant before the handler has joined. The runtime's
	// CAS fence is advanced by abort; Suspend preserves the restartable intent.
	if err != nil && !errors.Is(err, extension.ErrExpired) {
		t.Fatal(err)
	}
	close(release)
	if err := <-suspended; err != nil {
		t.Fatal(err)
	}
	if err := <-resumed; err != nil {
		t.Fatal(err)
	}
	task, _ := runtime.Get("background")
	if task.AbortReason != "" || task.Status != "suspended" {
		t.Fatalf("suspend=%+v", task)
	}
	if err := runtime.Abort(ctx, "background", "operator"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.FencedCommit(ctx, attempt, durable.Commit{ID: "stale"}); !errors.Is(err, extension.ErrExpired) {
		t.Fatalf("late commit=%v", err)
	}
}

func TestMissingMigrationAndCrashPolicyPark(t *testing.T) {
	ctx := context.Background()
	store := durable.NewMemory()
	definition := taskDefinition(func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		return extension.TaskDecision{Kind: "complete"}, nil
	})
	definitions := map[string]extension.TaskDefinition{"demo/flow": definition}
	runtime := newTestRuntime(t, store, nil, definitions, nil)
	admit(t, runtime, "flow", "demo/flow")
	delete(definitions, "demo/flow")
	if err := runtime.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	task, _ := runtime.Get("flow")
	if task.Status != "parked" {
		t.Fatal(task.Status)
	}
	definition.Version = 2
	definition.Migrate = func(uint32, json.RawMessage) (json.RawMessage, error) { return nil, errors.New("bad migration") }
	definitions["demo/flow"] = definition
	if err := runtime.Retry(ctx, "flow"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	task, _ = runtime.Get("flow")
	if task.Status != "parked" {
		t.Fatal(task.Status)
	}
	definition.Migrate = func(uint32, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"migrated":true}`), nil
	}
	definitions["demo/flow"] = definition
	if err := runtime.Retry(ctx, "flow"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	task, _ = runtime.Get("flow")
	if task.Status != "completed" || task.Version != 2 {
		t.Fatalf("migration=%+v", task)
	}
}

type loseAcknowledgment struct {
	Store
	mu   sync.Mutex
	lost bool
}

func (s *loseAcknowledgment) Commit(ctx context.Context, batch durable.Commit) (durable.Receipt, error) {
	receipt, err := s.Store.Commit(ctx, batch)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil && !s.lost && len(batch.Changes) > 1 {
		s.lost = true
		return durable.Receipt{}, errors.New("lost response")
	}
	return receipt, err
}

func TestLostDecisionAcknowledgmentDoesNotRepeatDelivery(t *testing.T) {
	ctx := context.Background()
	store := &loseAcknowledgment{Store: durable.NewMemory()}
	definition := taskDefinition(func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		return extension.TaskDecision{Kind: "complete", Submissions: []extension.TaskSubmission{{Key: "one", Text: "delivered"}}}, nil
	})
	definitions := map[string]extension.TaskDefinition{"demo/flow": definition}
	runtime := newTestRuntime(t, store, nil, definitions, nil)
	admit(t, runtime, "flow", "demo/flow")
	if err := runtime.Resume(ctx); err != nil {
		t.Fatalf("could not resolve lost decision acknowledgment: %v", err)
	}
	runtime = newTestRuntime(t, store, nil, definitions, nil)
	if err := runtime.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := store.Read(ctx, durable.Query{SessionID: "session", Kind: durable.SubmissionKind, Limit: 10})
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("delivery count=%d err=%v", len(page.Records), err)
	}
}
