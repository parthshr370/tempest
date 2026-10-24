package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/mcpclient"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest"
	"go.harness.dev/harness/tempest/durable"
)

func TestResolveOutputFormat(t *testing.T) {
	for _, test := range []struct {
		output, legacy, want string
	}{
		{"", "text", "text"},
		{"json", "text", "json"},
		{"stream-json", "json", "stream-json"},
		{"", "stream-json", "stream-json"},
	} {
		got, err := resolveOutputFormat(test.output, test.legacy)
		if err != nil || got != test.want {
			t.Fatalf("resolveOutputFormat(%q, %q) = %q, %v; want %q, nil", test.output, test.legacy, got, err, test.want)
		}
	}
	if _, err := resolveOutputFormat("invalid", ""); err == nil {
		t.Fatal("invalid output unexpectedly accepted")
	}
}

func TestBuildPermissionPolicy(t *testing.T) {
	tests := []struct {
		name  string
		allow []string
		deny  []string
		want  permission.Decision
	}{
		{name: "deny wins", allow: []string{"task"}, deny: []string{"task"}, want: permission.DecideDeny},
		{name: "allow only", allow: []string{"task"}, want: permission.DecideAllow},
		{name: "deny only", deny: []string{"task"}, want: permission.DecideDeny},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := buildPermissionPolicy(test.allow, test.deny).Overrides["task"]
			if got != test.want {
				t.Fatalf("policy override = %v; want %v", got, test.want)
			}
		})
	}
}
func TestCommandRetryDefaultsAndDisabledConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, key := range []string{
		"HARNESS_RETRY_MAX_ATTEMPTS",
		"HARNESS_RETRY_BASE_DELAY_MS",
		"HARNESS_RETRY_BACKOFF_CAP_MS",
		"HARNESS_RETRY_MAX_DELAY_MS",
		"HARNESS_RETRY_JITTER_MIN",
		"HARNESS_RETRY_JITTER_MAX",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("HARNESS_RETRY_ENABLED", "")
	opts := commandOptions{}
	if err := resolveCommandConfig(&opts); err != nil {
		t.Fatal(err)
	}
	if got := opts.Config.Retry(); !got.Enabled || got.MaxAttempts != 11 || got.BaseDelayMS != 500 || got.BackoffCapMS != 8000 || got.MaxDelayMS != 300000 || got.JitterMin != .75 || got.JitterMax != 1 {
		t.Fatalf("default command retry settings = %+v", got)
	}
	t.Setenv("HARNESS_RETRY_ENABLED", "false")
	disabled := commandOptions{}
	if err := resolveCommandConfig(&disabled); err != nil {
		t.Fatal(err)
	}
	if disabled.Config.Retry().Enabled {
		t.Fatalf("disabled retry remained enabled: %+v", disabled.Config.Retry())
	}
}

func TestCLIHelpPrintsZeroValueFlagsWithoutPanic(t *testing.T) {
	for _, args := range [][]string{{"-help"}, {"serve", "-help"}, {"mcp", "-help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr strings.Builder
			run(args, &stdout, &stderr)
			if text := stderr.String(); strings.Contains(text, "panic") || !strings.Contains(text, "-allow-tool") || !strings.Contains(text, "-deny-tool") {
				t.Fatalf("help output = %q", text)
			}
		})
	}
	if got := (skillPathsFlag{}).String(); got != "" {
		t.Fatalf("zero flag String() = %q", got)
	}
}

func TestTopLevelHelpNamesTempestAndListsSubcommands(t *testing.T) {
	var stdout, stderr strings.Builder
	run([]string{"-help"}, &stdout, &stderr)
	text := stderr.String()
	for _, want := range []string{"tempest -p <prompt>", "\n  serve ", "\n  mcp ", "\n  doctor ", "\n  version "} {
		if !strings.Contains(text, want) {
			t.Fatalf("help lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "harness") {
		t.Fatalf("help still names the binary harness:\n%s", text)
	}
}

func TestUnknownAllowDenyToolNamesWarnButMCPNamesDoNot(t *testing.T) {
	var stderr strings.Builder
	warnUnknownToolNames(&stderr, "-allow-tool", []string{"bash", "mcp__everything__echo", "nosuchtool"})
	text := stderr.String()
	if strings.Count(text, "warning:") != 1 || !strings.Contains(text, `"nosuchtool"`) || strings.Contains(text, `"bash"`) || strings.Contains(text, `"mcp__everything__echo"`) {
		t.Fatalf("warnings = %q", text)
	}
}

func TestMCPStartupFailureHintNamesServerAndHowToDisable(t *testing.T) {
	err := fmt.Errorf("open: %w", &mcpclient.StartupError{Server: "broken", Phase: "transport", Err: errors.New("exec: not found")})
	if hint := mcpStartupHint(err); !strings.Contains(hint, `"broken"`) || !strings.Contains(hint, `"disabled": true`) || !strings.Contains(hint, `-mcp-config ""`) {
		t.Fatalf("hint = %q", hint)
	}
	if hint := mcpStartupHint(errors.New("other")); hint != "" {
		t.Fatalf("non-MCP error got hint %q", hint)
	}
}

func TestCLITextOutput(t *testing.T) {
	tmp := t.TempDir()
	exe := filepath.Join(tmp, "harness-test")
	build := exec.Command("go", "build", "-o", exe, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build harness: %v\n%s", err, out)
	}
	script := filepath.Join(tmp, "faux.json")
	if err := os.WriteFile(script, []byte(`[{"text":"done"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(tmp, "sessions")
	cmd := exec.Command(exe, "-p", "create hello", "-faux-script", script, "-session-dir", sessionDir)
	cmd.Dir = tmp
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("one-shot run: %v\n%s\n%s", err, out, stderr.String())
	}
	if string(out) != "done\n" {
		t.Fatalf("stdout = %q, want only completion text", out)
	}
	id := loadOnlyJournal(t, sessionDir).Header.ID
	hint := "session: " + id + " (resume with -resume " + id + ")\n"
	if !strings.HasSuffix(stderr.String(), hint) || strings.Count(stderr.String(), "session: ") != 1 {
		t.Fatalf("stderr = %q, want one final session hint %q", stderr.String(), hint)
	}
}

func TestCLIDiscoversSkillAssetAndFailsBeforeProviderOnFatalContext(t *testing.T) {
	tmp := t.TempDir()
	exe := filepath.Join(tmp, "harness-test")
	build := exec.Command("go", "build", "-o", exe, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build harness: %v\n%s", err, out)
	}
	agentDir := filepath.Join(tmp, "agent")
	skillDir := filepath.Join(tmp, ".harness", "skills", "alpha")
	for _, path := range []string{agentDir, skillDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: alpha\ndescription: alpha skill\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "reference.md"), []byte("CLI DISCOVERED ASSET"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(tmp, "faux.json")
	if err := os.WriteFile(script, []byte(`[
		{"toolCalls":[{"id":"read-asset","name":"read","arguments":{"path":"skill://alpha/reference.md"}}]},
		{"text":"CLI DISCOVERED ASSET"}
	]`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-p", "read the alpha asset", "-faux-script", script, "-agent-dir", agentDir)
	cmd.Dir = tmp
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("skill asset one-shot run: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "CLI DISCOVERED ASSET") {
		t.Fatalf("skill asset output = %q", out)
	}

	if err := os.Mkdir(filepath.Join(agentDir, "AGENTS.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(exe, "-p", "must not call provider", "-faux-script", script, "-agent-dir", agentDir)
	cmd.Dir = tmp
	out, err = cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "context discovery error") {
		t.Fatalf("fatal context output = %q, err = %v", out, err)
	}
	if strings.Contains(string(out), "CLI DISCOVERED ASSET") {
		t.Fatalf("provider was called after fatal discovery error: %q", out)
	}
}

func TestCLIRejectsEmptySkillFlag(t *testing.T) {
	tmp := t.TempDir()
	exe := filepath.Join(tmp, "harness-test")
	build := exec.Command("go", "build", "-o", exe, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build harness: %v\n%s", err, out)
	}
	cmd := exec.Command(exe, "-skill", "")
	cmd.Dir = tmp
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "config skill") {
		t.Fatalf("empty -skill output = %q, err = %v", out, err)
	}
}

func TestResolveCommandConfigRejectsResumeContinueConflict(t *testing.T) {
	opts := commandOptions{Resume: "session-id", Continue: true}
	if err := resolveCommandConfig(&opts); err == nil || !strings.Contains(err.Error(), "-resume and -continue") {
		t.Fatalf("resolveCommandConfig conflict error = %v", err)
	}
}

func TestOpenSessionRecordsResumedModelChanges(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	open := func(dir, resume, model string) (*tempest.Session, error) {
		f := faux.New(faux.Options{})
		return openSession(ctx, tempest.Options{
			SessionDir: dir,
			Cwd:        cwd,
			New:        &tempest.NewSession{Model: model},
			Stack: &tempest.StackConfig{
				Model:    ptypes.Model{ID: model, Provider: "faux"},
				StreamFn: f.StreamSimple,
			},
		}, resume, false)
	}
	for _, test := range []struct {
		name        string
		initial     string
		resumed     string
		withModel   bool
		wantChanges int
	}{
		{name: "same model", initial: "model-a", resumed: "model-a", withModel: true, wantChanges: 1},
		{name: "changed model", initial: "model-a", resumed: "model-b", withModel: true, wantChanges: 2},
		{name: "no prior model", resumed: "model-a", wantChanges: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			var id string
			if test.withModel {
				store, err := open(dir, "", test.initial)
				if err != nil {
					t.Fatal(err)
				}
				id = store.ID()
				if err := store.Close(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				store, err := journal.Create(ctx, dir, cwd, journal.Options{})
				if err != nil {
					t.Fatal(err)
				}
				id = store.ID()
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			store, err := open(dir, id, test.resumed)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(ctx); err != nil {
				t.Fatal(err)
			}
			path, err := journal.Resolve(ctx, dir, cwd, id)
			if err != nil {
				t.Fatal(err)
			}
			if got := countEntryType(t, path, "model_change"); got != test.wantChanges {
				t.Fatalf("model-change entries = %d, want %d", got, test.wantChanges)
			}
		})
	}
}

// runReportOutput is the subset of the json run report the tests assert on.
type runReportOutput struct {
	SessionID  string   `json:"session_id"`
	StopReason string   `json:"stop_reason"`
	Error      string   `json:"error"`
	Turns      int      `json:"turns"`
	CostUSD    *float64 `json:"cost_usd"`
	ElapsedMS  *int64   `json:"elapsed_ms"`
	Text       string   `json:"text"`
	Message    struct {
		Role string `json:"role"`
	} `json:"message"`
}

func TestCLIRunLifecycle(t *testing.T) {
	tmp := t.TempDir()
	exe := filepath.Join(tmp, "harness-test")
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("build harness: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(tmp, "note.txt"), []byte("note body"), 0o644); err != nil {
		t.Fatal(err)
	}
	readThenAnswer := `[{"toolCalls":[{"id":"read-1","name":"read","arguments":{"path":"note.txt"}}]},{"text":"answered"}]`
	readOnly := `[{"toolCalls":[{"id":"read-1","name":"read","arguments":{"path":"note.txt"}}]}]`

	run := func(t *testing.T, script string, args ...string) (runReportOutput, string, int) {
		t.Helper()
		scriptPath := filepath.Join(t.TempDir(), "faux.json")
		if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
		sessionDir := filepath.Join(t.TempDir(), "sessions")
		cmd := exec.Command(exe, append([]string{"-p", "go", "-faux-script", scriptPath, "-session-dir", sessionDir, "-output-format", "json"}, args...)...)
		cmd.Dir = tmp
		var stderr strings.Builder
		cmd.Stderr = &stderr
		stdout, err := cmd.Output()
		exitCode := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if err != nil {
			t.Fatalf("run harness: %v", err)
		}
		var report runReportOutput
		if err := json.Unmarshal(stdout, &report); err != nil {
			t.Fatalf("decode run report %q: %v\nstderr: %s", stdout, err, stderr.String())
		}
		return report, sessionDir, exitCode
	}

	t.Run("completes when the assistant stops calling tools", func(t *testing.T) {
		report, sessionDir, code := run(t, readThenAnswer)
		if code != 0 || report.StopReason != "completed" || report.Turns != 2 || report.Text != "answered" || report.CostUSD == nil || report.ElapsedMS == nil {
			t.Fatalf("exit %d report %+v, want completed run with cost and elapsed", code, report)
		}
		loaded := loadOnlyJournal(t, sessionDir)
		if report.SessionID != loaded.Header.ID || loaded.LastRun == nil || !loaded.LastRun.Finished || loaded.LastRun.StopReason != "completed" {
			t.Fatalf("journal last run = %+v, want finished completed run", loaded.LastRun)
		}
		if got := len(loaded.Messages); got != 4 {
			t.Fatalf("journal messages = %d, want user, tool call, tool result, answer", got)
		}
	})

	t.Run("resume and continue append new prompts to the same session", func(t *testing.T) {
		_, sessionDir, code := run(t, readThenAnswer)
		if code != 0 {
			t.Fatalf("initial run exit = %d", code)
		}
		id := loadOnlyJournal(t, sessionDir).Header.ID
		paths, err := filepath.Glob(filepath.Join(sessionDir, "*", "*.jsonl"))
		if err != nil || len(paths) != 1 {
			t.Fatalf("journal paths = %v, err = %v", paths, err)
		}
		name := filepath.Base(paths[0])
		for index, flagArgs := range [][]string{{"-resume", id}, {"-resume", name}, {"-resume", strings.TrimSuffix(name, ".jsonl")}, {"-continue"}} {
			args := append([]string{"-session-dir", sessionDir}, flagArgs...)
			report, _, code := run(t, `[{"text":"resumed answer"}]`, args...)
			if code != 0 || report.Text != "resumed answer" || report.SessionID != id {
				t.Fatalf("resume args %v: exit %d, report %+v", flagArgs, code, report)
			}
			loaded := loadOnlyJournal(t, sessionDir)
			if want := 4 + 2*(index+1); len(loaded.Messages) != want {
				t.Fatalf("resume args %v: messages=%d, want %d", flagArgs, len(loaded.Messages), want)
			}
			if loaded.Header.ID != id || loaded.Messages[len(loaded.Messages)-2].Role() != "user" {
				t.Fatalf("resume args %v didn't preserve the session and add a prompt", flagArgs)
			}
		}
	})

	t.Run("attached files remain readable through the attachment tool", func(t *testing.T) {
		script := `[{"toolCalls":[{"id":"attachment-read","name":"attachment","arguments":{"op":"read","id":"note.txt"}}]},{"text":"read attachment"}]`
		report, sessionDir, code := run(t, script, "-attach", filepath.Join(tmp, "note.txt"))
		if code != 0 || report.Text != "read attachment" {
			t.Fatalf("attachment run exit %d, report %+v", code, report)
		}
		loaded := loadOnlyJournal(t, sessionDir)
		if len(loaded.Messages) != 4 {
			t.Fatalf("attachment messages = %d, want 4", len(loaded.Messages))
		}
		result, ok := loaded.Messages[2].(ptypes.ToolResultMessage)
		if !ok || result.IsError || len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, "note body") {
			t.Fatalf("attachment tool result = %#v", loaded.Messages[2])
		}
	})

	t.Run("plan uses faux and exposes no mutating tools", func(t *testing.T) {
		// A non-empty todo list: if plan mode ever exposed todo_write, it would write progress.md.
		script := `[{"toolCalls":[{"id":"write-plan","name":"write","arguments":{"path":"plan-output.txt","content":"must not write"}},{"id":"todo-plan","name":"todo_write","arguments":{"todos":[{"content":"must not checkpoint","status":"pending"}]}}]},{"text":"read-only plan"}]`
		report, sessionDir, code := run(t, script, "-plan", "-enable-task", "-max-cost", "1", "-plan-model", "openai:not-a-cataloged-model@http://127.0.0.1:1/v1")
		if code != 0 || report.StopReason != "completed" || report.Text != "read-only plan" {
			t.Fatalf("plan exit %d, report %+v", code, report)
		}
		loaded := loadOnlyJournal(t, sessionDir)
		for _, message := range loaded.Messages {
			if result, ok := message.(ptypes.ToolResultMessage); ok {
				if !result.IsError || len(result.Content) != 1 || !strings.HasPrefix(result.Content[0].Text, "Tool "+result.ToolName+" not found") {
					t.Fatalf("plan tool result = %#v, want tool unavailable", result)
				}
			}
		}
		journalPaths, err := filepath.Glob(filepath.Join(sessionDir, "*", "*.jsonl"))
		if err != nil || len(journalPaths) != 1 {
			t.Fatalf("journals = %v, %v; want one", journalPaths, err)
		}
		// todo_write keeps its progress file beside the journal.
		for _, path := range []string{filepath.Join(tmp, "plan-output.txt"), filepath.Join(journalPaths[0]+".d", "progress.md")} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("plan created %s: %v", path, err)
			}
		}
	})

	t.Run("clean finish on the final allowed turn completes", func(t *testing.T) {
		for _, test := range []struct{ script, turns string }{
			{`[{"text":"OK"}]`, "1"},
			{readThenAnswer, "2"},
		} {
			report, sessionDir, code := run(t, test.script, "-max-turns", test.turns)
			if code != 0 || report.StopReason != "completed" {
				t.Fatalf("max-turns %s: exit %d, report %+v", test.turns, code, report)
			}
			if loaded := loadOnlyJournal(t, sessionDir); loaded.LastRun == nil || loaded.LastRun.StopReason != "completed" {
				t.Fatalf("journal last run = %+v, want completed", loaded.LastRun)
			}
		}
	})

	t.Run("max turns stops after the turn's tools finish", func(t *testing.T) {
		report, sessionDir, code := run(t, readThenAnswer, "-max-turns", "1")
		if code != 0 || report.StopReason != "max_turns" || report.Turns != 1 || report.Message.Role != "toolResult" {
			t.Fatalf("exit %d report %+v, want max_turns ending on the tool result", code, report)
		}
		if loaded := loadOnlyJournal(t, sessionDir); loaded.LastRun == nil || loaded.LastRun.StopReason != "max_turns" {
			t.Fatalf("journal last run = %+v, want max_turns", loaded.LastRun)
		}
	})

	t.Run("provider failure exits non-zero with the error", func(t *testing.T) {
		report, _, code := run(t, readOnly)
		if code != 1 || report.StopReason != "error" || report.Error == "" {
			t.Fatalf("exit %d report %+v, want error stop with exit 1", code, report)
		}
	})

	t.Run("bash timeout flag kills the command group", func(t *testing.T) {
		script := `[{"toolCalls":[{"id":"timeout","name":"bash","arguments":{"command":"printf '%s' $$ > timeout-pgid; exec sleep 30"}}]},{"text":"handled timeout"}]`
		started := time.Now()
		report, sessionDir, code := run(t, script, "-bash-timeout", "1s", "-permission-mode", "bypass")
		if elapsed := time.Since(started); elapsed >= 2*time.Second {
			t.Fatalf("timeout run took %s, want less than 2s", elapsed)
		}
		if code != 0 || report.Text != "handled timeout" {
			t.Fatalf("timeout run exit %d, report %+v", code, report)
		}
		loaded := loadOnlyJournal(t, sessionDir)
		if len(loaded.Messages) != 4 {
			t.Fatalf("timeout messages = %d, want 4", len(loaded.Messages))
		}
		toolResult, ok := loaded.Messages[2].(ptypes.ToolResultMessage)
		if !ok || !toolResult.IsError || len(toolResult.Content) != 1 || !strings.Contains(toolResult.Content[0].Text, "Command timed out after 1 seconds") {
			t.Fatalf("timeout result = %#v", loaded.Messages[2])
		}
		pgidBytes, err := os.ReadFile(filepath.Join(tmp, "timeout-pgid"))
		if err != nil {
			t.Fatal(err)
		}
		pgid, err := strconv.Atoi(string(pgidBytes))
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("process group %d still exists: %v", pgid, err)
		}
	})

	t.Run("cost budget is refused for an unknown price", func(t *testing.T) {
		cmd := exec.Command(exe, "-p", "go", "-model", "openai:not-a-cataloged-model@http://127.0.0.1:1/v1", "-max-cost", "1", "-session-dir", filepath.Join(t.TempDir(), "sessions"))
		cmd.Dir = tmp
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "price is unknown") {
			t.Fatalf("output = %q, err = %v; want refusal before the run", out, err)
		}
	})

	t.Run("non-finite cost is refused before session creation", func(t *testing.T) {
		for _, limit := range []string{"NaN", "+Inf", "-Inf"} {
			sessionDir := filepath.Join(t.TempDir(), "sessions")
			cmd := exec.Command(exe, "-p", "go", "-model", "openai:not-a-cataloged-model@http://127.0.0.1:1/v1", "-max-cost", limit, "-session-dir", sessionDir)
			cmd.Dir = tmp
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "budget max cost must be finite") {
				t.Fatalf("max-cost %s: output = %q, err = %v", limit, out, err)
			}
			if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
				t.Fatalf("session directory created for max-cost %s: %v", limit, err)
			}
		}
	})
}

// loadOnlyJournal loads the single session journal written under sessionDir.
func loadOnlyJournal(t *testing.T, sessionDir string) *journal.Loaded {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(sessionDir, "*", "*.jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("journals under %s = %v (err %v), want exactly one", sessionDir, paths, err)
	}
	loaded, err := journal.Load(context.Background(), paths[0])
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func countEntryType(t *testing.T, path, entryType string) int {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(contents)), "\n")[1:] {
		var entry struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Type == "commit" {
			envelope, err := durable.DecodeEnvelope(path, []byte(line))
			if err != nil {
				t.Fatal(err)
			}
			for _, change := range envelope.Commit.Changes {
				if change.Kind != durable.EntryKind || change.Namespace != "_tempest/journal" {
					continue
				}
				if err := json.Unmarshal(change.Value, &entry); err != nil {
					t.Fatal(err)
				}
				if entry.Type == entryType {
					count++
				}
			}
			continue
		}
		if entry.Type == entryType {
			count++
		}
	}
	return count
}
