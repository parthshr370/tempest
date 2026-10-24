package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

func TestTaskReminderGoAndRPCResumeSameRuntime(t *testing.T) {
	for _, adapter := range []string{"go", "rpc"} {
		t.Run(adapter, func(t *testing.T) {
			ctx := t.Context()
			provider := faux.New(faux.Options{})
			provider.SetResponses(faux.Respond(faux.Text("delivered")))
			registry := extension.NewRegistry()
			defer registry.Close(context.Background())
			now := time.Unix(1000, 0).UTC()
			wake := now.Add(time.Hour)
			if adapter == "go" {
				definition := extension.TaskDefinition{Name: "flow", Version: 1, InputSchema: json.RawMessage(`{"type":"object"}`), StateSchema: json.RawMessage(`{"type":"object"}`), Initial: json.RawMessage(`{}`), Run: func(_ context.Context, task extension.Task, _ extension.Invocation) (extension.TaskDecision, error) {
					if task.Phase == "run" {
						return extension.TaskDecision{Kind: "sleep", Phase: "delivery", WakeAt: &wake}, nil
					}
					return extension.TaskDecision{Kind: "complete", Submissions: []extension.TaskSubmission{{Key: "delivery", Text: "durable reminder"}}}, nil
				}}
				if err := registry.Install(ctx, extension.Definition{ID: "tasks", Revision: "v1", Tasks: []extension.TaskDefinition{definition}}); err != nil {
					t.Fatal(err)
				}
			} else {
				python, err := exec.LookPath("python3")
				if err != nil {
					t.Skip("python3 unavailable")
				}
				fixture, err := filepath.Abs("extension/testdata/tasks.py")
				if err != nil {
					t.Fatal(err)
				}
				if err = registry.InstallProcess(ctx, extension.ProcessOptions{ID: "tasks", Argv: []string{python, fixture}, Capabilities: []string{"tasks", "memo"}}); err != nil {
					t.Fatal(err)
				}
			}
			options := Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), New: &NewSession{PermissionMode: permission.ModeBypass}, Registry: registry, Extensions: []extension.Selection{{ID: "tasks", Required: true}}, Tasks: TaskOptions{Now: func() time.Time { return now }}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple, AllowedTools: []string{"read"}}}
			owner, err := Open(ctx, options)
			if err != nil {
				t.Fatal(err)
			}
			input, _ := json.Marshal(struct {
				WakeAt time.Time `json:"wakeAt"`
			}{wake})
			if _, err = owner.CreateTask(ctx, TaskRequest{ID: "reminder", Definition: "tasks/flow", Input: input, Background: true}); err != nil {
				t.Fatal(err)
			}
			if err = owner.ResumePending(ctx); err != nil {
				t.Fatal(err)
			}
			task, err := owner.Task(ctx, "reminder")
			if err != nil || task.Status != "sleeping" {
				t.Fatalf("task=%+v err=%v", task, err)
			}
			id := owner.ID()
			if err = owner.Close(ctx); err != nil {
				t.Fatal(err)
			}
			options.New = nil
			options.Resume = &ResumeSession{SessionID: id}
			owner, err = Open(ctx, options)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close(context.Background())
			if wakeAt, ok := owner.NextWake(); !ok || !wakeAt.Equal(wake) {
				t.Fatalf("alarm=%v %v", wakeAt, ok)
			}
			snapshot, _, stop := owner.Watch(ctx, WatchOptions{})
			stop()
			if len(snapshot.Tasks) != 1 {
				t.Fatalf("task projection=%+v", snapshot.Tasks)
			}
			task, _ = owner.Task(ctx, "reminder")
			if task.Status != "sleeping" {
				t.Fatal("inspection ran task")
			}
			now = wake
			if err = owner.ResumePending(ctx); err != nil {
				t.Fatal(err)
			}
			submission, err := owner.WaitSubmission(ctx, "task:reminder:delivery")
			if err != nil || submission.Answer != "delivered" {
				t.Fatalf("submission=%+v err=%v", submission, err)
			}
			task, _ = owner.Task(ctx, "reminder")
			if task.Status != "completed" {
				t.Fatal(task.Status)
			}
			page, err := owner.core.store.Read(ctx, durable.Query{Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", Limit: 10})
			if err != nil || len(page.Records) != 1 {
				t.Fatalf("deliveries=%d err=%v", len(page.Records), err)
			}
		})
	}
}

func TestTaskCloseSuspendsAndCancelRunKeepsBackground(t *testing.T) {
	ctx := t.Context()
	provider := faux.New(faux.Options{})
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	started := make(chan struct{})
	var once sync.Once
	definition := extension.TaskDefinition{Name: "flow", Version: 1, InputSchema: json.RawMessage(`{"type":"object"}`), StateSchema: json.RawMessage(`{"type":"object"}`), Initial: json.RawMessage(`{}`), Run: func(ctx context.Context, task extension.Task, _ extension.Invocation) (extension.TaskDecision, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return extension.TaskDecision{}, ctx.Err()
	}}
	if err := registry.Install(ctx, extension.Definition{ID: "tasks", Revision: "v1", Tasks: []extension.TaskDefinition{definition}}); err != nil {
		t.Fatal(err)
	}
	options := Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), New: &NewSession{PermissionMode: permission.ModeBypass}, Registry: registry, Extensions: []extension.Selection{{ID: "tasks", Required: true}}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple, AllowedTools: []string{"read"}}}
	owner, err := Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.CreateTask(ctx, TaskRequest{ID: "background", Definition: "tasks/flow", Input: json.RawMessage(`{}`), Background: true}); err != nil {
		t.Fatal(err)
	}
	resumed := make(chan error, 1)
	go func() { resumed <- owner.ResumePending(ctx) }()
	<-started
	owner.Cancel()
	id := owner.ID()
	if err = owner.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-resumed; err != nil && !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	options.New = nil
	options.Resume = &ResumeSession{SessionID: id}
	owner, err = Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	task, err := owner.Task(ctx, "background")
	if err != nil || task.Status != "suspended" || task.AbortReason != "" {
		t.Fatalf("task=%+v err=%v", task, err)
	}
}
