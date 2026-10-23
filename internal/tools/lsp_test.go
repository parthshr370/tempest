package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/editdiff"
	"go.harness.dev/harness/internal/lsp"
	"go.harness.dev/harness/internal/toolio"
)

// newInertLSPManager returns a manager over cwd whose servers are never
// started by these tests; Close is registered for cleanup.
func newInertLSPManager(t *testing.T, cwd string) *lsp.Manager {
	t.Helper()
	return newLSPManager(t, cwd, lsp.Config{Enabled: true})
}

func newLSPManager(t *testing.T, cwd string, cfg lsp.Config) *lsp.Manager {
	t.Helper()
	workspace, err := NewLSPWorkspace(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := lsp.New(workspace, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(errors.Join(err, workspace.Close()))
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	return manager
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func openWorkspace(t *testing.T, cwd string, queue *toolio.FileMutationQueue) lsp.Workspace {
	t.Helper()
	workspace, err := NewLSPWorkspace(cwd, queue)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := workspace.Close(); err != nil {
			t.Errorf("close workspace: %v", err)
		}
	})
	return workspace
}

func TestLSPWorkspaceReadsContainedCanonicalFiles(t *testing.T) {
	// t.TempDir sits behind a symlinked prefix on macOS (/var -> /private/var),
	// so both spellings of an absolute path are exercised there.
	cwd := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(cwd, "pkg", "a.go"), "package pkg\n")
	writeTestFile(t, filepath.Join(outside, "secret.go"), "package secret\n")
	if err := os.Symlink(filepath.Join(cwd, "pkg", "a.go"), filepath.Join(cwd, "alias.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.go"), filepath.Join(cwd, "escape.go")); err != nil {
		t.Fatal(err)
	}
	workspace := openWorkspace(t, cwd, nil)
	canonicalRoot, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Root() != canonicalRoot {
		t.Fatalf("Root = %q, want canonical %q", workspace.Root(), canonicalRoot)
	}
	for _, name := range []string{"pkg/a.go", "./pkg/a.go", filepath.Join(cwd, "pkg", "a.go"), filepath.Join(canonicalRoot, "pkg", "a.go"), "alias.go"} {
		document, err := workspace.ReadFile(context.Background(), name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if document.Path != "pkg/a.go" || string(document.Text) != "package pkg\n" || document.Anchor != editdiff.ContentAnchor(document.Text) {
			t.Fatalf("%s read as %+v", name, document)
		}
	}
	for name, want := range map[string]error{
		"escape.go":                           ErrOutsideRoot,
		"../" + filepath.Base(outside) + "/x": ErrOutsideRoot,
		filepath.Join(outside, "secret.go"):   ErrOutsideRoot,
		"missing.go":                          fs.ErrNotExist,
	} {
		if _, err := workspace.ReadFile(context.Background(), name); !errors.Is(err, want) {
			t.Fatalf("%s: got %v, want %v", name, err, want)
		}
	}
	for _, name := range []string{"pkg", "", "a\x00b"} {
		if _, err := workspace.ReadFile(context.Background(), name); err == nil {
			t.Fatalf("%q read as a document", name)
		}
	}
}

// TestLSPWorkspaceReadWaitsForMutationLock checks that a language-server
// read cannot observe a file halfway through an edit holding its lock.
func TestLSPWorkspaceReadWaitsForMutationLock(t *testing.T) {
	cwd := t.TempDir()
	path := filepath.Join(cwd, "a.go")
	writeTestFile(t, path, "package a\n")
	queue := toolio.NewFileMutationQueue()
	workspace := openWorkspace(t, cwd, queue)
	locked, release := make(chan struct{}), make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- queue.Do(path, func() error {
			close(locked)
			<-release
			return os.WriteFile(path, []byte("package b\n"), 0o644)
		})
	}()
	<-locked
	read := make(chan lsp.Document, 1)
	go func() {
		document, err := workspace.ReadFile(context.Background(), "a.go")
		if err != nil {
			t.Error(err)
		}
		read <- document
	}()
	select {
	case document := <-read:
		t.Fatalf("read %q while the mutation lock was held", document.Text)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if document := <-read; string(document.Text) != "package b\n" {
		t.Fatalf("read %q, want the completed write", document.Text)
	}
}

func TestLSPWorkspaceSnapshotIsBoundedAndSkipsGitAndSymlinks(t *testing.T) {
	cwd := t.TempDir()
	writeTestFile(t, filepath.Join(cwd, "main.go"), "package main\n")
	writeTestFile(t, filepath.Join(cwd, "deploy", "app.YAML"), "kind: x\n")
	writeTestFile(t, filepath.Join(cwd, "infra", "main.tf"), "")
	writeTestFile(t, filepath.Join(cwd, "README.md"), "# skip\n")
	writeTestFile(t, filepath.Join(cwd, ".git", "hooks", "x.go"), "package hooks\n")
	writeTestFile(t, filepath.Join(t.TempDir(), "unused"), "")
	if err := os.Symlink(filepath.Join(cwd, "infra"), filepath.Join(cwd, "linked")); err != nil {
		t.Fatal(err)
	}
	workspace := openWorkspace(t, cwd, nil)
	documents, err := workspace.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, document := range documents {
		paths = append(paths, document.Path)
	}
	if got := strings.Join(paths, ","); got != "deploy/app.YAML,infra/main.tf,main.go" {
		t.Fatalf("snapshot = %s", got)
	}

	for i := range maxSnapshotFiles + 1 {
		writeTestFile(t, filepath.Join(cwd, "many", fmt.Sprintf("f%03d.go", i)), "package many\n")
	}
	_, err = workspace.Snapshot(context.Background())
	var lspErr *lsp.Error
	if !errors.As(err, &lspErr) || lspErr.Code != lsp.CodeRenameScopeLimit {
		t.Fatalf("got %v, want %s", err, lsp.CodeRenameScopeLimit)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := workspace.Snapshot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled snapshot: %v", err)
	}
}

// changeRecorder records OnFileChanged calls and can act inside the hook.
type changeRecorder struct {
	mu     sync.Mutex
	paths  []string
	inHook func()
}

func (r *changeRecorder) record(path string) {
	r.mu.Lock()
	r.paths = append(r.paths, path)
	r.mu.Unlock()
	if r.inHook != nil {
		r.inHook()
	}
}

func (r *changeRecorder) recorded() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.paths, ",")
}

func TestWriteAndEditReportCanonicalChanges(t *testing.T) {
	cwd := t.TempDir()
	writeTestFile(t, filepath.Join(cwd, "src", "a.go"), "package a\n\nconst Name = \"a\"\n")
	if err := os.Symlink("src", filepath.Join(cwd, "link")); err != nil {
		t.Fatal(err)
	}
	recorder := &changeRecorder{}
	options := ToolsOptions{OnFileChanged: recorder.record}
	write, err := NewTool(WriteTool, cwd, options)
	if err != nil {
		t.Fatal(err)
	}
	edit, err := NewTool(EditTool, cwd, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executeTool(t, write, map[string]any{"path": "new/b.go", "content": "package b\n"}); err != nil {
		t.Fatal(err)
	}
	// Edits through a symlinked directory report the file actually written.
	if _, err := executeTool(t, edit, map[string]any{"path": "link/a.go", "edits": []map[string]any{{"old_text": `"a"`, "new_text": `"b"`}}}); err != nil {
		t.Fatal(err)
	}
	if got := recorder.recorded(); got != "new/b.go,src/a.go" {
		t.Fatalf("changes = %s, want canonical paths", got)
	}

	// Rejected edits change nothing on disk and report nothing.
	stale := map[string]any{"path": "src/a.go", "edits": []map[string]any{{"old_text": `"b"`, "new_text": `"c"`, "anchor": strings.Repeat("0", 64)}}}
	missing := map[string]any{"path": "src/a.go", "edits": []map[string]any{{"old_text": "absent", "new_text": "x"}}}
	for _, args := range []map[string]any{stale, missing} {
		if _, err := executeTool(t, edit, args); err == nil {
			t.Fatalf("edit %v succeeded", args)
		}
	}
	if got := recorder.recorded(); got != "new/b.go,src/a.go" {
		t.Fatalf("rejected edits reported changes: %s", got)
	}
}

// TestWriteReportsChangeEvenWhenCancelledAfterDiskWrite cancels the call from
// inside the hook, after the bytes are on disk: the tool fails, but the
// change was still reported because the file did change.
func TestWriteReportsChangeEvenWhenCancelledAfterDiskWrite(t *testing.T) {
	for _, name := range []ToolName{WriteTool, EditTool} {
		t.Run(string(name), func(t *testing.T) {
			cwd := t.TempDir()
			writeTestFile(t, filepath.Join(cwd, "a.go"), "package a\n")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			recorder := &changeRecorder{inHook: cancel}
			tool, err := NewTool(name, cwd, ToolsOptions{OnFileChanged: recorder.record})
			if err != nil {
				t.Fatal(err)
			}
			args := `{"path":"a.go","content":"package b\n"}`
			if name == EditTool {
				args = `{"path":"a.go","edits":[{"old_text":"package a","new_text":"package b"}]}`
			}
			if _, err := tool.Execute(ctx, "call", json.RawMessage(args), nil); err == nil {
				t.Fatal("cancelled call reported success")
			}
			content, err := os.ReadFile(filepath.Join(cwd, "a.go"))
			if err != nil {
				t.Fatal(err)
			}
			if string(content) != "package b\n" || recorder.recorded() != "a.go" {
				t.Fatalf("disk %q, changes %q", content, recorder.recorded())
			}
		})
	}
}

func TestLSPToolArgumentsAreStrict(t *testing.T) {
	cwd := t.TempDir()
	tools := AllTools(cwd, ToolsOptions{LSP: newInertLSPManager(t, cwd)})
	cases := map[ToolName][]string{
		LSPDefinitionTool: {
			`{"path":"a.go","line":1}`,
			`{"path":"a.go","line":0,"column":1}`,
			`{"path":"a.go","line":1.5,"column":1}`,
			`{"path":"a.go","line":"1","column":1}`,
			`{"path":"a.go","line":1,"column":1,"max_results":201}`,
			`{"path":"a.go","line":1,"column":1,"max_results":0}`,
			`{"path":"a.go","line":1,"column":1,"server":"python"}`,
			`{"path":"a.go","line":1,"column":1,"maxResults":5}`,
			`{"path":"","line":1,"column":1}`,
			`{"path":"file:///etc/passwd","line":1,"column":1}`,
			`{"path":"a\u0000.go","line":1,"column":1}`,
			`{"path":"a.go","line":1,"column":1} []`,
		},
		LSPReferencesTool: {
			`{"path":"a.go","line":1,"column":1,"include_declaration":"yes"}`,
			`{"path":"a.go","line":1,"column":1,"symbol":"Name"}`,
		},
		LSPRenameTool: {
			`{"path":"a.go","line":1,"column":1}`,
			`{"path":"a.go","line":1,"column":1,"new_name":""}`,
			`{"path":"a.go","line":1,"column":1,"new_name":"` + strings.Repeat("é", maxLSPNewNameRunes+1) + `"}`,
			`{"path":"a.go","line":1,"column":1,"new_name":"x","apply":true}`,
		},
		LSPDiagnosticsTool: {
			`{}`,
			`{"path":"a.go","severity":"fatal"}`,
			`{"path":"a.go","wait_ms":10001}`,
			`{"path":"a.go","wait_ms":-1}`,
			`{"path":"a.go","line":3}`,
		},
	}
	for name, invalid := range cases {
		for _, args := range invalid {
			_, err := tools[name].Execute(context.Background(), "call", json.RawMessage(args), nil)
			var lspErr *lsp.Error
			if err == nil || errors.As(err, &lspErr) {
				t.Fatalf("%s accepted or forwarded %s: %v", name, args, err)
			}
			if !strings.HasPrefix(err.Error(), string(name)+": ") {
				t.Fatalf("%s error does not name the tool: %v", name, err)
			}
		}
	}
	// A 256-character multi-byte name is within the limit: it is counted in
	// characters, not bytes, and reaches the manager.
	writeTestFile(t, filepath.Join(cwd, "notes.txt"), "text\n")
	valid := map[ToolName]string{
		LSPDefinitionTool:  `{"path":"notes.txt","line":1,"column":1}`,
		LSPReferencesTool:  `{"path":"notes.txt","line":1,"column":1,"include_declaration":true,"max_results":200}`,
		LSPRenameTool:      `{"path":"notes.txt","line":1,"column":1,"new_name":"` + strings.Repeat("é", maxLSPNewNameRunes) + `"}`,
		LSPDiagnosticsTool: `{"path":"notes.txt","severity":"hint","wait_ms":0,"max_results":1}`,
	}
	for name, args := range valid {
		_, err := tools[name].Execute(context.Background(), "call", json.RawMessage(args), nil)
		var lspErr *lsp.Error
		if !errors.As(err, &lspErr) || lspErr.Code != lsp.CodeNoServer {
			t.Fatalf("%s %s: got %v, want the manager's %s", name, args, err, lsp.CodeNoServer)
		}
	}
}

func TestLSPToolsRegisterOnlyWithManagerAndCannotBeShadowed(t *testing.T) {
	cwd := t.TempDir()
	impostor := agent.AgentTool{Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		return textResult("impostor", nil), nil
	}}
	impostor.Name = string(LSPDefinitionTool)
	disabled := AllTools(cwd, ToolsOptions{ConfiguredTools: []agent.AgentTool{impostor}})
	if _, ok := disabled[LSPDefinitionTool]; ok {
		t.Fatal("a configured tool claimed a reserved lsp_* name")
	}
	enabled := AllTools(cwd, ToolsOptions{LSP: newInertLSPManager(t, cwd), ConfiguredTools: []agent.AgentTool{impostor}})
	for _, name := range LSPToolNames {
		if enabled[name].Execute == nil {
			t.Fatalf("%s not registered with a manager", name)
		}
	}
	result, err := enabled[LSPDefinitionTool].Execute(context.Background(), "call", json.RawMessage(`{"path":"x.txt","line":1,"column":1}`), nil)
	if resultText(result) == "impostor" || err == nil {
		t.Fatalf("built-in was shadowed: %q %v", resultText(result), err)
	}
}
