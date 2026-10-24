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

func TestTaskNativeBashJoinsBeforeOwnerAbortAndKeepsBackground(t *testing.T) {
	ctx := t.Context()
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.ToolCall("ext__tasks__start", json.RawMessage(`{}`), "start")), faux.Respond(faux.Text("waiting")))
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	var owner *Session
	called := make(chan struct{})
	cleanupJoined := make(chan bool, 1)
	schema := json.RawMessage(`{"type":"object"}`)
	phase := extension.TaskDefinition{Name: "flow", Version: 1, InputSchema: schema, StateSchema: schema, Initial: json.RawMessage(`{}`), Run: func(ctx context.Context, task extension.Task, inv extension.Invocation) (extension.TaskDecision, error) {
		result, err := inv.CallTool(ctx, "shell", "bash", json.RawMessage(`{"command":"sleep 30","async":true}`))
		if err != nil {
			return extension.TaskDecision{}, err
		}
		if result.IsError {
			t.Errorf("native bash failed: %+v", result)
		}
		close(called)
		return extension.TaskDecision{Kind: "complete"}, nil
	}, Abort: func(_ context.Context, task extension.Task, _ extension.Invocation) (extension.TaskDecision, error) {
		cleanupJoined <- owner.core.jobs.OwnedTasksSettled(task.ID)
		return extension.TaskDecision{Kind: "complete"}, nil
	}}
	timer := phase
	timer.Name = "timer"
	timer.Run = func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		wake := time.Now().Add(time.Hour)
		return extension.TaskDecision{Kind: "sleep", WakeAt: &wake}, nil
	}
	timer.Abort = nil
	start := extension.Tool{Name: "start", Effect: extension.EffectExecute, InputSchema: schema, Execute: func(ctx context.Context, _ extension.Call, _ extension.Invocation) (extension.Result, error) {
		_, err := owner.CreateTask(ctx, TaskRequest{ID: "foreground", Definition: "tasks/flow", Input: json.RawMessage(`{}`)})
		return extension.Result{Content: []extension.Content{{Type: "text", Text: "admitted"}}}, err
	}}
	if err := registry.Install(ctx, extension.Definition{ID: "tasks", Revision: "v1", Tasks: []extension.TaskDefinition{phase, timer}, Tools: []extension.Tool{start}}); err != nil {
		t.Fatal(err)
	}
	var err error
	owner, err = Open(ctx, Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), New: &NewSession{PermissionMode: permission.ModeBypass}, Registry: registry, Extensions: []extension.Selection{{ID: "tasks", Required: true}}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple, AllowedTools: []string{"bash", "ext__tasks__start"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	if _, err = owner.CreateTask(ctx, TaskRequest{ID: "background", Definition: "tasks/timer", Input: json.RawMessage(`{}`), Background: true}); err != nil {
		t.Fatal(err)
	}
	if err = owner.ResumePending(ctx); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		result RunResult
		err    error
	}
	finished := make(chan answer, 1)
	go func() { result, err := owner.Prompt(ctx, Prompt{Text: "start work"}); finished <- answer{result, err} }()
	select {
	case <-called:
	case <-time.After(10 * time.Second):
		t.Fatal("phase never admitted native bash")
	}
	owner.Cancel()
	select {
	case reply := <-finished:
		if reply.result.StopReason != "aborted" {
			t.Fatalf("run=%+v err=%v", reply.result, reply.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not join native task")
	}
	select {
	case joined := <-cleanupJoined:
		if !joined {
			t.Fatal("owner abort ran before native child joined")
		}
	case <-time.After(time.Second):
		tasks, _ := owner.Tasks(ctx, TaskQuery{})
		t.Fatalf("owner cleanup was not invoked: tasks=%+v jobs=%+v", tasks, owner.core.jobs.Snapshot())
	}
	waitTaskStatus(t, owner, "foreground", "aborted")
	foreground, err := owner.Task(ctx, "foreground")
	if err != nil || foreground.Status != "aborted" {
		t.Fatalf("foreground=%+v err=%v", foreground, err)
	}
	background, err := owner.Task(ctx, "background")
	if err != nil || background.AbortReason != "" || background.Status != "sleeping" {
		t.Fatalf("background=%+v err=%v", background, err)
	}
	if err = owner.CancelAll(ctx); err != nil {
		t.Fatal(err)
	}
	background, _ = owner.Task(ctx, "background")
	if background.Status != "aborted" {
		t.Fatalf("cancel all=%+v", background)
	}
}
