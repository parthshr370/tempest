package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAgentFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// agents list prints each loaded agent's name, model and source, and exits 1
// naming the file and field of an invalid one.
func TestAgentsListShowsSourcesAndFailsOnInvalidFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project, agentDir := t.TempDir(), t.TempDir()
	// Agent files are listed from the resolved repository root.
	project, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	native := filepath.Join(project, ".harness", "agents", "reviewer.md")
	writeAgentFile(t, native, "---\nname: reviewer\ndescription: Reviews diffs\nmodel: haiku\n---\nReview.\n")
	user := filepath.Join(agentDir, "agents", "writer.md")
	writeAgentFile(t, user, "---\nname: writer\ndescription: Writes docs\n---\nWrite.\n")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"agents", "list", "-agent-dir", agentDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("agents list exit %d: %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "NAME") {
		t.Fatalf("agents list output:\n%s", stdout.String())
	}
	for i, want := range [][]string{{"reviewer", "smol", native}, {"writer", "default", user}} {
		if fields := strings.Fields(lines[i+1]); len(fields) != 3 || fields[0] != want[0] || fields[1] != want[1] || fields[2] != want[2] {
			t.Errorf("row %d = %q, want %v", i+1, lines[i+1], want)
		}
	}

	invalid := filepath.Join(project, ".claude", "agents", "broken.md")
	writeAgentFile(t, invalid, "---\nname: broken\ndescription: Broken\ntools: [read, teleport]\n---\nBody.\n")
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"agents", "list", "-agent-dir", agentDir}, &stdout, &stderr); code != 1 {
		t.Fatalf("agents list with an invalid file exit %d, want 1", code)
	}
	if msg := stderr.String(); !strings.Contains(msg, invalid) || !strings.Contains(msg, "tools") || !strings.Contains(msg, "teleport") {
		t.Fatalf("error does not name the file and field: %s", msg)
	}
	if !strings.Contains(stdout.String(), "reviewer") {
		t.Fatalf("valid agents still listed: %s", stdout.String())
	}
}

// agents list run below the repository root still lists the repository's
// agent files.
func TestAgentsListFromSubdirectoryUsesRepoRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAgentFile(t, filepath.Join(project, ".claude", "agents", "compat.md"), "---\nname: compat-only\ndescription: Repo agent\n---\nWork.\n")
	sub := filepath.Join(project, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"agents", "list", "-agent-dir", t.TempDir()}, &stdout, &stderr); code != 0 {
		t.Fatalf("agents list exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "compat-only") {
		t.Fatalf("agents list in a subdirectory lost the repository agent:\n%s", stdout.String())
	}
}

// A repository agent directory that links outside the repository is skipped,
// and the error names it.
func TestAgentsListNamesASkippedDirectory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeAgentFile(t, filepath.Join(outside, "spy.md"), "---\nname: spy\ndescription: Outside agent\n---\nWork.\n")
	linked := filepath.Join(project, ".harness", "agents")
	if err := os.MkdirAll(filepath.Dir(linked), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, linked); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"agents", "list", "-agent-dir", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Fatalf("agents list exit %d, want 1: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "spy") || !strings.Contains(stderr.String(), linked) {
		t.Fatalf("stdout=%q stderr=%q: want the outside agent skipped and %s named", stdout.String(), stderr.String(), linked)
	}
}
