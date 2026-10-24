package hooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func commandSettings(event, command string) Settings {
	return Settings{Hooks: map[string][]Group{event: {{Matcher: "write", Hooks: []Command{{Type: "command", Command: command}}}}}}
}

func TestCommandDecisionsAndContext(t *testing.T) {
	for _, test := range []struct {
		name, event, command, decision, reason, context string
		updated                                         bool
	}{
		{name: "exit2", event: "PreToolUse", command: "printf 'policy says no' >&2; exit 2", decision: "deny", reason: "policy says no"},
		{name: "json deny", event: "PreToolUse", command: `printf '%s' '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"denied"}}'`, decision: "deny", reason: "denied"},
		{name: "json allow input", event: "PreToolUse", command: `printf '%s' '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"path":"changed"}}}'`, decision: "allow", updated: true},
		{name: "legacy", event: "PreToolUse", command: `printf '%s' '{"decision":"block","reason":"legacy"}'`, decision: "deny", reason: "legacy"},
		{name: "post context", event: "PostToolUse", command: `printf '%s' '{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"post advice"}}'`, context: "post advice"},
		{name: "stop", event: "Stop", command: `printf '%s' '{"decision":"block","reason":"keep going"}'`, decision: "deny", reason: "keep going"},
		{name: "nonblocking error", event: "PreToolUse", command: "printf 'failed' >&2; exit 3"},
		{name: "plain prompt", event: "UserPromptSubmit", command: "printf 'prompt context'", context: "prompt context"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner, err := New(commandSettings(test.event, test.command), Input{SessionID: "s", Cwd: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			result, err := runner.Run(context.Background(), Input{Event: test.event, ToolName: "write", ToolInput: json.RawMessage(`{"path":"old"}`)})
			if err != nil {
				t.Fatal(err)
			}
			if result.Decision != test.decision || result.Reason != test.reason {
				t.Fatalf("result=%+v", result)
			}
			if test.updated && string(result.UpdatedInput) != `{"path":"changed"}` {
				t.Fatalf("updated input=%s", result.UpdatedInput)
			}
			if test.context != "" && strings.Join(result.Context, "\n") != test.context {
				t.Fatalf("context=%v", result.Context)
			}
		})
	}
}

func TestHooksReceiveWirePayloadWorkspaceAndEnvironment(t *testing.T) {
	cwd := t.TempDir()
	runner, err := New(commandSettings("PostToolUse", `cat > payload.json; printf '%s\n%s\n%s' "$PWD" "$TEMPEST_PROJECT_DIR" "$CLAUDE_PROJECT_DIR" > environment`), Input{SessionID: "session-1", TranscriptPath: "/session.jsonl", Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(context.Background(), Input{Event: "PostToolUse", ToolName: "write", ToolUseID: "call-1", ToolInput: json.RawMessage(`{"path":"file"}`), ToolResponse: json.RawMessage(`{"content":"tool response"}`)})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(cwd, "payload.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"session_id": "session-1", "transcript_path": "/session.jsonl", "cwd": cwd, "hook_event_name": "PostToolUse", "tool_name": "write", "tool_use_id": "call-1"} {
		if payload[key] != want {
			t.Fatalf("%s=%v, want %s", key, payload[key], want)
		}
	}
	if payload["tool_response"].(map[string]any)["content"] != "tool response" {
		t.Fatalf("tool_response=%v", payload["tool_response"])
	}
	env, err := os.ReadFile(filepath.Join(cwd, "environment"))
	if err != nil {
		t.Fatal(err)
	}
	realCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if string(env) != realCwd+"\n"+cwd+"\n"+cwd {
		t.Fatalf("environment=%s", env)
	}
}

func TestTimeoutKillsDescendantAndCapsOutput(t *testing.T) {
	cwd := t.TempDir()
	settings := commandSettings("PreToolUse", `(sleep 0.3; printf escaped > escaped) & wait`)
	settings.Hooks["PreToolUse"][0].Hooks[0].Timeout = 0.03
	runner, err := New(settings, Input{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	var report Report
	ctx := WithRun(context.Background(), func(_ context.Context, r Report) error { report = r; return nil })
	result, err := runner.Run(ctx, Input{Event: "PreToolUse", ToolName: "write"})
	if err != nil || result.Decision != "" || !report.TimedOut || report.ExitCode == 0 {
		t.Fatalf("result=%+v report=%+v err=%v", result, report, err)
	}
	time.Sleep(400 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(cwd, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("descendant escaped process group: %v", err)
	}
	var capped cappedBuffer
	data := []byte(strings.Repeat("x", outputLimit+10))
	n, err := capped.Write(data)
	if n != len(data) || err != nil || capped.Len() != outputLimit {
		t.Fatalf("cap len=%d written=%d err=%v", capped.Len(), n, err)
	}
}

func TestProjectTrustAndMergeOrder(t *testing.T) {
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, ".harness"), 0700); err != nil {
		t.Fatal(err)
	}
	project := `{"trusted_hook_projects":["` + cwd + `"],"no_hooks":true,"hooks":{"PreToolUse":[{"matcher":"write","hooks":[{"type":"command","command":"printf project > marker"}]}]}}`
	if err := os.WriteFile(filepath.Join(cwd, ".harness", "settings.json"), []byte(project), 0600); err != nil {
		t.Fatal(err)
	}
	user := commandSettings("PreToolUse", "printf user > marker")
	untrusted, skipped, err := ResolveProject(user, cwd)
	if err != nil || len(skipped) != 1 || len(untrusted.Hooks["PreToolUse"]) != 1 {
		t.Fatalf("settings=%+v skipped=%v err=%v", untrusted, skipped, err)
	}
	runner, err := New(untrusted, Input{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), Input{Event: "PreToolUse", ToolName: "write"}); err != nil {
		t.Fatal(err)
	}
	marker, err := os.ReadFile(filepath.Join(cwd, "marker"))
	if err != nil || string(marker) != "user" {
		t.Fatalf("untrusted project command ran: %s %v", marker, err)
	}
	user.TrustedProjects = []string{cwd}
	trusted, skipped, err := ResolveProject(user, cwd)
	if err != nil || len(skipped) != 0 || trusted.NoHooks || len(trusted.Hooks["PreToolUse"]) != 2 {
		t.Fatalf("settings=%+v skipped=%v err=%v", trusted, skipped, err)
	}
	runner, err = New(trusted, Input{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), Input{Event: "PreToolUse", ToolName: "write"}); err != nil {
		t.Fatal(err)
	}
	marker, err = os.ReadFile(filepath.Join(cwd, "marker"))
	if err != nil || string(marker) != "project" {
		t.Fatalf("trusted hook ordering: %s %v", marker, err)
	}
	trusted.NoHooks = true
	runner, err = New(trusted, Input{Cwd: cwd})
	if err != nil || runner != nil {
		t.Fatalf("no_hooks runner=%v err=%v", runner, err)
	}
}

func TestMatcherAndDecisionOrdering(t *testing.T) {
	settings := Settings{Hooks: map[string][]Group{"PreToolUse": {
		{Matcher: "write|edit", Hooks: []Command{{Type: "command", Command: `printf '%s' '{"permissionDecision":"deny","reason":"deny wins"}'`}}},
		{Matcher: "^write$", Hooks: []Command{{Type: "command", Command: `printf '%s' '{"permissionDecision":"ask"}'`}, {Type: "command", Command: `printf '%s' '{"permissionDecision":"allow"}'`}}},
	}}}
	runner, err := New(settings, Input{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), Input{Event: "PreToolUse", ToolName: "write"})
	if err != nil || result.Decision != "deny" || result.Reason != "deny wins" {
		t.Fatalf("%+v %v", result, err)
	}
	result, err = runner.Run(context.Background(), Input{Event: "PreToolUse", ToolName: "overwrite"})
	if err != nil || result.Decision != "" {
		t.Fatalf("simple matcher must be exact: %+v %v", result, err)
	}
}

func TestWaitDelayPreservesDecision(t *testing.T) {
	runner, err := New(commandSettings("PreToolUse", `printf '%s' '{"permissionDecision":"deny","reason":"guard"}'; sleep 3 &`), Input{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), Input{Event: "PreToolUse", ToolName: "write"})
	if err != nil || result.Decision != "deny" || result.Reason != "guard" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestExitTwoStillReadsContinueFalse(t *testing.T) {
	runner, err := New(commandSettings("PreToolUse", `printf '%s' '{"continue":false,"stopReason":"operator stop"}'; printf refusal >&2; exit 2`), Input{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), Input{Event: "PreToolUse", ToolName: "write"})
	if err != nil || !result.Stop || result.StopReason != "operator stop" || result.Decision != "deny" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestWireModeDefaultsAndMatcherWarnings(t *testing.T) {
	if DefaultTimeout("Stop") != 600*time.Second || DefaultTimeout("UserPromptSubmit") != 30*time.Second || DefaultTimeout("SessionEnd") != 1500*time.Millisecond {
		t.Fatal("incorrect event defaults")
	}
	settings := commandSettings("PreToolUse", "cat > payload")
	settings.Hooks["PreToolUse"][0].Matcher = "Bash"
	if warnings := settings.UnmatchedToolMatchers([]string{"bash", "write"}); len(warnings) != 1 {
		t.Fatalf("warnings=%v", warnings)
	}
	settings.Hooks["PreToolUse"][0].Matcher = "bash"
	cwd := t.TempDir()
	runner, err := New(settings, Input{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	runner.PermissionMode = func() string { return "bypass" }
	if _, err := runner.Run(context.Background(), Input{Event: "PreToolUse", ToolName: "bash"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(cwd, "payload"))
	if err != nil || !strings.Contains(string(raw), `"permission_mode":"bypassPermissions"`) {
		t.Fatalf("payload=%s err=%v", raw, err)
	}
}

func TestCancelNeverSettlesAsEmptySuccess(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(map[bool]string{true: "before", false: "during"}[before], func(t *testing.T) {
			cwd := t.TempDir()
			runner, err := New(commandSettings("UserPromptSubmit", "touch started; sleep 10; exit 2"), Input{Cwd: cwd})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if before {
				cancel()
			} else {
				go func() {
					for {
						if _, err := os.Stat(filepath.Join(cwd, "started")); err == nil {
							cancel()
							return
						}
						select {
						case <-ctx.Done():
							return
						case <-time.After(time.Millisecond):
						}
					}
				}()
			}
			_, err = runner.Run(ctx, Input{Event: "UserPromptSubmit"})
			if err != context.Canceled {
				t.Fatalf("cancel err=%v", err)
			}
		})
	}
}

func TestProjectResolutionAllowsMissingWorkspace(t *testing.T) {
	settings := commandSettings("PreToolUse", "exit 0")
	resolved, skipped, err := ResolveProject(settings, filepath.Join(t.TempDir(), "not-created"))
	if err != nil || len(skipped) != 0 || len(resolved.Hooks["PreToolUse"]) != 1 {
		t.Fatalf("missing workspace lost operator hooks: settings=%+v skipped=%v err=%v", resolved, skipped, err)
	}
}

func TestResumedEmptySessionSkipsStartup(t *testing.T) {
	cwd := t.TempDir()
	runner, err := New(commandSettings("SessionStart", "touch unexpected-start"), Input{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	runner.MarkResumed()
	if _, err := runner.Start(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "unexpected-start")); !os.IsNotExist(err) {
		t.Fatalf("resumed empty session ran startup: %v", err)
	}
}
