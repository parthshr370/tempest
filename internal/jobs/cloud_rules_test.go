package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.harness.dev/harness/internal/cloudcmd"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/work"
)

// TestStartedFactCarriesCloudRules proves the launch ceiling's cloud rules are
// persisted in the shape the strict journal schema decodes, and that the
// child receives its own copy.
func TestStartedFactCarriesCloudRules(t *testing.T) {
	rules := []cloudcmd.Rule{{Tool: "kubectl", Context: "kind-dev", Action: "allow_write"}}
	prepared := work.TaskEnvironment{Ceiling: work.Ceiling{Mode: "acceptEdits", Tools: []string{"bash"}, CloudRules: rules}, Model: "test", Provider: "test"}
	launched := make(chan work.TaskLaunch, 1)
	host := &memoryHost{queued: make(map[string]work.QueueInput), changed: make(chan struct{})}
	directory := t.TempDir()
	manager, err := New(work.Host{SessionID: "parent", JournalPath: filepath.Join(directory, "parent.jsonl"), Cwd: directory, Queues: host, Append: host.append, Enqueue: host.enqueue,
		PrepareTask: func(context.Context, work.TaskSpec) (work.TaskEnvironment, error) { return prepared, nil },
		RunTask: func(_ context.Context, launch work.TaskLaunch) (work.TaskResult, error) {
			launched <- launch
			return work.TaskResult{Text: "done"}, nil
		}}, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	host.manager = manager
	if err = manager.Start(context.Background(), work.RunInfo{SessionID: "parent", RunID: "run", LogicalRunID: "run"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Finish(context.Background(), "aborted"); err != nil {
			t.Error(err)
		}
	})
	if _, err = manager.StartTask(invocation(1), work.TaskSpec{Prompt: "work", SubagentType: "explore"}); err != nil {
		t.Fatal(err)
	}
	var launch work.TaskLaunch
	select {
	case launch = <-launched:
	case <-time.After(5 * time.Second):
		t.Fatal("task did not launch")
	}
	rules[0].Context = "production"
	if got := launch.Environment.Ceiling.CloudRules; len(got) != 1 || got[0].Context != "kind-dev" {
		t.Fatalf("launch ceiling aliases the prepared rules: %+v", got)
	}

	host.mu.Lock()
	facts := slices.Clone(host.facts)
	host.mu.Unlock()
	var started *journal.JobStarted
	for _, fact := range facts {
		if fact.Kind != string(journal.KindJobStarted) {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(fact.Payload))
		decoder.DisallowUnknownFields()
		started = &journal.JobStarted{}
		if err := decoder.Decode(started); err != nil {
			t.Fatalf("journal schema rejects job_started payload %s: %v", fact.Payload, err)
		}
	}
	if started == nil || started.Environment == nil {
		t.Fatalf("no job_started environment in %+v", facts)
	}
	want := []cloudcmd.Rule{{Tool: "kubectl", Context: "kind-dev", Action: "allow_write"}}
	if !slices.Equal(started.Environment.Ceiling.CloudRules, want) {
		t.Fatalf("persisted rules = %+v", started.Environment.Ceiling.CloudRules)
	}
}
