package permission

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/tools"
)

func TestParseMode(t *testing.T) {
	for _, value := range []string{"", "default", "plan", "acceptEdits", "bypass"} {
		if _, err := ParseMode(value); err != nil {
			t.Fatalf("ParseMode(%q): %v", value, err)
		}
	}
	if _, err := ParseMode("invalid"); err == nil {
		t.Fatal("expected invalid mode error")
	}
}

func TestParseDecision(t *testing.T) {
	for value, want := range map[string]Decision{
		"allow":  DecideAllow,
		"deny":   DecideDeny,
		"prompt": DecidePrompt,
	} {
		got, err := ParseDecision(value)
		if err != nil || got != want {
			t.Fatalf("ParseDecision(%q) = %v, %v; want %v, nil", value, got, err, want)
		}
	}
	if _, err := ParseDecision("invalid"); err == nil {
		t.Fatal("expected invalid decision error")
	}
}

func TestClassifyToolTiers(t *testing.T) {
	tests := map[tools.ToolName]Tier{
		tools.ReadTool:       TierRead,
		tools.BashTool:       TierExec,
		tools.EditTool:       TierWrite,
		tools.WriteTool:      TierWrite,
		tools.GrepTool:       TierRead,
		tools.FindTool:       TierRead,
		tools.LsTool:         TierRead,
		tools.TodoTool:       TierWrite,
		tools.TaskTool:       TierExec,
		tools.SkillTool:      TierRead,
		tools.WebSearch:      TierRead,
		tools.WebFetch:       TierRead,
		tools.AttachmentTool: TierRead,
		tools.JobsTool:       TierExec,
		tools.WaitTool:       TierRead,
		tools.CancelTool:     TierExec,
		tools.LogsTool:       TierRead,
	}
	if len(tests) != len(tools.AllToolNames) {
		t.Fatalf("tier expectations = %d; built-in tool names = %d", len(tests), len(tools.AllToolNames))
	}
	for tool := range tools.AllToolNames {
		if _, ok := tests[tool]; !ok {
			t.Errorf("missing tier expectation for built-in tool %q", tool)
		}
	}
	for tool, want := range tests {
		if got := ClassifyTool(string(tool)); got != want {
			t.Errorf("ClassifyTool(%q) = %v; want %v", tool, got, want)
		}
	}
	if got := ClassifyTool("unknown_tool"); got != TierExec {
		t.Errorf("ClassifyTool(%q) = %v; want %v", "unknown_tool", got, TierExec)
	}
}

func TestGateModeTierMatrix(t *testing.T) {
	tests := []struct {
		name       string
		mode       Mode
		tool       string
		args       json.RawMessage
		wantAllow  bool
		wantReason string
	}{
		{name: "bypass destructive bash", mode: ModeBypass, tool: "bash", args: commandArgs("rm -rf /tmp/example"), wantAllow: true},
		{name: "default edit", mode: ModeDefault, tool: "edit", wantAllow: true},
		{name: "accept edits edit", mode: ModeAcceptEdits, tool: "edit", wantAllow: true},
		{name: "default destructive bash", mode: ModeDefault, tool: "bash", args: commandArgs("rm -rf /tmp/example"), wantReason: "Command blocked by destructive-command guard (deletions, existing-file overwrites, in-place edits, and destructive git operations require approval; appends and new-file writes are allowed): rm -rf /tmp/example"},
		{name: "default safe bash", mode: ModeDefault, tool: "bash", args: commandArgs("ls"), wantAllow: true},
		{name: "default read", mode: ModeDefault, tool: "read", wantAllow: true},
		{name: "plan write", mode: ModePlan, tool: "write", wantReason: "Plan mode: write is disabled because it can mutate the workspace"},
		{name: "plan edit", mode: ModePlan, tool: "edit", wantReason: "Plan mode: edit is disabled because it can mutate the workspace"},
		{name: "plan safe bash", mode: ModePlan, tool: "bash", args: commandArgs("git status"), wantAllow: true},
		{name: "plan unsafe bash", mode: ModePlan, tool: "bash", args: commandArgs("python x.py"), wantReason: "Plan mode: command blocked (not allowlisted). Disable plan mode or use a read-only command. Command: python x.py"},
		{name: "plan task", mode: ModePlan, tool: "task", wantReason: "Plan mode: task is disabled because it can mutate the workspace"},
		{name: "plan read", mode: ModePlan, tool: "read", wantAllow: true},
		{name: "plan unknown tool", mode: ModePlan, tool: "unknown_tool", wantReason: "Plan mode: unknown_tool is disabled because it can mutate the workspace"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out := Gate(context.Background(), test.mode, test.tool, test.args, &Policy{})
			allow, reason := out.Allow, out.Reason
			if allow != test.wantAllow {
				t.Fatalf("Gate() allow = %v, reason %q; want %v", allow, reason, test.wantAllow)
			}
			if !test.wantAllow && reason != test.wantReason {
				t.Fatalf("Gate() reason = %q; want %q", reason, test.wantReason)
			}
			if test.wantAllow && reason != "" {
				t.Fatalf("Gate() reason = %q; want empty", reason)
			}
		})
	}
}

func TestGatePolicyOverrides(t *testing.T) {
	t.Run("allow", func(t *testing.T) {
		policy := Policy{Overrides: map[string]Decision{"task": DecideAllow}}
		out := Gate(context.Background(), ModePlan, "task", nil, &policy)
		allow, reason := out.Allow, out.Reason
		if !allow || reason != "" {
			t.Fatalf("allow override = allow %v, reason %q", allow, reason)
		}
	})

	t.Run("deny", func(t *testing.T) {
		policy := Policy{Overrides: map[string]Decision{"edit": DecideDeny}}
		out := Gate(context.Background(), ModeDefault, "edit", nil, &policy)
		allow, reason := out.Allow, out.Reason
		if allow || reason != "Tool edit denied by policy" {
			t.Fatalf("deny override = allow %v, reason %q", allow, reason)
		}
	})

	t.Run("prompt", func(t *testing.T) {
		prompter := &testPrompter{approved: true}
		policy := Policy{Overrides: map[string]Decision{"task": DecidePrompt}, Prompter: prompter}
		out := Gate(context.Background(), ModeDefault, "task", nil, &policy)
		allow, reason := out.Allow, out.Reason
		if !allow || reason != "" {
			t.Fatalf("prompt override = allow %v, reason %q", allow, reason)
		}
		if prompter.request.Tool != "task" || prompter.request.Tier != TierExec || len(prompter.request.Args) != 0 {
			t.Fatalf("prompt request = %+v", prompter.request)
		}
	})

	t.Run("safety wins over allow", func(t *testing.T) {
		policy := Policy{Overrides: map[string]Decision{"bash": DecideAllow}}
		out := Gate(context.Background(), ModeDefault, "bash", commandArgs("rm -rf /tmp/example"), &policy)
		allow, reason := out.Allow, out.Reason
		wantReason := "Command blocked by destructive-command guard (deletions, existing-file overwrites, in-place edits, and destructive git operations require approval; appends and new-file writes are allowed): rm -rf /tmp/example"
		if allow || reason != wantReason {
			t.Fatalf("destructive bash = allow %v, reason %q; want reason %q", allow, reason, wantReason)
		}
	})
}

func TestGateHeadlessPromptDenies(t *testing.T) {
	policy := Policy{Overrides: map[string]Decision{"task": DecidePrompt}}
	out := Gate(context.Background(), ModeDefault, "task", nil, &policy)
	allow, reason := out.Allow, out.Reason
	if allow || reason != "Tool task requires approval but no interactive prompt is available" {
		t.Fatalf("headless prompt = allow %v, reason %q", allow, reason)
	}

	prompter := &testPrompter{approved: true}
	policy.Prompter = prompter
	out = Gate(context.Background(), ModeDefault, "task", nil, &policy)
	allow, reason = out.Allow, out.Reason
	if !allow || reason != "" {
		t.Fatalf("approved prompt = allow %v, reason %q", allow, reason)
	}
}

func TestGateCanceledContextDenies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := Gate(ctx, ModeBypass, "bash", commandArgs("rm -rf /tmp/example"), &Policy{})
	allow, reason := out.Allow, out.Reason
	if allow || reason != "Operation aborted" {
		t.Fatalf("canceled gate = allow %v, reason %q", allow, reason)
	}
}

func TestCommandFromArgs(t *testing.T) {
	if got := commandFromArgs(commandArgs("git status")); got != "git status" {
		t.Fatalf("commandFromArgs() = %q", got)
	}
	if got := commandFromArgs(json.RawMessage("{")); got != "" {
		t.Fatalf("commandFromArgs(malformed) = %q", got)
	}
	if got := commandFromArgs(json.RawMessage(`{"command":42}`)); got != "" {
		t.Fatalf("commandFromArgs(non-string) = %q", got)
	}
}

type testPrompter struct {
	approved bool
	request  PromptRequest
}

func (p *testPrompter) Approve(_ context.Context, req PromptRequest) (Choice, error) {
	p.request = req
	if p.approved {
		return AllowOnce, nil
	}
	return RejectOnce, nil
}

func commandArgs(command string) json.RawMessage {
	args, err := json.Marshal(struct {
		Command string `json:"command"`
	}{Command: command})
	if err != nil {
		panic(err)
	}
	return args
}

func TestDestructiveCommandPatterns(t *testing.T) {
	for _, command := range []string{
		"git reset --hard",
		"git clean -fd",
		"npm install",
		"grep x file > /etc/passwd",
		"sudo ls",
		"mkfs /dev/sda1",
		"mkfs.ext4 /dev/sda1",
		":(){ :|:& };:",
		"bomb(){ bomb|bomb & }; bomb",
		"dd if=/tmp/image of=/dev/sda",
	} {
		if !IsDestructiveCommand(command) {
			t.Fatalf("%q should be destructive", command)
		}
	}
	for _, command := range []string{"ls -la", "rg TODO", "git diff --stat"} {
		if !IsSafeCommand(command) {
			t.Fatalf("%q should be safe", command)
		}
	}
	for _, command := range []string{"git cleanup-branch", "printf mkfsnot", "cat mkfs.sh"} {
		if IsDestructiveCommand(command) {
			t.Fatalf("%q should not be destructive", command)
		}
	}
}

func TestIsDestructiveCommandRedirectTargets(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, name := range []string{"file.txt", "out.log"} {
		if err := os.WriteFile(name, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, command := range []string{
		"cat workflow.json 2>&1 | head -5",
		"foo 2>/dev/null",
		"foo &>/dev/null",
		"echo x 1>&2",
		"foo >/dev/null",
	} {
		if IsDestructiveCommand(command) {
			t.Fatalf("%q should not be destructive", command)
		}
	}
	for _, command := range []string{
		"echo x > file.txt",
		"echo x > /etc/passwd",
		"foo &>out.log",
	} {
		if !IsDestructiveCommand(command) {
			t.Fatalf("%q should be destructive", command)
		}
	}
}

func TestPlanExtractionAndCompletion(t *testing.T) {
	message := "Here is the plan.\n\n**Plan:**\n1. Create the `workflow.json` file\n2) Run tests and verify output\n3. `/skip command`\n4. - invalid bullet\n"
	items := ExtractTodoItems(message)
	if len(items) != 2 {
		t.Fatalf("items = %+v", items)
	}
	if items[0].Step != 1 || items[0].Text != "Workflow.json file" || items[0].Completed {
		t.Fatalf("item 1 = %+v", items[0])
	}
	if items[1].Step != 2 || items[1].Text != "Tests and verify output" {
		t.Fatalf("item 2 = %+v", items[1])
	}
	if got := ExtractDoneSteps("ok [DONE:2] [done:99]"); len(got) != 2 || got[0] != 2 || got[1] != 99 {
		t.Fatalf("done steps = %v", got)
	}
	if marked := MarkCompletedSteps("[DONE:2]", items); marked != 1 || !items[1].Completed {
		t.Fatalf("marked=%d items=%+v", marked, items)
	}
}

func TestPlanCollector(t *testing.T) {
	collector := NewPlanCollector()
	collector.Observe("Plan:\n1. Build the UI\n2. Test the UI")
	collector.Observe("Finished [DONE:1]")
	items := collector.Items()
	if len(items) != 2 || !items[0].Completed || items[1].Completed {
		t.Fatalf("items = %+v", items)
	}
	formatted := FormatPlan(items)
	if !strings.Contains(formatted, "[x] 1. Build the UI") || !strings.Contains(formatted, "[ ] 2. Test the UI") {
		t.Fatalf("formatted = %q", formatted)
	}
}

type choicePrompter func(context.Context, PromptRequest) (Choice, error)

func (f choicePrompter) Approve(ctx context.Context, req PromptRequest) (Choice, error) {
	return f(ctx, req)
}

func TestGatePromptSourcesAndAlwaysOverrides(t *testing.T) {
	for _, tc := range []struct {
		name   string
		choice Choice
		err    error
		source Source
		allow  bool
	}{
		{"allow", AllowOnce, nil, SourcePrompt, true},
		{"deny", RejectOnce, nil, SourcePrompt, false},
		{"cancelled", "", nil, SourcePromptCancelled, false},
		{"timeout", "", context.DeadlineExceeded, SourcePromptTimeout, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := &Policy{Prompter: choicePrompter(func(_ context.Context, req PromptRequest) (Choice, error) {
				if req.ToolCallID != "c" || string(req.Args) != `{"command":"rm x"}` {
					t.Errorf("request=%+v", req)
				}
				return tc.choice, tc.err
			})}
			out := Gate(context.Background(), ModeDefault, "bash", commandArgs("rm x"), policy, "c")
			if out.Allow != tc.allow || out.Source != tc.source || !out.Prompted {
				t.Fatalf("outcome=%+v", out)
			}
		})
	}
	for _, choice := range []Choice{AllowAlways, RejectAlways} {
		calls := 0
		policy := &Policy{Overrides: map[string]Decision{"edit": DecidePrompt}, Prompter: choicePrompter(func(context.Context, PromptRequest) (Choice, error) { calls++; return choice, nil })}
		for range 2 {
			out := Gate(context.Background(), ModeDefault, "edit", nil, policy, "c")
			if out.Allow != (choice == AllowAlways) {
				t.Fatalf("choice=%s outcome=%+v", choice, out)
			}
		}
		if calls != 1 {
			t.Fatalf("choice=%s prompted %d times", choice, calls)
		}
	}
}

func TestGuardAlwaysChoicesApplyOnlyOnce(t *testing.T) {
	for _, choice := range []Choice{AllowAlways, RejectAlways} {
		t.Run(string(choice), func(t *testing.T) {
			calls := 0
			policy := &Policy{Prompter: choicePrompter(func(_ context.Context, req PromptRequest) (Choice, error) {
				calls++
				if req.Reason == "" {
					t.Errorf("guard ask has no reason: %+v", req)
				}
				return choice, nil
			})}
			for _, command := range []string{"rm first", "rm -rf second"} {
				out := Gate(context.Background(), ModeDefault, "bash", commandArgs(command), policy)
				if out.Allow != (choice == AllowAlways) || !out.Prompted || out.Source != SourcePrompt {
					t.Fatalf("command=%q outcome=%+v", command, out)
				}
			}
			out := Gate(context.Background(), ModeDefault, "bash", commandArgs("echo harmless"), policy)
			if !out.Allow || out.Prompted || out.Source != SourceMode || calls != 2 {
				t.Fatalf("harmless outcome=%+v prompts=%d", out, calls)
			}
			if len(policy.Overrides) != 0 || len(policy.promptOverrides) != 0 {
				t.Fatalf("guard ask changed policy: %+v", policy)
			}
		})
	}
}

func TestSessionAlwaysChoicesNeverSkipGuard(t *testing.T) {
	for _, choice := range []Choice{AllowAlways, RejectAlways} {
		t.Run(string(choice), func(t *testing.T) {
			calls := 0
			policy := &Policy{
				Overrides: map[string]Decision{"bash": DecidePrompt},
				Prompter: choicePrompter(func(context.Context, PromptRequest) (Choice, error) {
					calls++
					return choice, nil
				}),
			}
			for _, command := range []string{"echo first", "rm -rf second", "rmdir third", "echo last"} {
				out := Gate(context.Background(), ModeDefault, "bash", commandArgs(command), policy)
				if out.Allow != (choice == AllowAlways) {
					t.Fatalf("command=%q outcome=%+v", command, out)
				}
				if command != "echo last" && (!out.Prompted || out.Source != SourcePrompt) {
					t.Fatalf("command=%q skipped ask: %+v", command, out)
				}
				if command == "echo last" && (out.Prompted || out.Source != SourceOverride) {
					t.Fatalf("session choice lost: %+v", out)
				}
			}
			if calls != 3 {
				t.Fatalf("prompted %d times, want 3", calls)
			}
		})
	}
}
