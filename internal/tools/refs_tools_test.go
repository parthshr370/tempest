package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/refs"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/skills"
)

// refsFixture is a skill "alpha" plus a journaled root session, with
// resolvers for the root and for a child of that root.
type refsFixture struct {
	base        string
	rootJournal string
	root        *refs.Resolver
	child       *refs.Resolver
}

func newRefsFixture(t *testing.T) refsFixture {
	t.Helper()
	base := filepath.Join(t.TempDir(), "alpha")
	writeFile(t, filepath.Join(base, "SKILL.md"), "---\ndescription: alpha\n---\nbody\n")
	writeFile(t, filepath.Join(base, "reference.md"), "one\ntwo\nthree\n")
	items := []skills.Skill{{Name: "alpha", Description: "alpha", FilePath: filepath.Join(base, "SKILL.md"), BaseDir: base}}
	store, err := journal.Create(context.Background(), t.TempDir(), t.TempDir(), journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	newResolver := func() *refs.Resolver {
		resolver, err := refs.New(refs.Config{Skills: items, RootJournal: store.Path()})
		if err != nil {
			t.Fatal(err)
		}
		return resolver
	}
	return refsFixture{base: base, rootJournal: store.Path(), root: newResolver(), child: newResolver()}
}

func TestReadToolResolvesInternalURLs(t *testing.T) {
	fixture := newRefsFixture(t)
	read, _ := NewTool(ReadTool, t.TempDir(), ToolsOptions{Refs: fixture.root})

	result, err := executeTool(t, read, map[string]any{"path": "skill://alpha/reference.md:2-3"})
	text := resultText(result)
	if err != nil || !strings.HasPrefix(text, "[skill://alpha/reference.md#") || !strings.Contains(text, "two") || strings.Contains(text, "one") || strings.Contains(text, fixture.base) {
		t.Fatalf("skill read = %q, %v", text, err)
	}
	result, err = executeTool(t, read, map[string]any{"path": "skill://alpha/"})
	if text := resultText(result); err != nil || !strings.HasPrefix(text, "[skill://alpha/#") || !strings.Contains(text, "reference.md") {
		t.Fatalf("skill directory read = %q, %v", text, err)
	}
	if _, err := executeTool(t, read, map[string]any{"path": "skill://alpha/../../etc/passwd"}); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("escaping skill read error = %v", err)
	}
	unconfigured, _ := NewTool(ReadTool, t.TempDir(), ToolsOptions{})
	if _, err := executeTool(t, unconfigured, map[string]any{"path": "local://notes.md"}); err == nil || !strings.Contains(err.Error(), "journaled session") {
		t.Fatalf("local read without a journal error = %v", err)
	}
}

// TestLocalURLHandsPayloadsToChildren is the hand-off path: the root writes
// and edits local://, a child of the same root reads and greps it, and no
// write reaches the workspace or the language-server change feed.
func TestLocalURLHandsPayloadsToChildren(t *testing.T) {
	fixture := newRefsFixture(t)
	workspace := t.TempDir()
	var changed []string
	rootOptions := ToolsOptions{Refs: fixture.root, OnFileChanged: func(path string) { changed = append(changed, path) }}
	write, _ := NewTool(WriteTool, workspace, rootOptions)
	edit, _ := NewTool(EditTool, workspace, rootOptions)

	if _, err := executeTool(t, write, map[string]any{"path": "local://handoff/notes.md", "content": "plan: alpha\nstatus: draft\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := executeTool(t, edit, map[string]any{"path": "local://handoff/notes.md", "edits": []map[string]any{{"old_text": "draft", "new_text": "ready"}}}); err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatalf("local:// writes notified the language server: %v", changed)
	}
	if entries, _ := os.ReadDir(workspace); len(entries) != 0 {
		t.Fatalf("local:// write touched the workspace: %v", entries)
	}

	childCwd := t.TempDir()
	childRead, _ := NewTool(ReadTool, childCwd, ToolsOptions{Refs: fixture.child, Child: true})
	result, err := executeTool(t, childRead, map[string]any{"path": "local://handoff/notes.md"})
	if text := resultText(result); err != nil || !strings.HasPrefix(text, "[local://handoff/notes.md#") || !strings.Contains(text, "status: ready") {
		t.Fatalf("child read of local:// = %q, %v", text, err)
	}
	childGrep, _ := NewTool(GrepTool, childCwd, ToolsOptions{Refs: fixture.child, Child: true})
	result, err = executeTool(t, childGrep, map[string]any{"pattern": "status", "path": "local://"})
	if text := resultText(result); err != nil || !strings.Contains(text, "local://handoff/notes.md") || !strings.Contains(text, "status: ready") {
		t.Fatalf("child grep of local:// = %q, %v", text, err)
	}

	for path, want := range map[string]string{
		"skill://alpha/reference.md": "only local:// URLs are writable",
		"job://":                     "only local:// URLs are writable",
		"local://":                   "name a file",
		"local://../escape.md":       "escapes",
	} {
		if _, err := executeTool(t, write, map[string]any{"path": path, "content": "x"}); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("write %s error = %v, want %q", path, err, want)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(fixture.rootJournal+".d", refs.LocalDirName, "out")); err != nil {
		t.Fatal(err)
	}
	if _, err := executeTool(t, write, map[string]any{"path": "local://out/x.md", "content": "x"}); err == nil {
		t.Fatal("write through a symlink leaving local:// succeeded")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("write escaped local:// into %v", entries)
	}
}

func TestGrepToolSearchesInternalURLs(t *testing.T) {
	fixture := newRefsFixture(t)
	grep, _ := NewTool(GrepTool, t.TempDir(), ToolsOptions{Refs: fixture.root})
	result, err := executeTool(t, grep, map[string]any{"pattern": "^two$", "path": "skill://alpha"})
	if text := resultText(result); err != nil || !strings.Contains(text, "skill://alpha/reference.md") || !strings.Contains(text, "two") || strings.Contains(text, fixture.base) {
		t.Fatalf("skill grep = %q, %v", text, err)
	}
	// Rendered resources are searched as text.
	result, err = executeTool(t, grep, map[string]any{"pattern": "no jobs", "path": "job://", "ignore_case": true})
	if text := resultText(result); err != nil || !strings.Contains(text, "job://") || !strings.Contains(text, "No jobs") {
		t.Fatalf("job listing grep = %q, %v", text, err)
	}
	if _, err := executeTool(t, grep, map[string]any{"pattern": "x", "path": "local://../"}); err == nil {
		t.Fatal("grep escaped local://")
	}
}

func TestBashExpandsURLsBeforeThePermissionGate(t *testing.T) {
	fixture := newRefsFixture(t)
	bash, _ := NewTool(BashTool, t.TempDir(), ToolsOptions{Refs: fixture.root})

	prepared := bash.PrepareArguments(mustJSON(t, map[string]any{"command": "cat skill://alpha/reference.md", "timeout": 5}))
	var args struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal(prepared, &args); err != nil {
		t.Fatal(err)
	}
	if args.Command != "cat '"+filepath.Join(fixture.base, "reference.md")+"'" || args.Timeout != 5 {
		t.Fatalf("prepared arguments = %s", prepared)
	}
	result, err := executeTool(t, bash, map[string]any{"command": "cat skill://alpha/reference.md"})
	if err != nil || resultText(result) != "one\ntwo\nthree\n" {
		t.Fatalf("bash on skill:// = %q, %v", resultText(result), err)
	}
	result, err = executeTool(t, bash, map[string]any{"command": "echo handoff > local://from-bash.txt && cat local://from-bash.txt"})
	if err != nil || resultText(result) != "handoff\n" {
		t.Fatalf("bash on local:// = %q, %v", resultText(result), err)
	}
	if _, err := executeTool(t, bash, map[string]any{"command": "cat skill://missing/x"}); err == nil || !strings.Contains(err.Error(), "cannot expand skill://missing/x") {
		t.Fatalf("unresolvable URL error = %v", err)
	}
	// Execute never runs a command whose URLs the gate did not see expanded.
	if _, err := bash.Execute(context.Background(), "id", mustJSON(t, map[string]any{"command": "cat skill://alpha/reference.md"}), nil); err == nil || !strings.Contains(err.Error(), "permission check") {
		t.Fatalf("unprepared command error = %v", err)
	}
}
