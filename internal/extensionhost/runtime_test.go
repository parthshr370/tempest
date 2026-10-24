package extensionhost

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
	"go.harness.dev/harness/tempest/extension/testdata/goextension"
)

func newRuntime(t *testing.T, registry *extension.Registry) *Runtime {
	t.Helper()
	store, err := journal.Create(context.Background(), t.TempDir(), t.TempDir(), journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	lease, err := registry.Acquire([]extension.Selection{{ID: "sample", Required: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range lease.Definitions() {
		if err := store.RegisterDocuments(context.Background(), definition.Documents); err != nil {
			t.Fatal(err)
		}
	}
	_ = lease.Close(context.Background())
	runtime := &Runtime{Registry: registry, Selected: []extension.Selection{{ID: "sample", Required: true}}, Store: store, SessionID: store.ID(), EnvironmentID: "local:fixture", BasePrompt: "Base"}
	bindExecutor(runtime, permission.ModeDefault, nil)
	return runtime
}
func bindExecutor(runtime *Runtime, mode permission.Mode, policy *permission.Policy) {
	runtime.ExecuteTool = func(ctx context.Context, tools []agent.AgentTool, call types.ContentBlock) (agent.AgentToolResult, error) {
		return agent.ExecuteScopedTool(ctx, tools, call, agent.AgentLoopConfig{Extensions: runtime, BeforeToolCall: func(ctx context.Context, input agent.BeforeToolCallContext) *agent.BeforeToolCallResult {
			arguments, err := json.Marshal(input.Args)
			if err != nil {
				return &agent.BeforeToolCallResult{Block: true, Reason: err.Error()}
			}
			outcome := permission.GateDefinition(ctx, mode, input.ToolCall.Name, input.Definition.Identity, input.Definition.Effect, arguments, policy, "", input.ToolCall.ID)
			return &agent.BeforeToolCallResult{Block: !outcome.Allow, Reason: outcome.Reason}
		}})
	}
}

func TestGoPythonAndJavaScriptShareDurableReceipts(t *testing.T) {
	for _, language := range []string{"go", "python", "javascript"} {
		t.Run(language, func(t *testing.T) {
			registry := extension.NewRegistry()
			t.Cleanup(func() {
				if err := registry.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			if language == "go" {
				if err := registry.Install(context.Background(), goextension.Definition()); err != nil {
					t.Fatal(err)
				}
			} else {
				binary, fixture := "python3", "peer.py"
				if language == "javascript" {
					binary, fixture = "node", "peer.js"
				}
				program, err := exec.LookPath(binary)
				if err != nil {
					t.Skipf("%s peer unavailable: %v", language, err)
				}
				path, err := filepath.Abs(filepath.Join("..", "..", "tempest", "extension", "testdata", fixture))
				if err != nil {
					t.Fatal(err)
				}
				if err = registry.InstallProcess(context.Background(), extension.ProcessOptions{ID: "sample", Argv: []string{program, path}, Capabilities: []string{"read", "commit", "memo", "next"}, ReadTools: []string{"echo"}, SafeHandlers: []string{"tool:echo", "hook:approval", "wrap:brackets"}}); err != nil {
					t.Fatal(err)
				}
			}
			runtime := newRuntime(t, registry)
			ctx, frame, release, err := runtime.Prepare(context.Background(), agent.AgentContext{})
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if !strings.Contains(frame.SystemPrompt, "Extension section") {
				t.Fatal(frame.SystemPrompt)
			}
			ctx = work.WithInvocation(ctx, work.Invocation{CallID: "canonical-call", ToolCallID: "call"})
			result, err := runtime.ExecuteTool(ctx, frame.Tools, types.NewToolCall("call", "ext__sample__echo", json.RawMessage(`{"text":"hello"}`)))
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Content) != 1 || result.Content[0].Text != "[hello]" || string(result.StructuredContent) != `{"text":"[hello]"}` {
				t.Fatalf("different wrapper semantics: %+v", result)
			}
			logical := runtime.callID(ctx, ctx.Value(requestKey{}).(*request), "call")
			for _, memo := range []struct{ handler, key, value string }{{"before_tool:approval", "approval", "true"}, {"tool:echo", "false", "false"}, {"tool:echo", "null", "null"}} {
				value, present, err := runtime.Store.ReadMemo(ctx, durable.MemoScope{SessionID: runtime.SessionID, InvocationID: logical, ExtensionID: "sample", HookID: memo.handler}, memo.key)
				if err != nil || !present || string(value) != memo.value {
					t.Fatalf("memo %s: %s present=%t err=%v", memo.key, value, present, err)
				}
			}
			if _, present, err := runtime.Store.ReadMemo(ctx, durable.MemoScope{SessionID: runtime.SessionID, InvocationID: logical, ExtensionID: "sample", HookID: "tool:echo"}, "missing"); err != nil || present {
				t.Fatalf("missing memo: %t %v", present, err)
			}
			page, err := runtime.Store.Read(ctx, durable.Query{SessionID: runtime.SessionID, Kind: durable.DocumentKind, Namespace: "sample", ID: "state", Limit: 1})
			if err != nil || len(page.Records) != 1 || string(page.Records[0].Value) != `{"count":1}` {
				t.Fatalf("callback commit did not finish while tool awaited: %+v %v", page, err)
			}
			facts, err := runtime.Store.Read(ctx, durable.Query{SessionID: runtime.SessionID, Kind: durable.EntryKind, Namespace: "_host.extensions", Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			kinds := map[string]bool{}
			for _, record := range facts.Records {
				var fact evidence
				if err := json.Unmarshal(record.Value, &fact); err != nil {
					t.Fatal(err)
				}
				kinds[fact.Kind] = true
			}
			for _, kind := range []string{"request_frame", "tool_intent", "tool_result", "hook_intent", "hook_result", "wrapper_intent", "wrapper_result"} {
				if !kinds[kind] {
					t.Fatalf("missing %s receipt", kind)
				}
			}
		})
	}
}

func TestRewritesAreRevalidatedAndCannotBypassGate(t *testing.T) {
	for _, test := range []struct {
		name      string
		arguments string
		mode      permission.Mode
		deny      bool
	}{{"schema", `{"forbidden":true}`, permission.ModeDefault, false}, {"plan", `{"text":"safe"}`, permission.ModePlan, false}, {"deny", `{"text":"safe"}`, permission.ModeBypass, true}} {
		t.Run(test.name, func(t *testing.T) {
			registry := extension.NewRegistry()
			defer registry.Close(context.Background())
			definition := goextension.Definition()
			var calls atomic.Int32
			definition.Tools[0].Execute = func(context.Context, extension.Call, extension.Invocation) (extension.Result, error) {
				calls.Add(1)
				return extension.Result{}, nil
			}
			definition.Hooks = []extension.Hook{{Name: "rewrite", Event: extension.BeforeTool, Handle: func(context.Context, extension.HookInput, extension.Invocation) (extension.HookDecision, error) {
				return extension.HookDecision{Kind: "rewrite", Arguments: json.RawMessage(test.arguments)}, nil
			}}}
			if err := registry.Install(context.Background(), definition); err != nil {
				t.Fatal(err)
			}
			runtime := newRuntime(t, registry)
			policy := &permission.Policy{}
			if test.deny {
				policy.Overrides = map[string]permission.Decision{"sample/echo": permission.DecideDeny}
			}
			bindExecutor(runtime, test.mode, policy)
			ctx, frame, release, err := runtime.Prepare(context.Background(), agent.AgentContext{})
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if _, err = runtime.ExecuteTool(ctx, frame.Tools, types.NewToolCall("call", "ext__sample__echo", json.RawMessage(`{"text":"original"}`))); err == nil {
				t.Fatal("rewrite bypassed schema or gate")
			}
			if calls.Load() != 0 {
				t.Fatal("effect ran before gate")
			}
		})
	}
}

func TestExplicitRecoveryPinsPolicyRevisionEnvironmentAndMemo(t *testing.T) {
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	definition := goextension.Definition()
	var effects atomic.Int32
	original := definition.Tools[0].Execute
	definition.Tools[0].Execute = func(ctx context.Context, call extension.Call, inv extension.Invocation) (extension.Result, error) {
		effects.Add(1)
		if inv.Identity().Attempt != 2 {
			t.Errorf("attempt not fenced: %+v", inv.Identity())
		}
		return original(ctx, call, inv)
	}
	if err := registry.Install(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	runtime := newRuntime(t, registry)
	ctx, frame, release, err := runtime.Prepare(context.Background(), agent.AgentContext{})
	if err != nil {
		t.Fatal(err)
	}
	metadata := frame.Tools[0].Definition
	for _, call := range []struct {
		id                    string
		policy                durable.ReplayPolicy
		revision, environment string
	}{{"never", durable.ReplayNever, metadata.Revision, metadata.Environment}, {"safe", durable.ReplaySafe, metadata.Revision, metadata.Environment}, {"old_revision", durable.ReplaySafe, "v0", metadata.Environment}, {"old_environment", durable.ReplaySafe, metadata.Revision, "destroyed"}} {
		declared := metadata
		declared.Replay = call.policy
		declared.Revision = call.revision
		declared.Environment = call.environment
		arguments := json.RawMessage(`{"text":"recovered"}`)
		declared.ArgumentsHash = hash(arguments)
		if err = runtime.fact(ctx, call.id+":intent", evidence{Kind: "tool_intent", CallID: call.id, Name: "ext__sample__echo", Definition: declared, Arguments: arguments}); err != nil {
			t.Fatal(err)
		}
	}
	release()
	store := runtime.Store.(*journal.Store)
	path := store.Path()
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		loaded, err := journal.Load(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		if loaded == nil || effects.Load() != 0 {
			t.Fatal("read-only recovery executed handlers")
		}
	}
	reopened, _, err := journal.OpenForResume(context.Background(), path, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	runtime.Store = reopened
	outcomes, err := runtime.ResumePending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, outcome := range outcomes {
		states[outcome.CallID] = outcome.State
	}
	if states["never"] != "unknown" || states["safe"] != "completed" || states["old_revision"] != "parked" || states["old_environment"] != "parked" || effects.Load() != 1 {
		t.Fatalf("outcomes %+v effects %d", outcomes, effects.Load())
	}
	if _, err = runtime.ResumePending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if effects.Load() != 1 {
		t.Fatal("settled Safe replayed")
	}
}

func TestGrantCallbackBudgetAndExpiredTokens(t *testing.T) {
	grant := extension.NewGrant(context.Background(), extension.GrantOptions{Identity: extension.InvocationIdentity{SessionID: "session"}, Namespaces: []string{"sample"}, Read: func(context.Context, durable.Query) (durable.Page, error) { return durable.Page{}, nil }})
	for range 1024 {
		if _, err := grant.Read(context.Background(), durable.Query{Namespace: "sample"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := grant.Read(context.Background(), durable.Query{Namespace: "sample"}); err == nil {
		t.Fatal("unbounded callbacks")
	}
	grant.Revoke()
	if _, err := grant.Read(context.Background(), durable.Query{}); !errors.Is(err, extension.ErrExpired) {
		t.Fatal(err)
	}
}

func TestKeyedNestedCallsKeepGateAndCanonicalIdentity(t *testing.T) {
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	var effects atomic.Int32
	child := extension.Tool{Name: "child", Effect: extension.EffectRead, Replay: extension.ReplaySafe, InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"]}`), Execute: func(_ context.Context, call extension.Call, _ extension.Invocation) (extension.Result, error) {
		effects.Add(1)
		if !strings.Contains(call.ID, ":child:") {
			t.Errorf("nested call lost logical identity: %s", call.ID)
		}
		return extension.Result{Content: []extension.Content{{Type: "text", Text: "child result"}}}, nil
	}}
	parent := extension.Tool{Name: "parent", Effect: extension.EffectExecute, InputSchema: json.RawMessage(`{"type":"object"}`), Execute: func(ctx context.Context, _ extension.Call, inv extension.Invocation) (extension.Result, error) {
		first, err := inv.CallTool(ctx, "stable-child", "ext__sample__child", json.RawMessage(`{"value":1}`))
		if err != nil {
			return first, err
		}
		if _, err = inv.CallTool(ctx, "stable-child", "ext__sample__child", json.RawMessage(`{ "value" : 1 }`)); err != nil {
			return first, err
		}
		if _, err = inv.CallTool(ctx, "stable-child", "ext__sample__child", json.RawMessage(`{"value":2}`)); !errors.Is(err, durable.ErrConflict) {
			return first, errors.New("changed nested key was not rejected")
		}
		return first, nil
	}}
	if err := registry.Install(context.Background(), extension.Definition{ID: "sample", Revision: "v1", Tools: []extension.Tool{parent, child}}); err != nil {
		t.Fatal(err)
	}
	runtime := newRuntime(t, registry)
	ctx, frame, release, err := runtime.Prepare(context.Background(), agent.AgentContext{})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx = work.WithInvocation(ctx, work.Invocation{CallID: "parent-call", ToolCallID: "parent"})
	if _, err = runtime.ExecuteTool(ctx, frame.Tools, types.NewToolCall("parent", "ext__sample__parent", json.RawMessage(`{}`))); err != nil {
		t.Fatal(err)
	}
	if effects.Load() != 1 {
		t.Fatalf("nested key repeated effect: %d", effects.Load())
	}
}

func TestTerminalReceiptContainsRedactedModelAndStructuredOutput(t *testing.T) {
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	definition := goextension.Definition()
	definition.Hooks = append(definition.Hooks, extension.Hook{Name: "redact", Event: extension.AfterTool, Replay: extension.ReplaySafe, Handle: func(context.Context, extension.HookInput, extension.Invocation) (extension.HookDecision, error) {
		return extension.HookDecision{Kind: "redact", Result: &extension.Result{Content: []extension.Content{{Type: "text", Text: "redacted"}}, StructuredContent: json.RawMessage(`{"text":"redacted"}`)}}, nil
	}})
	if err := registry.Install(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	runtime := newRuntime(t, registry)
	ctx, frame, release, err := runtime.Prepare(context.Background(), agent.AgentContext{})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx = work.WithInvocation(ctx, work.Invocation{CallID: "redaction-call", ToolCallID: "raw"})
	result, err := runtime.ExecuteTool(ctx, frame.Tools, types.NewToolCall("raw", "ext__sample__echo", json.RawMessage(`{"text":"secret"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Content[0].Text != "redacted" || string(result.StructuredContent) != `{"text":"redacted"}` {
		t.Fatal(result)
	}
	receipt, present, err := runtime.readFact(ctx, "redaction-call:result")
	if err != nil || !present || receipt.Result == nil {
		t.Fatalf("%+v %t %v", receipt, present, err)
	}
	if receipt.Result.Content[0].Text != "redacted" || strings.Contains(string(receipt.Result.StructuredContent), "secret") {
		t.Fatalf("recovery would expose unredacted output: %+v", receipt)
	}
}
