package extensionhost

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hooks"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
	"go.harness.dev/harness/tempest/extension/testdata/goextension"
)

func TestFailedBuiltinRemainsFailureWithExtensions(t *testing.T) {
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	if err := registry.Install(context.Background(), extension.Definition{ID: "sample", Revision: "v1"}); err != nil {
		t.Fatal(err)
	}
	runtime := newRuntime(t, registry)
	runtime.BaseTools = []agent.AgentTool{{Tool: types.Tool{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}, Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		return agent.AgentToolResult{}, errors.New("no such file")
	}}}
	ctx, frame, release, err := runtime.Prepare(context.Background(), agent.AgentContext{})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx = work.WithInvocation(ctx, work.Invocation{CallID: "failed-read", ToolCallID: "read"})
	result, err := runtime.ExecuteTool(ctx, frame.Tools, types.NewToolCall("read", "read", json.RawMessage(`{}`)))
	if err == nil || !result.IsError || result.Content[0].Text != "no such file" {
		t.Fatalf("failure lost: %+v %v", result, err)
	}
	fact, found, err := runtime.readFact(ctx, "failed-read:result")
	if err != nil || !found || fact.Result == nil || !fact.Result.IsError {
		t.Fatalf("failure receipt: %+v %v", fact, err)
	}
}

func TestCanceledCallsAreTerminalAcrossExplicitRecovery(t *testing.T) {
	for _, policy := range []extension.ReplayPolicy{extension.ReplaySafe, extension.ReplayNever} {
		t.Run(string(policy), func(t *testing.T) {
			registry := extension.NewRegistry()
			defer registry.Close(context.Background())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var effects atomic.Int32
			d := extension.Definition{ID: "sample", Revision: "v1", Tools: []extension.Tool{{Name: "slow", Effect: extension.EffectRead, Replay: policy, InputSchema: json.RawMessage(`{"type":"object"}`), Execute: func(context.Context, extension.Call, extension.Invocation) (extension.Result, error) {
				effects.Add(1)
				cancel()
				return extension.Result{}, context.Canceled
			}}}}
			if err := registry.Install(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			runtime := newRuntime(t, registry)
			prepared, frame, release, err := runtime.Prepare(ctx, agent.AgentContext{})
			if err != nil {
				t.Fatal(err)
			}
			prepared = work.WithInvocation(prepared, work.Invocation{CallID: "cancelled", ToolCallID: "slow"})
			if _, err = runtime.ExecuteTool(prepared, frame.Tools, types.NewToolCall("slow", "ext__sample__slow", json.RawMessage(`{}`))); err == nil {
				t.Fatal("cancelled tool succeeded")
			}
			release()
			fact, present, err := runtime.readFact(context.Background(), "cancelled:result")
			if err != nil || !present || fact.Kind != "tool_aborted" {
				t.Fatalf("missing abort: %+v %v", fact, err)
			}
			pending, err := runtime.ResumePending(context.Background())
			if err != nil || len(pending) != 0 || effects.Load() != 1 {
				t.Fatalf("cancel replayed: %+v %v effects=%d", pending, err, effects.Load())
			}
		})
	}
}

func TestRedactionFactsNeverContainIntermediateOutput(t *testing.T) {
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	d := goextension.Definition()
	d.Hooks = append(d.Hooks, extension.Hook{Name: "first", Event: extension.AfterTool, Replay: extension.ReplaySafe, Handle: func(context.Context, extension.HookInput, extension.Invocation) (extension.HookDecision, error) {
		return extension.HookDecision{Kind: "redact", Result: &extension.Result{Content: []extension.Content{{Type: "text", Text: "intermediate-secret"}}, StructuredContent: json.RawMessage(`{"text":"intermediate-secret"}`)}}, nil
	}}, extension.Hook{Name: "final", Event: extension.AfterTool, Replay: extension.ReplaySafe, Handle: func(context.Context, extension.HookInput, extension.Invocation) (extension.HookDecision, error) {
		return extension.HookDecision{Kind: "redact", Result: &extension.Result{Content: []extension.Content{{Type: "text", Text: "redacted"}}, StructuredContent: json.RawMessage(`{"text":"redacted"}`)}}, nil
	}})
	if err := registry.Install(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	runtime := newRuntime(t, registry)
	ctx, frame, release, err := runtime.Prepare(context.Background(), agent.AgentContext{})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err = runtime.ExecuteTool(ctx, frame.Tools, types.NewToolCall("raw", "ext__sample__echo", json.RawMessage(`{"text":"secret"}`))); err != nil {
		t.Fatal(err)
	}
	page, err := runtime.Store.Read(ctx, durable.Query{SessionID: runtime.SessionID, Kind: durable.EntryKind, Namespace: "_host.extensions", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range page.Records {
		if strings.Contains(string(record.Value), "[secret]") || strings.Contains(string(record.Value), "intermediate-secret") {
			t.Fatalf("raw result in %s: %s", record.ID, record.Value)
		}
	}
}

func TestRedactorFailureWithholdsEveryOutputChannel(t *testing.T) {
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	d := goextension.Definition()
	d.Hooks = []extension.Hook{{Name: "redactor", Event: extension.AfterTool, Handle: func(context.Context, extension.HookInput, extension.Invocation) (extension.HookDecision, error) {
		return extension.HookDecision{}, errors.New("redactor dead")
	}}}
	if err := registry.Install(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	runtime := newRuntime(t, registry)
	ctx, frame, release, err := runtime.Prepare(context.Background(), agent.AgentContext{})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	result, err := runtime.ExecuteTool(ctx, frame.Tools, types.NewToolCall("raw", "ext__sample__echo", json.RawMessage(`{"text":"secret"}`)))
	raw, _ := json.Marshal(result)
	if err == nil || !result.IsError || strings.Contains(string(raw), "secret") || len(result.StructuredContent) > 0 || len(result.Details) > 0 {
		t.Fatalf("failed redactor leaked: %s %v", raw, err)
	}
}

func TestCommandAllowCannotAuthorizeLaterExtensionRewrite(t *testing.T) {
	cwd := t.TempDir()
	settings := hooks.Settings{Hooks: map[string][]hooks.Group{"PreToolUse": {{Matcher: "mcp__srv__danger", Hooks: []hooks.Command{{Type: "command", Command: `if grep -q '"mode":"safe"'; then printf '%s' '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}'; fi`}}}}}}
	runner, err := hooks.New(settings, hooks.Input{SessionID: "s", Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	for _, rewrite := range []bool{false, true} {
		registry := extension.NewRegistry()
		defer registry.Close(context.Background())
		d := extension.Definition{ID: "sample", Revision: "v1"}
		if rewrite {
			d.Hooks = []extension.Hook{{Name: "rewrite", Event: extension.BeforeTool, Handle: func(context.Context, extension.HookInput, extension.Invocation) (extension.HookDecision, error) {
				return extension.HookDecision{Kind: "rewrite", Arguments: json.RawMessage(`{"mode":"danger"}`)}, nil
			}}}
		}
		if err = registry.Install(context.Background(), d); err != nil {
			t.Fatal(err)
		}
		runtime := newRuntime(t, registry)
		var executed bool
		runtime.BaseTools = []agent.AgentTool{{Tool: types.Tool{Name: "mcp__srv__danger", Parameters: json.RawMessage(`{"type":"object","properties":{"mode":{"type":"string"}}}`)}, Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			executed = true
			return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("ok")}}, nil
		}}}
		ctx, frame, release, err := runtime.Prepare(context.Background(), agent.AgentContext{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = agent.ExecuteScopedTool(ctx, frame.Tools, types.NewToolCall("c", "mcp__srv__danger", json.RawMessage(`{"mode":"safe"}`)), agent.AgentLoopConfig{Hooks: runner, Extensions: runtime, BeforeToolCall: func(ctx context.Context, in agent.BeforeToolCallContext) *agent.BeforeToolCallResult {
			arguments, marshalErr := json.Marshal(in.Args)
			if marshalErr != nil {
				return &agent.BeforeToolCallResult{Block: true, Reason: marshalErr.Error()}
			}
			outcome := permission.GateDefinition(ctx, permission.ModeDefault, in.ToolCall.Name, in.Definition.Identity, in.Definition.Effect, arguments, &permission.Policy{}, cwd, in.ToolCall.ID)
			return &agent.BeforeToolCallResult{Block: !outcome.Allow, Reason: outcome.Reason}
		}})
		release()
		if rewrite && (err == nil || executed) {
			t.Fatal("rewritten danger inherited allow")
		}
		if !rewrite && (err != nil || !executed) {
			t.Fatalf("unchanged approval broken: %v", err)
		}
	}
}

func TestLargeBuiltinResultUsesDurableBlobStorage(t *testing.T) {
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	if err := registry.Install(context.Background(), extension.Definition{ID: "sample", Revision: "v1"}); err != nil {
		t.Fatal(err)
	}
	runtime := newRuntime(t, registry)
	text := strings.Repeat("large output ", 100000)
	runtime.BaseTools = []agent.AgentTool{{Tool: types.Tool{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}, Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText(text)}}, nil
	}}}
	ctx, frame, release, err := runtime.Prepare(context.Background(), agent.AgentContext{})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx = work.WithInvocation(ctx, work.Invocation{CallID: "large-read", ToolCallID: "read"})
	result, err := runtime.ExecuteTool(ctx, frame.Tools, types.NewToolCall("read", "read", json.RawMessage(`{}`)))
	if err != nil || result.Content[0].Text != text {
		t.Fatalf("large builtin failed with extensions: %v", err)
	}
	fact, found, err := runtime.readFact(ctx, "large-read:result")
	if err != nil || !found || fact.Result == nil || fact.Result.Content[0].Text != text {
		t.Fatalf("large receipt not reusable: found=%t err=%v", found, err)
	}
}

func TestMemoInsertedReportsAtomicWinnerIncludingFalse(t *testing.T) {
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	definition := extension.Definition{ID: "sample", Revision: "v1"}
	if err := registry.Install(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	runtime := newRuntime(t, registry)
	grant := runtime.grant(context.Background(), nil, definition, "tool:memo", "memo-call", 1, nil)
	defer grant.Revoke()
	value, inserted, err := grant.MemoInserted(context.Background(), "choice", json.RawMessage(`false`))
	if err != nil || !inserted || string(value) != "false" {
		t.Fatalf("first insert: %s %t %v", value, inserted, err)
	}
	value, inserted, err = grant.MemoInserted(context.Background(), "choice", json.RawMessage(`null`))
	if err != nil || inserted || string(value) != "false" {
		t.Fatalf("second insert: %s %t %v", value, inserted, err)
	}
}
