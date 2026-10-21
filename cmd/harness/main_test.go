package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/session/journal"
)

func TestResolveOutputFormat(t *testing.T) {
	for _, test := range []struct {
		output, legacy, want string
	}{
		{"", "text", "text"},
		{"json", "text", "json"},
	} {
		got, err := resolveOutputFormat(test.output, test.legacy)
		if err != nil || got != test.want {
			t.Fatalf("resolveOutputFormat(%q, %q) = %q, %v; want %q, nil", test.output, test.legacy, got, err, test.want)
		}
	}
	if _, err := resolveOutputFormat("stream-json", ""); err == nil {
		t.Fatal("stream-json unexpectedly accepted")
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
	cmd := exec.Command(exe, "-p", "create hello", "-faux-script", script)
	cmd.Dir = tmp
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("one-shot run: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "done") {
		t.Fatalf("output = %q, want completion", out)
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
	cwd := "/workspace/project"
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
				store, _, err := openSession(ctx, dir, cwd, "", false, ptypes.Model{ID: test.initial})
				if err != nil {
					t.Fatal(err)
				}
				id = store.ID()
				if err := store.Close(); err != nil {
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
			store, _, err := openSession(ctx, dir, cwd, id, false, ptypes.Model{ID: test.resumed})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
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
		if loaded.LastRun == nil || !loaded.LastRun.Finished || loaded.LastRun.StopReason != "completed" {
			t.Fatalf("journal last run = %+v, want finished completed run", loaded.LastRun)
		}
		if got := len(loaded.Messages); got != 4 {
			t.Fatalf("journal messages = %d, want user, tool call, tool result, answer", got)
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

	t.Run("cost budget is refused for an unknown price", func(t *testing.T) {
		cmd := exec.Command(exe, "-p", "go", "-model", "openai:not-a-cataloged-model@http://127.0.0.1:1/v1", "-max-cost", "1", "-session-dir", filepath.Join(t.TempDir(), "sessions"))
		cmd.Dir = tmp
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "price is unknown") {
			t.Fatalf("output = %q, err = %v; want refusal before the run", out, err)
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
		if entry.Type == entryType {
			count++
		}
	}
	return count
}
