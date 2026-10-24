package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest/extension"
)

func TestTaskLiveBackgroundDoesNotHoldRunAdmissionOrCancellation(t *testing.T) {
	ctx := t.Context()
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.Text("foreground complete")))
	started := make(chan struct{})
	stopped := make(chan struct{})
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	definition := extension.TaskDefinition{Name: "flow", Version: 1, Initial: json.RawMessage(`{}`), InputSchema: json.RawMessage(`{"type":"object"}`), StateSchema: json.RawMessage(`{"type":"object"}`), Run: func(ctx context.Context, _ extension.Task, _ extension.Invocation) (extension.TaskDecision, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return extension.TaskDecision{}, ctx.Err()
	}}
	if err := registry.Install(ctx, extension.Definition{ID: "tasks", Revision: "v1", Tasks: []extension.TaskDefinition{definition}}); err != nil {
		t.Fatal(err)
	}
	owner, err := Open(ctx, Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), New: &NewSession{PermissionMode: permission.ModeBypass}, Registry: registry, Extensions: []extension.Selection{{ID: "tasks", Required: true}}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple, AllowedTools: []string{"read"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	if _, err = owner.CreateTask(ctx, TaskRequest{ID: "background", Definition: "tasks/flow", Input: json.RawMessage(`{}`), Background: true}); err != nil {
		t.Fatal(err)
	}
	resumed := make(chan error, 1)
	go func() { resumed <- owner.ResumePending(ctx) }()
	<-started
	finished := make(chan error, 1)
	go func() { _, err := owner.Prompt(ctx, Prompt{Text: "answer while background is live"}); finished <- err }()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("background phase held conversational admission")
	}
	owner.Cancel()
	select {
	case <-stopped:
		t.Fatal("ordinary run finish or cancel stopped background phase")
	default:
	}
	if err = owner.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-resumed:
		if err != nil && !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close did not join background drain")
	}
}
