package taskstate

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

func TestConcurrentResumesInvokeOneAttemptPerRevision(t *testing.T) {
	var invocations atomic.Uint32
	entered := make(chan struct{})
	release := make(chan struct{})
	definition := taskDefinition(func(context.Context, extension.Task, extension.Invocation) (extension.TaskDecision, error) {
		if invocations.Add(1) == 1 {
			close(entered)
		}
		<-release
		return extension.TaskDecision{Kind: "complete"}, nil
	})
	runtime := newTestRuntime(t, durable.NewMemory(), nil, map[string]extension.TaskDefinition{"demo/flow": definition}, nil)
	admit(t, runtime, "flow", "demo/flow")
	const callers = 16
	var drains sync.WaitGroup
	failures := make(chan error, callers)
	for range callers {
		drains.Add(1)
		go func() { defer drains.Done(); failures <- runtime.Resume(t.Context()) }()
	}
	<-entered
	close(release)
	drains.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if invocations.Load() != 1 {
		t.Fatalf("phase invocations=%d", invocations.Load())
	}
	task, err := runtime.Get("flow")
	if err != nil || task.Status != "completed" || task.Attempt != 1 {
		t.Fatalf("task=%+v err=%v", task, err)
	}
}
