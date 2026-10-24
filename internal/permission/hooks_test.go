package permission

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/hooks"
	"go.harness.dev/harness/internal/work"
)

type hookPrompter func(context.Context, PromptRequest) (Choice, error)

func (p hookPrompter) Approve(ctx context.Context, req PromptRequest) (Choice, error) {
	return p(ctx, req)
}

func TestHookAllowCannotOverrideHardDenials(t *testing.T) {
	ctx := hooks.WithToolDecision(context.Background(), "allow", "")
	cwd := t.TempDir()
	for _, test := range []struct {
		name, tool, args string
		mode             Mode
		policy           Policy
	}{
		{name: "plan", tool: "write", args: `{"path":"file","content":"x"}`, mode: ModePlan},
		{name: "guard", tool: "bash", args: `{"command":"rm -rf target"}`, mode: ModeDefault},
		{name: "explicit deny", tool: "read", args: `{"path":"file"}`, mode: ModeDefault, policy: Policy{Overrides: map[string]Decision{"read": DecideDeny}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			out := GateInDirectory(ctx, test.mode, test.tool, json.RawMessage(test.args), &test.policy, cwd)
			if out.Allow {
				t.Fatalf("hook bypassed hard denial: %+v", out)
			}
		})
	}
}

func TestHookAskForcesOneChildApprovalAndNotification(t *testing.T) {
	asks, notifications := 0, 0
	ctx := hooks.WithToolDecision(context.Background(), "ask", "confirm hook action")
	ctx = hooks.WithNotification(ctx, func(context.Context, string) error { notifications++; return nil })
	policy := &Policy{Prompter: hookPrompter(func(_ context.Context, req PromptRequest) (Choice, error) {
		asks++
		if req.Reason != "confirm hook action" {
			t.Errorf("reason=%s", req.Reason)
		}
		return AllowOnce, nil
	})}
	ceiling := work.Ceiling{Mode: string(ModeDefault), Tools: []string{"write"}}
	out := GateChild(ctx, ceiling, ModeDefault, []string{"write"}, policy, "write", json.RawMessage(`{"path":"file","content":"x"}`), ChildScope{SessionID: "child", RunID: "run", CallID: "call", ToolCallID: "tool", Cwd: t.TempDir()})
	if !out.Allow || asks != 1 || notifications != 1 {
		t.Fatalf("out=%+v asks=%d notifications=%d", out, asks, notifications)
	}
	headless := GateInDirectory(hooks.WithToolDecision(context.Background(), "ask", "headless hook reason"), ModeBypass, "read", json.RawMessage(`{"path":"file"}`), nil, t.TempDir())
	if headless.Allow || !strings.Contains(headless.Reason, "headless hook reason") {
		t.Fatalf("ask bypassed headless refusal: %+v", headless)
	}
}

func TestHookAllowPreservesBypassAndExplicitPrompt(t *testing.T) {
	ctx := hooks.WithToolDecision(context.Background(), "allow", "")
	out := GateInDirectory(ctx, ModeBypass, "bash", json.RawMessage(`{"command":"rm -rf target"}`), nil, t.TempDir())
	if !out.Allow || out.Source != SourceBypass {
		t.Fatalf("bypass narrowed: %+v", out)
	}
	policy := &Policy{Overrides: map[string]Decision{"write": DecidePrompt}}
	out = GateInDirectory(ctx, ModeDefault, "write", json.RawMessage(`{"path":"file"}`), policy, t.TempDir())
	if out.Allow {
		t.Fatalf("explicit ask skipped: %+v", out)
	}
}

func TestHookSettingsRequirePerCallApproval(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, ".harness"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".harness", filepath.Join(cwd, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".harness/settings.json", filepath.Join(cwd, "dangling")); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ tool, args string }{
		{"write", `{"path":".harness/settings.local.json","content":"{}"}`},
		{"edit", `{"path":"alias/settings.json"}`},
		{"write", `{"path":"dangling","content":"{}"}`},
		{"hashline", `{"input":"[.harness/settings.json#ABCD]\nREM\n"}`},
		{"hashline", `{"input":"[file#ABCD]\nMV .harness/settings.local.json\n"}`},
		{"bash", `{"command":"printf '{}' > .harness/settings.local.json"}`},
		{"bash", `{"command":"printf '{}' >> alias/settings.local.json"}`},
		{"bash", `{"command":"rm -rf .harness"}`},
		{"bash", `{"command":"cp payload alias/settings.json"}`},
		{"bash", `{"command":"cd .harness && printf '{}' > settings.json"}`},
		{"eval", `{"language":"py","code":"open('.harness/settings.json','w').write('{}')"}`},
		{"bash", `{"command":"rm -rf ~/.config/harness"}`},
		{"bash", `{"command":"rm -rf \"$HOME/.config/harness\""}`},
		{"write_stdin", `{"job_id":"shell","text":"printf '{}' > .harness/settings.local.json\n"}`},
		{"write", `{"path":"` + filepath.Join(os.Getenv("HOME"), ".config", "harness", "settings.json") + `"}`},
	} {
		for _, mode := range []Mode{ModeDefault, ModeAcceptEdits, ModeBypass} {
			t.Run(string(mode)+"/"+test.tool+"/"+test.args, func(t *testing.T) {
				calls := 0
				policy := &Policy{Overrides: map[string]Decision{test.tool: DecideAllow}, Prompter: hookPrompter(func(_ context.Context, req PromptRequest) (Choice, error) {
					calls++
					if !strings.Contains(req.Reason, "hook settings") {
						t.Errorf("missing reason: %+v", req)
					}
					return AllowAlways, nil
				})}
				ctx := hooks.WithToolDecision(context.Background(), "allow", "")
				for range 2 {
					if out := GateInDirectory(ctx, mode, test.tool, json.RawMessage(test.args), policy, cwd); !out.Allow {
						t.Fatalf("approval refused: %+v", out)
					}
				}
				if calls != 2 {
					t.Fatalf("settings approval remembered: %d", calls)
				}
				if out := GateInDirectory(ctx, mode, test.tool, json.RawMessage(test.args), nil, cwd); out.Allow {
					t.Fatalf("headless settings change allowed: %+v", out)
				}
			})
		}
	}
	if out := GateInDirectory(context.Background(), ModeDefault, "bash", json.RawMessage(`{"command":"cat .harness/settings.json"}`), nil, cwd); !out.Allow {
		t.Fatalf("read denied: %+v", out)
	}
}
