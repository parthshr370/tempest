package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteEnvironmentBinaryNeverUsesHostWorkspace(t *testing.T) {
	directory, host, remote := t.TempDir(), t.TempDir(), t.TempDir()
	binary := filepath.Join(directory, "tempest")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build tempest: %v\n%s", err, output)
	}
	script := filepath.Join(directory, "faux.json")
	responses := `[{"toolCalls":[{"id":"write","name":"write","arguments":{"path":"note.txt","content":"remote workspace bytes\n"}}]},{"toolCalls":[{"id":"command","name":"bash","arguments":{"command":"printf remote-command > command.txt"}}]},{"toolCalls":[{"id":"read","name":"read","arguments":{"path":"note.txt"}}]},{"toolCalls":[{"id":"edit","name":"edit","arguments":{"path":"note.txt","edits":[{"old_text":"remote workspace bytes","new_text":"edited remote bytes"}]}}]},{"text":"remote proof complete"}]`
	if err := os.WriteFile(script, []byte(responses), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, "note.txt"), []byte("host must remain unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	daemonArgv, err := json.Marshal([]string{binary, "env", "-id", "fake-remote", "-cwd", remote})
	if err != nil {
		t.Fatal(err)
	}
	var cleanEnvironment []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "HOME" || strings.HasPrefix(name, "HARNESS_") || strings.HasPrefix(name, "LYRA_") || strings.HasSuffix(name, "_API_KEY") {
			continue
		}
		cleanEnvironment = append(cleanEnvironment, entry)
	}
	cleanEnvironment = append(cleanEnvironment, "HOME="+directory)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "-p", "exercise remote tools", "-faux-script", script, "-session-dir", filepath.Join(directory, "sessions"), "-mcp-config", "", "-permission-mode", "bypass", "-environment", "fake-remote", "-environment-command", string(daemonArgv), "-output-format", "json")
	command.Dir = host
	command.Env = cleanEnvironment
	var commandStderr bytes.Buffer
	command.Stderr = &commandStderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("remote binary: %v\n%s\n%s", err, output, commandStderr.String())
	}
	if !bytes.Contains(output, []byte("remote proof complete")) {
		t.Fatalf("missing final answer: %s", output)
	}
	for name, want := range map[string]string{"note.txt": "edited remote bytes\n", "command.txt": "remote-command"} {
		contents, err := os.ReadFile(filepath.Join(remote, name))
		if err != nil || string(contents) != want {
			t.Fatalf("remote %s: %q %v", name, contents, err)
		}
	}
	contents, err := os.ReadFile(filepath.Join(host, "note.txt"))
	if err != nil || string(contents) != "host must remain unchanged" {
		t.Fatalf("host changed: %q %v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(host, "command.txt")); !os.IsNotExist(err) {
		t.Fatalf("exec used host cwd: %v", err)
	}
	paths, err := filepath.Glob(filepath.Join(directory, "sessions", "*", "*.jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("journals: %v %v", paths, err)
	}
	journal, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, evidence := range []string{"fake-remote", "generation", "tool_started", "remote workspace bytes", "remote-command"} {
		if !bytes.Contains(journal, []byte(evidence)) {
			t.Fatalf("journal missing %q", evidence)
		}
	}
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(output, &created); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(directory, ".config", "harness", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0700); err != nil {
		t.Fatal(err)
	}
	registry, _ := json.Marshal(map[string]any{"environments": map[string]any{"fake-remote": map[string]any{"argv": []string{binary, "env", "-id", "fake-remote", "-cwd", remote}}}})
	if err := os.WriteFile(settings, registry, 0600); err != nil {
		t.Fatal(err)
	}
	hostOnlyRule := "HOST-ONLY-PROJECT-RULE-7731"
	if err := os.WriteFile(filepath.Join(host, "AGENTS.md"), []byte(hostOnlyRule), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(host, ".harness"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, ".harness", "settings.json"), []byte("invalid host provider config"), 0600); err != nil {
		t.Fatal(err)
	}
	resume := exec.CommandContext(ctx, binary, "-resume", created.SessionID, "-p", "resume remotely", "-faux-script", script, "-session-dir", filepath.Join(directory, "sessions"), "-mcp-config", "", "-permission-mode", "bypass", "-trust-project-providers", "-output-format", "json")
	resume.Dir, resume.Env = host, cleanEnvironment
	if resumed, err := resume.CombinedOutput(); err != nil {
		t.Fatalf("remote resume used host providers or rules: %v\n%s", err, resumed)
	}
	if err := filepath.WalkDir(filepath.Join(directory, "sessions"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(data, []byte(hostOnlyRule)) {
				t.Errorf("host project rule leaked into remote state: %s", path)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("real binary plus fake remote daemon: remote file/edit/exec paths fenced; host files unchanged; environment identity journaled")
	rejected := exec.CommandContext(ctx, binary, "-p", "must reject local LSP fallback", "-faux-script", script, "-session-dir", filepath.Join(directory, "rejected"), "-mcp-config", "", "-enable-lsp", "-environment", "fake-remote", "-environment-command", string(daemonArgv))
	rejected.Dir = host
	rejected.Env = cleanEnvironment
	refusal, err := rejected.CombinedOutput()
	if err == nil || !bytes.Contains(refusal, []byte("environment capability unavailable: lsp")) {
		t.Fatalf("remote LSP was not explicitly rejected: %v\n%s", err, refusal)
	}
	t.Run("symlink cwd request retry and continue", func(t *testing.T) {
		real, link := filepath.Join(directory, "real"), filepath.Join(directory, "link")
		if err := os.Mkdir(real, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		var localEnv []string
		for _, entry := range cleanEnvironment {
			if !strings.HasPrefix(entry, "PWD=") {
				localEnv = append(localEnv, entry)
			}
		}
		localEnv = append(localEnv, "PWD="+link)
		localScript := filepath.Join(directory, "local-faux.json")
		if err := os.WriteFile(localScript, []byte(`[{"text":"ok"}]`), 0600); err != nil {
			t.Fatal(err)
		}
		run := func(args ...string) string {
			command := exec.CommandContext(ctx, binary, append(args, "-faux-script", localScript, "-session-dir", filepath.Join(directory, "local-sessions"), "-mcp-config", "", "-permission-mode", "bypass", "-output-format", "json")...)
			command.Dir, command.Env = link, localEnv
			var diagnostics bytes.Buffer
			command.Stderr = &diagnostics
			output, err := command.Output()
			if err != nil {
				t.Fatalf("local continuation: %v\n%s\n%s", err, output, diagnostics.String())
			}
			var result struct {
				SessionID string `json:"session_id"`
			}
			if err := json.Unmarshal(output, &result); err != nil {
				t.Fatal(err)
			}
			return result.SessionID
		}
		id := run("-p", "same prompt", "-request-id", "same-request")
		if repeated := run("-p", "same prompt", "-request-id", "same-request"); repeated != id {
			t.Fatalf("request retry created %s instead of reusing %s", repeated, id)
		}
		if continued := run("-continue", "-p", "next"); continued != id {
			t.Fatalf("continue opened %s instead of %s", continued, id)
		}
	})
}
