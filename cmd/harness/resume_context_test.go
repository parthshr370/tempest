package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// -resume runs the session in its journal's workspace, so context discovery
// runs there too: the AGENTS.md of the directory the command starts in never
// reaches the resumed session's prompt.
func TestResumeDiscoversContextInTheSessionsWorkspace(t *testing.T) {
	for _, assignment := range os.Environ() {
		key, _, _ := strings.Cut(assignment, "=")
		if strings.HasPrefix(key, "HARNESS_") || strings.HasPrefix(key, "LYRA_") {
			t.Setenv(key, "")
		}
	}
	t.Setenv("HOME", t.TempDir())
	sessionDir := t.TempDir()
	script := filepath.Join(t.TempDir(), "provider.json")
	if err := os.WriteFile(script, []byte(`[{"text":"done"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, other := t.TempDir(), t.TempDir()
	for dir, mark := range map[string]string{workspace: "WORKSPACE_CONTEXT_MARK", other: "OTHER_CONTEXT_MARK"} {
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(mark), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(workspace)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-session-dir", sessionDir, "-faux-script", script, "-output-format", "json", "-p", "hi"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, &stderr)
	}
	var report runReportOutput
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || report.SessionID == "" {
		t.Fatalf("report %q: %v", stdout.String(), err)
	}

	t.Chdir(other)
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"-session-dir", sessionDir, "-faux-script", script, "-output-format", "json", "-resume", report.SessionID, "-p", "again"}, &stdout, &stderr); code != 0 {
		t.Fatalf("resume exit=%d stderr=%s", code, &stderr)
	}
	sawWorkspace := false
	err := filepath.WalkDir(sessionDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte("OTHER_CONTEXT_MARK")) {
			t.Errorf("%s holds the starting directory's AGENTS.md", path)
		}
		sawWorkspace = sawWorkspace || bytes.Contains(data, []byte("WORKSPACE_CONTEXT_MARK"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawWorkspace {
		t.Fatal("no request prompt held the session workspace's AGENTS.md")
	}
}
