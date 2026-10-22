package a2a

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	model "github.com/a2aproject/a2a-go/v2/a2a"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest"
)

func TestObserverGapAfterRunEndStillCapturesOwnResult(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.Text("last assistant answer")), faux.Respond(faux.Text("next run must not overwrite")))
	session, err := tempest.Open(t.Context(), tempest.Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &tempest.NewSession{PermissionMode: permission.ModeBypass}, Stack: &tempest.StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	initial, updates, stop := session.Watch(t.Context(), tempest.WatchOptions{Buffer: 1})
	defer stop()
	run, err := session.Begin(t.Context(), tempest.Prompt{Text: "first"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := run.Started(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Prompt(t.Context(), tempest.Prompt{Text: "second"}); err != nil {
		t.Fatal(err)
	}
	projected := projection(id, model.TaskStateWorking, time.Now().UTC())
	projected.run, projected.session = run, session
	observed := make(chan struct{})
	go func() { projected.observe(initial, updates, stop, t.Context()); close(observed) }()
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("closed Watch stranded terminal capture")
	}
	task := projected.snapshot(nil, true)
	if task.Status.State != model.TaskStateCompleted || task.Artifacts[0].Parts[0].Text() != "last assistant answer" {
		t.Fatalf("wrong run projection: %+v", task)
	}
	if run.FinishedAt().IsZero() || !task.Status.Timestamp.Equal(run.FinishedAt()) {
		t.Fatal("gap recovery replaced the original run-finished timestamp")
	}
	raw, err := json.Marshal(task.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"progressInterrupted":true`) {
		t.Fatal("Watch gap wasn't marked", string(raw))
	}
	if projected.run != nil || projected.session != nil {
		t.Fatal("terminal task retained unbounded run/session transcript")
	}
}
