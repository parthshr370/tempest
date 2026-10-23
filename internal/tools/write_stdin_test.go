package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.harness.dev/harness/internal/work"
)

type stdinController struct {
	work.Controller
	bash    work.BashSpec
	jobID   string
	request work.StdinRequest
}

func (controller *stdinController) StartBash(_ context.Context, spec work.BashSpec) (work.Job, error) {
	controller.bash = spec
	return work.Job{ID: "job_repl", Kind: "bash", State: "running", Name: spec.Name}, nil
}
func (controller *stdinController) JobByName(_ context.Context, name string) (work.Job, error) {
	return work.Job{ID: "job_" + name, Kind: "bash", State: "running", Name: name}, nil
}
func (controller *stdinController) WriteStdin(_ context.Context, jobID string, request work.StdinRequest) (work.StdinResult, error) {
	controller.jobID, controller.request = jobID, request
	code := 0
	return work.StdinResult{Job: work.Job{ID: jobID, State: "completed", ExitCode: &code}, Output: ">>> 42\r\n", Offset: 10, NextOffset: 18, OmittedBytes: 3, StdinClosed: true}, nil
}

func TestBashPTYStartsABackgroundJob(t *testing.T) {
	controller := &stdinController{}
	bash, err := NewTool(BashTool, t.TempDir(), ToolsOptions{Work: controller})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = executeTool(t, bash, map[string]any{"command": "python3 -i", "pty": true}); err != nil {
		t.Fatal(err)
	}
	if !controller.bash.PTY || controller.bash.Command != "python3 -i" {
		t.Fatalf("spec = %+v", controller.bash)
	}
	child, err := NewTool(BashTool, t.TempDir(), ToolsOptions{Work: controller, Child: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = executeTool(t, child, map[string]any{"command": "python3 -i", "pty": true}); err == nil {
		t.Fatal("a child's bash started a pty job")
	}
}

// TestBashStdinStartsABackgroundJob pins the opt-in: stdin:true alone makes
// a background job whose spec keeps stdin open, and a plain background job
// leaves it closed (/dev/null).
func TestBashStdinStartsABackgroundJob(t *testing.T) {
	controller := &stdinController{}
	bash, err := NewTool(BashTool, t.TempDir(), ToolsOptions{Work: controller})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = executeTool(t, bash, map[string]any{"command": "cat", "stdin": true}); err != nil {
		t.Fatal(err)
	}
	if !controller.bash.Stdin || controller.bash.PTY || controller.bash.Command != "cat" {
		t.Fatalf("spec = %+v", controller.bash)
	}
	if _, err = executeTool(t, bash, map[string]any{"command": "cat", "name": "c"}); err != nil {
		t.Fatal(err)
	}
	if controller.bash.Stdin || controller.bash.Name != "c" {
		t.Fatalf("named job spec = %+v", controller.bash)
	}
	child, err := NewTool(BashTool, t.TempDir(), ToolsOptions{Work: controller, Child: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = executeTool(t, child, map[string]any{"command": "cat", "stdin": true}); err == nil {
		t.Fatal("a child's bash started a background job with stdin")
	}
}

func TestWriteStdinToolMapsRequestAndResult(t *testing.T) {
	controller := &stdinController{}
	tool, err := NewTool(WriteStdinTool, t.TempDir(), ToolsOptions{Work: controller})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executeTool(t, tool, map[string]any{"name": "repl", "text": "print(6*7)\n\u0003", "close_stdin": true})
	if err != nil {
		t.Fatal(err)
	}
	want := work.StdinRequest{Text: "print(6*7)\n\x03", CloseStdin: true, Wait: work.DefaultStdinWaitMS * time.Millisecond}
	if controller.jobID != "job_repl" || controller.request != want {
		t.Fatalf("job %q request %+v", controller.jobID, controller.request)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(resultText(result)), &got); err != nil {
		t.Fatal(err)
	}
	wantKeys := map[string]any{"job_id": "job_repl", "state": "completed", "exit_code": 0.0, "output": ">>> 42\r\n", "offset": 10.0, "next_offset": 18.0, "omitted_bytes": 3.0, "stdin_closed": true}
	for key, value := range wantKeys {
		if got[key] != value {
			t.Fatalf("%s = %v, want %v (result %s)", key, got[key], value, resultText(result))
		}
	}
	if _, err = executeTool(t, tool, map[string]any{"job_id": "job_x", "wait_ms": 0}); err != nil || controller.request.Wait != 0 || controller.jobID != "job_x" {
		t.Fatalf("poll with wait_ms 0: %+v, %v", controller.request, err)
	}
	if _, err = executeTool(t, tool, map[string]any{"job_id": "job_x", "name": "repl"}); err == nil {
		t.Fatal("job_id and name together were accepted")
	}
	if _, err = NewTool(WriteStdinTool, t.TempDir(), ToolsOptions{Work: controller, Child: true}); err == nil {
		t.Fatal("a child session got write_stdin")
	}
}
