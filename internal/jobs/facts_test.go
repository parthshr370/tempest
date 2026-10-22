package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"go.harness.dev/harness/internal/work"
)

func strictFact(fact work.Fact) error {
	var required, optional []string
	switch fact.Kind {
	case "job_started":
		required = []string{"job_id", "identity_digest", "call_id", "tool_call_id", "logical_run_id", "kind", "spec", "cwd"}
		optional = []string{"environment", "child_session_id", "child_journal", "output_path", "worktree"}
	case "job_running":
		required = []string{"job_id"}
		optional = []string{"child_run_id", "pid", "pgid"}
	case "job_attached":
		required = []string{"job_id", "run_id", "logical_run_id"}
	case "job_cancel_requested":
		required = []string{"job_id", "reason"}
	case "job_finished":
		required = []string{"job_id", "state", "finished_at", "usage", "cleanup"}
		optional = []string{"reason", "exit_code", "result", "patch", "output", "value", "result_preview"}
	case "job_cleaned":
		required = []string{"job_id", "path"}
	case "run_stop_requested":
		required = []string{"reason"}
	default:
		return fmt.Errorf("unexpected fact %s", fact.Kind)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(fact.Payload, &fields); err != nil {
		return err
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, key := range required {
		raw, present := fields[key]
		if !present || bytes.Equal(raw, []byte(`""`)) || bytes.Equal(raw, []byte("null")) {
			return fmt.Errorf("missing %s in %s", key, fact.Kind)
		}
		allowed[key] = true
	}
	for _, key := range optional {
		allowed[key] = true
	}
	for key := range fields {
		if !allowed[key] {
			return fmt.Errorf("unknown %s in %s", key, fact.Kind)
		}
	}
	return nil
}

func TestTaskRunningOnlyAfterChildSignalsDurableStart(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	host := newHost(t, func(ctx context.Context, launch work.TaskLaunch) (work.TaskResult, error) {
		return work.TaskResult{}, nil
	})
	host.manager.host.RunTask = func(ctx context.Context, launch work.TaskLaunch) (work.TaskResult, error) {
		close(entered)
		select {
		case <-ctx.Done():
			return work.TaskResult{}, ctx.Err()
		case <-release:
		}
		if err := host.manager.TaskRunning(ctx, launch.Job.ID, "child-run"); err != nil {
			return work.TaskResult{}, err
		}
		return work.TaskResult{Text: "done"}, nil
	}
	job, err := host.manager.StartTask(invocation(1), work.TaskSpec{Prompt: "work", SubagentType: "explore"})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	host.mu.Lock()
	for _, fact := range host.facts {
		if fact.Kind == "job_running" {
			t.Error("running committed before child durable start")
		}
	}
	host.mu.Unlock()
	close(release)
	awaitJob(t, host.manager, job.ID)
	host.mu.Lock()
	defer host.mu.Unlock()
	found := false
	for _, fact := range host.facts {
		if fact.Kind == "job_running" {
			found = true
			if !bytes.Contains(fact.Payload, []byte(`"child_run_id":"child-run"`)) {
				t.Fatal(string(fact.Payload))
			}
		}
	}
	if !found {
		t.Fatal("missing child durable start")
	}
}

func TestTerminalPublicationCanReenterObservationalController(t *testing.T) {
	host := newHost(t, func(context.Context, work.TaskLaunch) (work.TaskResult, error) {
		return work.TaskResult{Text: "done"}, nil
	})
	host.manager.host.Publish = func(ctx context.Context, event work.Event) error {
		if event.Kind == "job" && event.Action == "finished" {
			job, err := host.manager.Cancel(ctx, event.Job.ID)
			if err != nil {
				return err
			}
			if job.State != "completed" {
				return fmt.Errorf("terminal transition changed: %s", job.State)
			}
		}
		return nil
	}
	job, err := host.manager.StartTask(invocation(1), work.TaskSpec{Prompt: "work", SubagentType: "explore"})
	if err != nil {
		t.Fatal(err)
	}
	awaitJob(t, host.manager, job.ID)
}
