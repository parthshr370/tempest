package tempest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest/extension"
)

func TestTaskHostAlarmSurvivesArchiveAndReopen(t *testing.T) {
	ctx := t.Context()
	now := time.Unix(1000, 0).UTC()
	wake := now.Add(time.Hour)
	calls := 0
	provider := faux.New(faux.Options{})
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	definition := extension.TaskDefinition{Name: "flow", Version: 1, InputSchema: json.RawMessage(`{"type":"object"}`), StateSchema: json.RawMessage(`{"type":"object"}`), Initial: json.RawMessage(`{}`), Run: func(_ context.Context, task extension.Task, _ extension.Invocation) (extension.TaskDecision, error) {
		calls++
		if task.Phase == "run" {
			return extension.TaskDecision{Kind: "sleep", Phase: "done", WakeAt: &wake}, nil
		}
		return extension.TaskDecision{Kind: "complete"}, nil
	}}
	if err := registry.Install(ctx, extension.Definition{ID: "tasks", Revision: "v1", Tasks: []extension.TaskDefinition{definition}}); err != nil {
		t.Fatal(err)
	}
	options := HostOptions{SessionDir: t.TempDir(), Registry: registry, Extensions: []extension.Selection{{ID: "tasks", Required: true}}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple, AllowedTools: []string{"read"}}, Options: Options{Tasks: TaskOptions{Now: func() time.Time { return now }}}}
	host, err := OpenHost(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := host.Create(ctx, AgentOptions{Cwd: t.TempDir(), New: &NewSession{PermissionMode: permission.ModeBypass}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.CreateTask(ctx, TaskRequest{ID: "reminder", Definition: "tasks/flow", Input: json.RawMessage(`{}`), Background: true}); err != nil {
		t.Fatal(err)
	}
	if err = host.ResumePending(ctx); err != nil {
		t.Fatal(err)
	}
	id := owner.ID()
	if err = host.CloseSession(ctx, id); err != nil {
		t.Fatal(err)
	}
	if alarm, present := host.NextWake(); !present || !alarm.Equal(wake) {
		t.Fatalf("archived alarm=%v present=%v", alarm, present)
	}
	if err = host.Close(ctx); err != nil {
		t.Fatal(err)
	}
	host, err = OpenHost(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	if alarm, present := host.NextWake(); !present || !alarm.Equal(wake) || calls != 1 {
		t.Fatalf("reopened alarm=%v present=%v calls=%d", alarm, present, calls)
	}
	if err = host.ResumePending(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("host ran future reminder")
	}
	now = wake
	if err = host.ResumePending(ctx); err != nil {
		t.Fatal(err)
	}
	owner, err = host.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	task, err := owner.Task(ctx, "reminder")
	if err != nil || task.Status != "completed" || calls != 2 {
		t.Fatalf("task=%+v calls=%d err=%v", task, calls, err)
	}
}
