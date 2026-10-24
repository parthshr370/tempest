package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hooks"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
)

func testCommandHooks(t *testing.T, cwd string, entries map[string]string) *hooks.Runner {
	t.Helper()
	settings := hooks.Settings{Hooks: map[string][]hooks.Group{}}
	for event, command := range entries {
		settings.Hooks[event] = []hooks.Group{{Hooks: []hooks.Command{{Type: "command", Command: command}}}}
	}
	runner, err := hooks.New(settings, hooks.Input{SessionID: "test", Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestPreToolUseBlocksUpdatesAndRespectsPermissions(t *testing.T) {
	for _, test := range []struct {
		name, command, mode, reason, wantInput string
		executed                               bool
		prompt                                 bool
	}{
		{name: "exit2", command: "printf 'hook stderr refusal' >&2; exit 2", reason: "hook stderr refusal"},
		{name: "json deny", command: `printf '%s' '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"json refusal"}}'`, reason: "json refusal"},
		{name: "allow respects explicit ask", command: `printf '%s' '{"hookSpecificOutput":{"permissionDecision":"allow"}}'`, prompt: true, reason: "requires approval"},
		{name: "allow updated input", command: `printf '%s' '{"hookSpecificOutput":{"permissionDecision":"allow","updatedInput":{"path":"changed"}}}'`, executed: true, wantInput: "changed"},
		{name: "updated input validated", command: `printf '%s' '{"hookSpecificOutput":{"updatedInput":{}}}'`, reason: "Hook updatedInput"},
		{name: "allow cannot bypass plan", command: `printf '%s' '{"hookSpecificOutput":{"permissionDecision":"allow"}}'`, mode: "plan", reason: "Plan mode"},
		{name: "ask without prompter", command: `printf '%s' '{"hookSpecificOutput":{"permissionDecision":"ask","permissionDecisionReason":"hook asks first"}}'`, reason: "requires approval"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cwd := t.TempDir()
			f := faux.New(faux.Options{})
			f.SetResponses(faux.Respond(faux.ToolCall("write", json.RawMessage(`{"path":"original"}`), "call")), func(c types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
				raw, _ := json.Marshal(c.Messages)
				if test.reason != "" && !strings.Contains(string(raw), test.reason) {
					t.Errorf("model did not receive refusal %q: %s", test.reason, raw)
				}
				return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("done")}, StopReason: types.StopStop}, nil
			})
			var executed atomic.Int32
			tool := executorTool("write", `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`, ExecParallel, func(_ context.Context, _ string, args json.RawMessage, _ AgentToolUpdateCallback) (AgentToolResult, error) {
				executed.Add(1)
				if !strings.Contains(string(args), test.wantInput) {
					t.Errorf("executed input=%s", args)
				}
				return AgentToolResult{Content: []types.ContentBlock{types.NewText("ok")}}, nil
			})
			policy := permission.Policy{}
			if test.prompt {
				policy.Overrides = map[string]permission.Decision{"write": permission.DecidePrompt}
			}
			mode := permission.ModeDefault
			if test.mode == "plan" {
				mode = permission.ModePlan
			}
			a := NewAgent(AgentOptions{InitialState: &AgentState{Model: f.Model(), Tools: []AgentTool{tool}}, StreamFn: f.StreamSimple, Hooks: testCommandHooks(t, cwd, map[string]string{"PreToolUse": test.command}), BeforeToolCall: func(ctx context.Context, c BeforeToolCallContext) *BeforeToolCallResult {
				raw, _ := json.Marshal(c.Args)
				outcome := permission.GateInDirectory(ctx, mode, c.ToolCall.Name, raw, &policy, cwd, c.ToolCall.ID)
				return &BeforeToolCallResult{Block: !outcome.Allow, Reason: outcome.Reason}
			}})
			result, err := a.PromptText(context.Background(), "go")
			if err != nil || result.Err != nil || result.Turns != 2 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if (executed.Load() == 1) != test.executed {
				t.Fatalf("executed=%d want %t", executed.Load(), test.executed)
			}
		})
	}
}

func TestPostToolUseContextAndResponseReachModel(t *testing.T) {
	cwd := t.TempDir()
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.ToolCall("work", json.RawMessage(`{}`), "call")), func(c types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
		raw, _ := json.Marshal(c.Messages)
		if !strings.Contains(string(raw), "post-tool advice") {
			t.Errorf("missing post-tool context: %s", raw)
		}
		return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("done")}, StopReason: types.StopStop}, nil
	})
	tool := executorTool("work", `{"type":"object"}`, ExecParallel, func(context.Context, string, json.RawMessage, AgentToolUpdateCallback) (AgentToolResult, error) {
		return AgentToolResult{Content: []types.ContentBlock{types.NewText("tool-result")}, Details: json.RawMessage(`{"answer":"response-payload"}`)}, nil
	})
	runner := testCommandHooks(t, cwd, map[string]string{"PostToolUse": `cat > post.json; printf '%s' '{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"post-tool advice"}}'`})
	a := NewAgent(AgentOptions{InitialState: &AgentState{Model: f.Model(), Tools: []AgentTool{tool}}, StreamFn: f.StreamSimple, Hooks: runner})
	result, err := a.PromptText(context.Background(), "go")
	if err != nil || result.Err != nil {
		t.Fatalf("%+v %v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(cwd, "post.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Response map[string]string `json:"tool_response"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || payload.Response["answer"] != "response-payload" {
		t.Fatalf("post payload=%s err=%v", data, err)
	}
}

func TestStopContinuesWithReasonWithoutResubmittingPrompt(t *testing.T) {
	for _, child := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "child"}[child], func(t *testing.T) {
			cwd := t.TempDir()
			event := "Stop"
			if child {
				event = "SubagentStop"
			}
			runner := testCommandHooks(t, cwd, map[string]string{
				"UserPromptSubmit": `printf x >> prompt-count; printf '%s' '{"hookSpecificOutput":{"additionalContext":"prompt advice"}}'`,
				event:              `input=$(cat); case "$input" in *'"stop_hook_active":true'*) ;; *) printf '%s' '{"decision":"block","reason":"finish the remaining work"}' ;; esac`,
			})
			f := faux.New(faux.Options{})
			f.SetResponses(func(c types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
				raw, _ := json.Marshal(c.Messages)
				if !strings.Contains(string(raw), "prompt advice") {
					t.Errorf("missing UserPromptSubmit context: %s", raw)
				}
				return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("first answer")}, StopReason: types.StopStop}, nil
			}, func(c types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
				raw, _ := json.Marshal(c.Messages)
				if !strings.Contains(string(raw), "finish the remaining work") {
					t.Errorf("missing Stop reason: %s", raw)
				}
				return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("finished")}, StopReason: types.StopStop}, nil
			})
			a := NewAgent(AgentOptions{InitialState: &AgentState{Model: f.Model()}, StreamFn: f.StreamSimple, Hooks: runner, Child: child})
			result, err := a.PromptText(context.Background(), "go")
			if err != nil || result.Err != nil || result.Turns != 2 {
				t.Fatalf("%+v %v", result, err)
			}
			count, err := os.ReadFile(filepath.Join(cwd, "prompt-count"))
			if err != nil || string(count) != "x" {
				t.Fatalf("synthetic feedback resubmitted prompt: %s %v", count, err)
			}
		})
	}
}

func TestContinueFalseStopsWithoutAnotherProviderCall(t *testing.T) {
	cwd := t.TempDir()
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.ToolCall("work", json.RawMessage(`{}`), "call")), faux.Respond(faux.Text("must not run")))
	a := NewAgent(AgentOptions{InitialState: &AgentState{Model: f.Model(), Tools: []AgentTool{okTool("work")}}, StreamFn: f.StreamSimple, Hooks: testCommandHooks(t, cwd, map[string]string{"PostToolUse": `printf '%s' '{"continue":false,"stopReason":"operator stop"}'`})})
	result, err := a.PromptText(context.Background(), "go")
	if err != nil || result.Err != nil || result.Turns != 1 {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestPostToolUseIncludesContentWhenDetailsAreNull(t *testing.T) {
	for _, details := range []json.RawMessage{nil, json.RawMessage("null")} {
		t.Run(string(details), func(t *testing.T) {
			cwd := t.TempDir()
			runner := testCommandHooks(t, cwd, map[string]string{"PostToolUse": "cat > payload.json"})
			prepared := &preparedToolCall{toolCall: types.NewToolCall("call", "read", json.RawMessage(`{}`)), args: json.RawMessage(`{}`)}
			finalizeExecutedToolCall(context.Background(), AgentContext{}, types.AssistantMessage{}, prepared, executedOutcome{result: AgentToolResult{Content: []types.ContentBlock{types.NewText("actual tool output")}, Details: details}}, AgentLoopConfig{Hooks: runner})
			data, err := os.ReadFile(filepath.Join(cwd, "payload.json"))
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Response struct {
					Content []types.ContentBlock `json:"content"`
				} `json:"tool_response"`
			}
			if err := json.Unmarshal(data, &payload); err != nil || len(payload.Response.Content) != 1 || payload.Response.Content[0].Text != "actual tool output" {
				t.Fatalf("tool response lost: %s err=%v", data, err)
			}
		})
	}
}

func TestChildSkipsSessionLifecycleHooks(t *testing.T) {
	cwd := t.TempDir()
	f := faux.New(faux.Options{})
	f.SetResponses(func(c types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
		raw, _ := json.Marshal(c.Messages)
		if strings.Contains(string(raw), "root-only-context") {
			t.Errorf("root context leaked to child: %s", raw)
		}
		return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("done")}, StopReason: types.StopStop}, nil
	})
	a := NewAgent(AgentOptions{InitialState: &AgentState{Model: f.Model()}, StreamFn: f.StreamSimple, Child: true, Hooks: testCommandHooks(t, cwd, map[string]string{
		"SessionStart": "touch root-start; printf root-only-context",
		"SessionEnd":   "touch root-end",
	})})
	if result, err := a.PromptText(context.Background(), "child task"); err != nil || result.Err != nil {
		t.Fatalf("%+v %v", result, err)
	}
	if err := a.EndSessionHooks(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"root-start", "root-end"} {
		if _, err := os.Stat(filepath.Join(cwd, name)); !os.IsNotExist(err) {
			t.Fatalf("child ran %s: %v", name, err)
		}
	}
}

func TestBlockedPromptReasonNeverReachesModel(t *testing.T) {
	cwd := t.TempDir()
	runner := testCommandHooks(t, cwd, map[string]string{"UserPromptSubmit": `if [ ! -f blocked ]; then touch blocked; printf 'private reason sk-SECRET' >&2; exit 2; fi`})
	f := faux.New(faux.Options{})
	f.SetResponses(func(c types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
		raw, _ := json.Marshal(c.Messages)
		if strings.Contains(string(raw), "sk-SECRET") || strings.Contains(string(raw), "rejected prompt") {
			t.Errorf("blocked content leaked: %s", raw)
		}
		return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("done")}, StopReason: types.StopStop}, nil
	})
	a := NewAgent(AgentOptions{InitialState: &AgentState{Model: f.Model()}, StreamFn: f.StreamSimple, Hooks: runner})
	result, err := a.PromptText(context.Background(), "rejected prompt")
	if err != nil || result.Err == nil || !strings.Contains(result.Err.Error(), "sk-SECRET") || f.CallCount() != 0 {
		t.Fatalf("block invisible: %+v err=%v", result, err)
	}
	result, err = a.PromptText(context.Background(), "accepted prompt")
	if err != nil || result.Err != nil || f.CallCount() != 1 {
		t.Fatalf("next prompt failed: %+v err=%v", result, err)
	}
}

func TestStopHookActiveSurvivesToolContinuation(t *testing.T) {
	cwd := t.TempDir()
	runner := testCommandHooks(t, cwd, map[string]string{"Stop": `input=$(cat); case "$input" in *'"stop_hook_active":true'*) ;; *) printf '%s' '{"decision":"block","reason":"use the tool"}';; esac`})
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("first")), faux.Respond(faux.ToolCall("work", json.RawMessage(`{}`), "call")), faux.Respond(faux.Text("done")))
	a := NewAgent(AgentOptions{InitialState: &AgentState{Model: f.Model(), Tools: []AgentTool{okTool("work")}}, StreamFn: f.StreamSimple, Hooks: runner, Budget: Budget{MaxTurns: 8}})
	result, err := a.PromptText(context.Background(), "go")
	if err != nil || result.Err != nil || result.StopReason != StopCompleted || result.Turns != 3 {
		t.Fatalf("stop guard reset: %+v %v", result, err)
	}
}
