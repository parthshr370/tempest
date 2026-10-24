package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/dap"
	"go.harness.dev/harness/internal/question"
)

func TestAskDebugHeadlessAndChild(t *testing.T) {
	tool, err := NewTool(AskTool, t.TempDir(), ToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"questions":[{"id":"q","question":"Choose","options":[{"label":"one"}]}]}`)
	if _, err := tool.Execute(t.Context(), "call", raw, nil); !errors.Is(err, question.Unavailable) || !strings.Contains(err.Error(), "best judgment") {
		t.Fatalf("headless error=%v", err)
	}
	if _, err := NewTool(AskTool, t.TempDir(), ToolsOptions{Child: true}); err == nil {
		t.Fatal("child can directly ask user")
	}
	if !AllToolNames[AskTool] || !AllToolNames[DebugTool] || !AllToolNames[SearchHistoryTool] {
		t.Fatal("tools missing from reserved names")
	}
}

func TestAskDebugQuestionValidation(t *testing.T) {
	calls := 0
	tool := newAskTool("session", func(_ context.Context, request question.Request) ([]question.Answer, error) {
		calls++
		if request.SessionID != "session" || request.ToolCallID != "call" {
			t.Fatalf("request=%+v", request)
		}
		return []question.Answer{{ID: "q", Selected: []string{"one"}, Text: "context"}}, nil
	})
	result, err := tool.Execute(t.Context(), "call", json.RawMessage(`{"questions":[{"id":"q","question":"Choose","options":[{"label":"one","description":"First"}],"allow_text":true}]}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if tool.ExecutionMode != agent.ExecSequential {
		t.Fatal("ask must serialize the batch")
	}
	for _, invalid := range []string{`{"questions":[]}`, `{"questions":[{"id":"q","question":"?","options":[{"label":"one"},{"label":"one"}]}]}`, `{"questions":[{"id":"q","question":"?","options":[]}]}`} {
		if _, err := tool.Execute(t.Context(), "call", json.RawMessage(invalid), nil); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
	if calls != 1 {
		t.Fatalf("invalid questions reached UI: %d", calls)
	}
}

func TestAskDebugQueuedAskCancelsWithoutPresenting(t *testing.T) {
	entered := make(chan struct{})
	tool := newAskTool("", func(ctx context.Context, _ question.Request) ([]question.Answer, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	raw := json.RawMessage(`{"questions":[{"id":"q","question":"Choose","options":[{"label":"one"}]}]}`)
	firstCtx, cancelFirst := context.WithCancel(t.Context())
	defer cancelFirst()
	firstDone := make(chan error, 1)
	go func() {
		_, err := tool.Execute(firstCtx, "first", raw, nil)
		firstDone <- err
	}()
	<-entered
	secondCtx, cancelSecond := context.WithCancel(t.Context())
	cancelSecond()
	if _, err := tool.Execute(secondCtx, "second", raw, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancellation: %v", err)
	}
	cancelFirst()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("active cancellation: %v", err)
	}
}

func TestAskDebugSchemas(t *testing.T) {
	assertSchemaParity(t, "ask", newAskTool("", nil).Parameters, `{"type":"object","properties":{"questions":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"question":{"type":"string"},"options":{"type":"array","items":{"type":"object","properties":{"label":{"type":"string"},"description":{"type":"string"}},"required":["label"]}},"multi":{"type":"boolean"},"allow_text":{"type":"boolean"}},"required":["id","question","options"]},"minItems":1,"maxItems":20}},"required":["questions"]}`)
	assertSchemaParity(t, "debug", newDebugTool("", nil).Parameters, `{"type":"object","properties":{"action":{"type":"string","enum":["launch","attach","set_breakpoint","remove_breakpoint","set_breakpoints","continue","step_over","step_in","step_out","pause","threads","stack_trace","scopes","variables","evaluate","output","terminate"]},"adapter":{"type":"string"},"program":{"type":"string"},"args":{"type":"array","items":{"type":"string"}},"cwd":{"type":"string"},"file":{"type":"string"},"line":{"type":"integer"},"lines":{"type":"array","items":{"type":"integer"}},"thread_id":{"type":"integer"},"frame_id":{"type":"integer"},"variable_ref":{"type":"integer"},"expression":{"type":"string"},"pid":{"type":"integer"},"arguments":{"type":"object","additionalProperties":true},"timeout":{"type":"number","minimum":1,"maximum":300,"default":30}},"required":["action"]}`)
}

func TestAskDebugHistoryRegistry(t *testing.T) {
	directory := t.TempDir()
	debugger, err := dap.New(directory, directory, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := debugger.Close(); err != nil {
			t.Error(err)
		}
	})
	options := ToolsOptions{Debugger: debugger, HistorySearch: func(context.Context, string, string, int) (json.RawMessage, error) {
		return json.RawMessage(`[]`), nil
	}}
	for _, name := range []ToolName{AskTool, DebugTool, SearchHistoryTool} {
		tool, err := NewTool(name, directory, options)
		if err != nil || tool.Name != string(name) || tool.Execute == nil {
			t.Fatalf("%s registry entry=%+v err=%v", name, tool, err)
		}
	}
	for _, name := range []ToolName{DebugTool, SearchHistoryTool} {
		if _, err := NewTool(name, directory, ToolsOptions{}); err == nil {
			t.Fatalf("%s registered without its session owner", name)
		}
	}
}

func TestDebugPathsUseSessionWorkspace(t *testing.T) {
	adapter := os.Getenv("TEMPEST_TEST_DEBUGPY_ADAPTER")
	if adapter == "" {
		var err error
		adapter, err = exec.LookPath("debugpy-adapter")
		if err != nil {
			t.Skip("debugpy-adapter is not installed")
		}
	}
	if _, err := os.Stat(adapter); err != nil {
		t.Skipf("debugpy adapter unavailable: %v", err)
	}
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "target.py"), []byte("value = 40\nvalue += 2\nprint(value)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	debugger, err := dap.New(directory, directory, map[string]dap.Adapter{"debugpy": {Command: adapter}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := debugger.Close(); err != nil {
			t.Error(err)
		}
	})
	tool := newDebugTool(directory, debugger)
	if _, err := tool.Execute(t.Context(), "launch", json.RawMessage(`{"action":"launch","adapter":"debugpy","program":"target.py","cwd":"sub"}`), nil); err != nil {
		t.Fatalf("program must resolve against workspace, not cwd: %v", err)
	}
	result, err := tool.Execute(t.Context(), "breakpoint", json.RawMessage(`{"action":"set_breakpoints","file":"target.py","lines":[3]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Breakpoints []struct {
			Verified bool `json:"verified"`
			Source   struct {
				Path string `json:"path"`
			} `json:"source"`
		} `json:"breakpoints"`
	}
	if err := json.Unmarshal([]byte(resultText(result)), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Breakpoints) != 1 || !response.Breakpoints[0].Verified || response.Breakpoints[0].Source.Path != filepath.Join(directory, "target.py") {
		t.Fatalf("breakpoint must resolve against the same workspace: %+v", response)
	}
}
