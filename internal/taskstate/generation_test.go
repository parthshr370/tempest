package taskstate

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

func TestChildAdmissionUsesPinnedPhaseGeneration(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var replaced atomic.Bool
	child := taskDefinition(func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		return extension.TaskDecision{Kind: "complete"}, nil
	})
	child.Name = "child"
	parent := taskDefinition(func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		close(started)
		<-release
		deadline := time.Now().Add(time.Hour)
		return extension.TaskDecision{Kind: "sleep", WakeAt: &deadline, Children: []extension.TaskRequest{{ID: "child", Definition: "demo/child", Input: json.RawMessage(`{}`)}}}, nil
	})
	pinned := []extension.Definition{{ID: "demo", Revision: "v1", Tasks: []extension.TaskDefinition{parent, child}}}
	runtime, err := New(t.Context(), Options{SessionID: "session", Store: durable.NewMemory(), Resolve: func(name string) (Definition, error) {
		definition := parent
		revision := "v1"
		if name == "demo/child" {
			definition = child
			if replaced.Load() {
				definition.Version = 2
				revision = "v2"
			}
		}
		return Definition{Task: definition, Revision: revision, Bundles: pinned, Release: func() {}}, nil
	}, Invoke: func(ctx context.Context, definition Definition, task extension.Task, _ bool) (extension.TaskDecision, error) {
		return definition.Task.Run(ctx, task, nil)
	}})
	if err != nil {
		t.Fatal(err)
	}
	admit(t, runtime, "parent", "demo/flow")
	finished := make(chan error, 1)
	go func() { finished <- runtime.Resume(t.Context()) }()
	<-started
	replaced.Store(true)
	close(release)
	if err = <-finished; err != nil {
		t.Fatal(err)
	}
	admitted, err := runtime.Get("child")
	if err != nil || admitted.Version != 1 || admitted.CodeRevision != "v1" || admitted.Status != "parked" {
		t.Fatalf("child=%+v err=%v", admitted, err)
	}
}
