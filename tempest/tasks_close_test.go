package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

// pausingTaskCreateStore holds the commit of one task's creation, after it is
// durable, until the test releases it.
type pausingTaskCreateStore struct {
	durable.Store
	taskID    string
	committed chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (s *pausingTaskCreateStore) AcquireSession(ctx context.Context, id string) (func(), error) {
	return s.Store.(durable.SessionLeaser).AcquireSession(ctx, id)
}

func (s *pausingTaskCreateStore) Commit(ctx context.Context, c durable.Commit) (durable.Receipt, error) {
	receipt, err := s.Store.Commit(ctx, c)
	if err == nil && strings.HasSuffix(c.ID, "task:create:"+s.taskID) {
		s.once.Do(func() { close(s.committed); <-s.release })
	}
	return receipt, err
}

// A task whose creation committed before Close sealed the session exists and
// resumes when the session reopens, so CreateTask reports it created. An
// ErrClosed here would make a client that retries on it create the task twice.
func TestCreateTaskCommittedDuringCloseSucceeds(t *testing.T) {
	provider := faux.New(faux.Options{})
	registry := extension.NewRegistry()
	t.Cleanup(func() { _ = registry.Close(context.Background()) })
	definition := extension.TaskDefinition{Name: "flow", Version: 1, Initial: json.RawMessage(`{}`), InputSchema: json.RawMessage(`{"type":"object"}`), StateSchema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		return extension.TaskDecision{Kind: "complete"}, nil
	}}
	if err := registry.Install(t.Context(), extension.Definition{ID: "tasks", Revision: "v1", Tasks: []extension.TaskDefinition{definition}}); err != nil {
		t.Fatal(err)
	}
	backend := &pausingTaskCreateStore{Store: durable.NewMemory(), taskID: "late", committed: make(chan struct{}), release: make(chan struct{})}
	owner, err := Open(t.Context(), Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), Store: backend, New: &NewSession{}, Registry: registry, Extensions: []extension.Selection{{ID: "tasks", Required: true}}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	type created struct {
		task Task
		err  error
	}
	creating := make(chan created, 1)
	go func() {
		task, err := owner.CreateTask(context.Background(), TaskRequest{ID: "late", Definition: "tasks/flow", Input: json.RawMessage(`{}`), Background: true})
		creating <- created{task, err}
	}()
	select {
	case <-backend.committed:
	case <-time.After(10 * time.Second):
		t.Fatal("the task's creation never committed")
	}
	closed := make(chan error, 1)
	go func() { closed <- owner.Close(context.Background()) }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		owner.core.mu.Lock()
		sealed := owner.core.closed
		owner.core.mu.Unlock()
		if sealed {
			break
		}
		if time.Now().After(deadline) {
			close(backend.release)
			t.Fatal("Close did not seal the session")
		}
	}
	close(backend.release)
	result := <-creating
	if closeErr := <-closed; closeErr != nil {
		t.Fatalf("Close = %v", closeErr)
	}
	if result.err != nil || result.task.ID != "late" {
		t.Fatalf("CreateTask whose write committed returned %+v, %v; want the task and no error", result.task, result.err)
	}
}

// openWithBackgroundTask opens a session holding one admitted, undrained
// background task named "background".
func openWithBackgroundTask(t *testing.T) *Session {
	t.Helper()
	provider := faux.New(faux.Options{})
	registry := extension.NewRegistry()
	t.Cleanup(func() { _ = registry.Close(context.Background()) })
	definition := extension.TaskDefinition{Name: "flow", Version: 1, Initial: json.RawMessage(`{}`), InputSchema: json.RawMessage(`{"type":"object"}`), StateSchema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		return extension.TaskDecision{Kind: "complete"}, nil
	}}
	if err := registry.Install(t.Context(), extension.Definition{ID: "tasks", Revision: "v1", Tasks: []extension.TaskDefinition{definition}}); err != nil {
		t.Fatal(err)
	}
	owner, err := Open(t.Context(), Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), New: &NewSession{}, Registry: registry, Extensions: []extension.Selection{{ID: "tasks", Required: true}}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close(context.Background()) })
	if _, err := owner.CreateTask(t.Context(), TaskRequest{ID: "background", Definition: "tasks/flow", Input: json.RawMessage(`{}`), Background: true}); err != nil {
		t.Fatal(err)
	}
	return owner
}

// taskMutations are the public calls that write durable task state.
var taskMutations = map[string]func(*Session) error{
	"CancelTask": func(s *Session) error { return s.CancelTask(context.Background(), "background") },
	"CancelAll":  func(s *Session) error { return s.CancelAll(context.Background()) },
	"RetryTask":  func(s *Session) error { return s.RetryTask(context.Background(), "background") },
	"CreateTask": func(s *Session) error {
		_, err := s.CreateTask(context.Background(), TaskRequest{ID: "late", Definition: "tasks/flow", Input: json.RawMessage(`{}`), Background: true})
		return err
	},
}

// A task mutation that starts after Close has sealed the session is refused
// with tempest.ErrClosed and leaves durable task state alone: Close is
// suspending the background task, not cancelling it.
func TestTaskMutationAfterCloseSealsHasNoDurableEffect(t *testing.T) {
	for name, mutate := range taskMutations {
		owner := openWithBackgroundTask(t)
		before, err := owner.Task(context.Background(), "background")
		if err != nil {
			t.Fatal(err)
		}
		// Hold admission so Close seals the session and suspends the runtime
		// but cannot release the store yet: a Close in flight.
		owner.core.admission.Lock()
		closed := make(chan error, 1)
		go func() { closed <- owner.Close(context.Background()) }()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
			owner.core.mu.Lock()
			sealed := owner.core.closed
			owner.core.mu.Unlock()
			if sealed {
				break
			}
			if time.Now().After(deadline) {
				owner.core.admission.Unlock()
				t.Fatal("Close did not seal the session")
			}
		}
		err = mutate(owner)
		after, readErr := owner.Task(context.Background(), "background")
		_, lateErr := owner.Task(context.Background(), "late")
		owner.core.admission.Unlock()
		if closeErr := <-closed; closeErr != nil || readErr != nil {
			t.Fatalf("%s: Close=%v read=%v", name, closeErr, readErr)
		}
		if !errors.Is(err, ErrClosed) {
			t.Errorf("%s during Close returned %v, want ErrClosed", name, err)
		}
		if after.Revision != before.Revision || after.Status != before.Status || after.AbortReason != "" || lateErr == nil {
			t.Errorf("%s during Close changed task state: revision %d->%d status %s->%s abort=%q late created=%v", name, before.Revision, after.Revision, before.Status, after.Status, after.AbortReason, lateErr == nil)
		}
	}
}

func TestTaskMutationAfterCloseReturnsErrClosed(t *testing.T) {
	for name, mutate := range taskMutations {
		owner := openWithBackgroundTask(t)
		if err := owner.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := mutate(owner); !errors.Is(err, ErrClosed) || errors.Is(err, durable.ErrClosed) {
			t.Errorf("%s after Close returned %v, want tempest.ErrClosed", name, err)
		}
	}
}
