//go:build unix

package discovery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const outsideMarker = "SYNTHETIC_OUTSIDE_SECRET"

// outsideFile writes a file outside every test repository and returns its path.
func outsideFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	writeAll(t, path, content)
	return path
}

// writeAll writes path, creating its directories.
func writeAll(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, path, content)
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func fifo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

// returnsInTime runs fn and fails the test when it does not return in time: a read
// that blocks on a FIFO never returns.
func returnsInTime(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s blocked", what)
	}
}

// A context file, or an import, that leaves the workspace through a symlink
// or a ../ path is not read: nested discovery may not widen what read may see.
func TestNestedRefusesContextOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	secret := outsideFile(t, "secret.md", outsideMarker)
	writeAll(t, filepath.Join(root, "link", "x.go"), "package link\n")
	symlink(t, secret, filepath.Join(root, "link", "AGENTS.md"))
	writeAll(t, filepath.Join(root, "imp", "x.go"), "package imp\n")
	writeAll(t, filepath.Join(root, "imp", "AGENTS.md"), "rules @"+secret+" and @./leak.md and @../../"+filepath.Base(filepath.Dir(secret))+"/secret.md")
	symlink(t, secret, filepath.Join(root, "imp", "leak.md"))

	nested := NewNested(NestedOptions{Root: root})
	for _, dir := range []string{"link", "imp"} {
		blocks, _, err := nested.Surface(context.Background(), filepath.Join(root, dir, "x.go"), false, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.Join(blocks, "\n"), outsideMarker) {
			t.Fatalf("reading %s/x.go surfaced a file outside the workspace: %q", dir, blocks)
		}
	}
}

// A FIFO named like a context file is not opened: opening it would block the
// tool call until something writes to it.
func TestNestedSkipsNonRegularContextFiles(t *testing.T) {
	root := t.TempDir()
	writeAll(t, filepath.Join(root, "pipe", "x.go"), "package pipe\n")
	fifo(t, filepath.Join(root, "pipe", "AGENTS.md"))
	returnsInTime(t, "nested discovery of a FIFO", func() {
		if _, _, err := NewNested(NestedOptions{Root: root}).Surface(context.Background(), filepath.Join(root, "pipe", "x.go"), false, false, nil); err != nil {
			t.Error(err)
		}
	})
}

// gitRepo makes a repository root with a nested working directory.
func gitRepo(t *testing.T) (root, sub string) {
	t.Helper()
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub = filepath.Join(root, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	return root, sub
}

// A repository context file that is a symlink out of the repository is not
// loaded at startup either.
func TestDiscoverRefusesProjectContextOutsideRepo(t *testing.T) {
	root, _ := gitRepo(t)
	symlink(t, outsideFile(t, "AGENTS.md", outsideMarker), filepath.Join(root, "AGENTS.md"))
	inputs, err := Discover(context.Background(), Options{Cwd: root, AgentDir: t.TempDir(), HomeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range inputs.ContextFiles {
		if strings.Contains(file.Content, outsideMarker) {
			t.Fatalf("startup loaded %s from outside the repository", file.Path)
		}
	}
}

// A repository context file or rule that imports a file outside the
// repository, through a symlink or a ../ path, keeps the import literal:
// startup may not show what read itself may not.
func TestDiscoverKeepsStartupImportsInsideRepo(t *testing.T) {
	root, sub := gitRepo(t)
	secret := outsideFile(t, "secret.md", outsideMarker)
	symlink(t, secret, filepath.Join(root, "leak.md"))
	escape, err := filepath.Rel(sub, secret)
	if err != nil {
		t.Fatal(err)
	}
	writeAll(t, filepath.Join(root, "AGENTS.override.md"), "override @./leak.md")
	writeAll(t, filepath.Join(sub, "AGENTS.md"), "sub @"+escape+" @"+secret)
	writeAll(t, filepath.Join(root, ".harness", "RULES.md"), "rule @../leak.md")
	inputs, err := Discover(context.Background(), Options{Cwd: sub, AgentDir: t.TempDir(), HomeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var loaded []string
	for _, file := range inputs.ContextFiles {
		loaded = append(loaded, file.Content)
	}
	loaded = append(loaded, inputs.GenericRules...)
	want := []string{"override @./leak.md", "sub @" + escape + " @" + secret, "rule @../leak.md"}
	if strings.Join(loaded, "\n") != strings.Join(want, "\n") {
		t.Fatalf("startup loaded %q, want the imports literal: %q", loaded, want)
	}
}

// The user's own context file and rules are read inside the home
// directory: a ~ import expands, an import outside home stays literal.
func TestDiscoverUserImportsStayInsideHome(t *testing.T) {
	home := t.TempDir()
	agentDir := filepath.Join(home, ".tempest")
	secret := outsideFile(t, "secret.md", outsideMarker)
	writeAll(t, filepath.Join(home, "notes.md"), "NOTES")
	writeAll(t, filepath.Join(agentDir, "AGENTS.md"), "user @~/notes.md @"+secret)
	writeAll(t, filepath.Join(agentDir, "RULES.md"), "rules @~/notes.md")
	root, _ := gitRepo(t)
	inputs, err := Discover(context.Background(), Options{Cwd: root, AgentDir: agentDir, HomeDir: home})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs.ContextFiles) != 1 || inputs.ContextFiles[0].Content != "user NOTES @"+secret {
		t.Fatalf("user context = %#v", inputs.ContextFiles)
	}
	if len(inputs.GenericRules) != 1 || inputs.GenericRules[0] != "rules NOTES" {
		t.Fatalf("user rules = %q", inputs.GenericRules)
	}
}

// Agent files come from the repository root, also when the session starts in
// a subdirectory; one linked from outside the repository, or one that is not
// a regular file, is skipped with a diagnostic instead of read.
func TestDiscoverAgentFilesFromRepoRootWithinRepo(t *testing.T) {
	root, sub := gitRepo(t)
	writeAll(t, filepath.Join(root, ".claude", "agents", "local.md"), "---\nname: local\ndescription: Lives in the repo\n---\nWork.\n")
	symlink(t, outsideFile(t, "external.md", "---\nname: external\ndescription: "+outsideMarker+"\n---\nWork.\n"), filepath.Join(root, ".claude", "agents", "external.md"))
	fifo(t, filepath.Join(root, ".harness", "agents", "stall.md"))
	var inputs SessionInputs
	returnsInTime(t, "agent discovery with a FIFO", func() {
		var err error
		inputs, err = Discover(context.Background(), Options{Cwd: sub, AgentDir: t.TempDir(), HomeDir: t.TempDir()})
		if err != nil {
			t.Error(err)
		}
	})
	var names []string
	for _, agent := range inputs.Agents {
		names = append(names, agent.Preset.ID)
	}
	if len(names) != 1 || names[0] != "local" {
		t.Fatalf("agents %v, want only local", names)
	}
	skipped := map[string]bool{}
	for _, diagnostic := range inputs.Diagnostics {
		if diagnostic.Code == DiagnosticAgent {
			skipped[filepath.Base(diagnostic.Path)] = true
		}
	}
	if !skipped["external.md"] || !skipped["stall.md"] {
		t.Fatalf("diagnostics %+v, want external.md and stall.md named", inputs.Diagnostics)
	}
}
