package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/progress"
)

func TestTodoToolOpsWithPhases(t *testing.T) {
	sessionDataDir := filepath.Join(t.TempDir(), "session.jsonl.d")
	todo, _ := NewTool(TodoTool, t.TempDir(), ToolsOptions{SessionDataDir: sessionDataDir})
	run := func(args map[string]any) string {
		t.Helper()
		result, err := executeTool(t, todo, args)
		if err != nil {
			t.Fatalf("todo_write %v: %v", args, err)
		}
		return resultText(result)
	}

	run(map[string]any{"op": "init", "phase": "Build", "items": []string{"parse input", "write output"}})
	run(map[string]any{"op": "append", "phase": "Verify", "items": []string{"run tests", "update docs"}})
	run(map[string]any{"op": "start", "items": []string{"parse input"}})
	run(map[string]any{"op": "start", "items": []string{"write output"}})
	run(map[string]any{"op": "block", "items": []string{"update docs"}, "reason": "waiting on\nreview"})
	run(map[string]any{"op": "done", "phase": "Build"})
	text := run(map[string]any{"op": "drop", "items": []string{"run tests"}})

	for _, want := range []string{
		"## Build\n- [x] parse input\n- [x] write output\n",
		"## Verify\n- [-] run tests _(dropped)_\n- [ ] update docs _(blocked: waiting on review)_\n",
		"Progress checkpoint: " + progress.Path(sessionDataDir),
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("todo result missing %q:\n%s", want, text)
		}
	}
	checkpoint, err := os.ReadFile(progress.Path(sessionDataDir))
	if err != nil || !strings.Contains(string(checkpoint), "## Verify") {
		t.Fatalf("progress.md = %q, %v", checkpoint, err)
	}

	// A new process (new store, same session directory) continues the list.
	resumed, _ := NewTool(TodoTool, t.TempDir(), ToolsOptions{SessionDataDir: sessionDataDir})
	result, err := executeTool(t, resumed, map[string]any{"op": "start", "items": []string{"update docs"}})
	if err != nil || !strings.Contains(resultText(result), "update docs _(in progress)_") {
		t.Fatalf("resumed op = %q, %v", resultText(result), err)
	}

	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"op": "start", "items": []string{"nope"}}, `no todo with content "nope"`},
		{map[string]any{"op": "append", "items": []string{"run tests"}}, "already exists"},
		{map[string]any{"op": "start", "items": []string{"a", "b"}}, "exactly one item"},
		{map[string]any{"op": "done"}, "items or a phase"},
		{map[string]any{"op": "block", "items": []string{"parse input"}}, "cannot be blocked"},
		{map[string]any{"op": "done", "phase": "Ship"}, `no todos in phase "Ship"`},
		{map[string]any{"op": "init", "todos": []map[string]any{{"content": "x", "status": "pending"}}}, "not both"},
		{map[string]any{}, "needs todos"},
	} {
		if _, err := executeTool(t, resumed, tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("todo_write %v error = %v, want %q", tc.args, err, tc.want)
		}
	}
}

func TestTodoToolFullReplaceCarriesPhases(t *testing.T) {
	todo, _ := NewTool(TodoTool, t.TempDir(), ToolsOptions{})
	result, err := executeTool(t, todo, map[string]any{"todos": []map[string]any{
		{"content": "loose end", "status": "pending"},
		{"content": "design", "status": "completed", "phase": "Plan"},
		{"content": "build", "status": "blocked", "phase": "Make", "blocker": "needs design sign-off"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := "# Progress\n\n- [ ] loose end\n\n## Plan\n- [x] design\n\n## Make\n- [ ] build _(blocked: needs design sign-off)_\n"
	if text := resultText(result); text != want {
		t.Fatalf("todo result = %q, want %q", text, want)
	}
}
