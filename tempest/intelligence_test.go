package tempest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/lsp"
	"go.harness.dev/harness/internal/session"
	"go.harness.dev/harness/internal/tools"
)

// fixtureServer is internal/lsp's language-server fixture, built once.
var fixtureServer string

func TestMain(m *testing.M) {
	os.Exit(runWithFixture(m))
}

func runWithFixture(m *testing.M) int {
	dir, err := os.MkdirTemp("", "tempest-lsp-fixture")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	fixtureServer = filepath.Join(dir, "lsp-fixture")
	if output, err := exec.Command("go", "build", "-o", fixtureServer, "../internal/lsp/testdata/server").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build LSP fixture: %v\n%s", err, output)
		return 1
	}
	return m.Run()
}

func ledgerEvents(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var events []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<24)
	for scanner.Scan() {
		var line struct {
			Event  string `json:"event"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		events = append(events, strings.TrimSuffix(line.Event+":"+line.Method, ":"))
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func writeScript(t *testing.T, steps string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(path, []byte(steps), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func toolResults(messages []Message) map[string]types.ToolResultMessage {
	results := map[string]types.ToolResultMessage{}
	for _, message := range messages {
		if result, ok := normalizeMessage(message).(types.ToolResultMessage); ok {
			results[result.ToolCallID] = result
		}
	}
	return results
}

// TestSessionLSPLifecycle opens a root session with language servers, checks
// that Open starts nothing, that a model's lsp_definition call reaches a real
// server through the session gate, and that Close stops and joins it.
func TestSessionLSPLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "a.go"), []byte("package a\n\nfunc Hello() {}\n\nvar _ = Hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "cm.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	script := writeScript(t, `[{"toolCalls":[{"id":"d1","name":"lsp_definition","arguments":{"path":"a.go","line":5,"column":10}},{"id":"m1","name":"manifest_diff","arguments":{"before":"cm.yaml","after":"cm.yaml"}}]},{"text":"done"}]`)
	stack := StackConfig{LSP: &LSPConfig{Enabled: false}}
	s, err := Open(t.Context(), Options{
		SessionDir: t.TempDir(), Cwd: cwd, New: &NewSession{PermissionMode: "default"}, FauxScript: script, Stack: &stack,
		// The explicit option replaces the stack's disabled host default.
		LSP: &LSPConfig{Enabled: true, Servers: []LSPServerConfig{{Name: "go", Command: fixtureServer, Args: []string{"-ledger", ledger}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = s.Close(context.Background())
		}
	}()
	names := session.ToolNames(s.core.agent.State().Tools)
	for _, name := range []string{"lsp_definition", "lsp_references", "lsp_rename", "lsp_diagnostics", "manifest_diff"} {
		if !slices.Contains(names, name) {
			t.Fatalf("root session lacks %s: %v", name, names)
		}
	}
	if events := ledgerEvents(t, ledger); len(events) != 0 {
		t.Fatalf("Open started a language server: %v", events)
	}
	if _, err := s.Prompt(t.Context(), Prompt{Text: "where is Hello defined?"}); err != nil {
		t.Fatal(err)
	}
	results := toolResults(s.Messages())
	definition := results["d1"]
	if definition.IsError || !strings.Contains(definition.Content[0].Text, `"line":3,"column":6`) {
		t.Fatalf("lsp_definition result = %#v", definition)
	}
	if diff := results["m1"]; diff.IsError || !strings.Contains(diff.Content[0].Text, `"summary"`) {
		t.Fatalf("manifest_diff result = %#v", diff)
	}
	manager := s.core.lsp
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	closed = true
	events := ledgerEvents(t, ledger)
	if !slices.Contains(events, "start") || !(slices.Contains(events, "exit") || slices.Contains(events, "eof")) {
		t.Fatalf("Close did not stop the server: %v", events)
	}
	_, err = manager.Query(context.Background(), lsp.Query{Method: lsp.MethodDefinition, Path: "a.go", Position: lsp.Position{Line: 1, Column: 1}})
	var lspErr *lsp.Error
	if !errors.As(err, &lspErr) || lspErr.Code != lsp.CodeClosed {
		t.Fatalf("manager still usable after Close: %v", err)
	}
}

func TestSessionWithoutLSPHasNoLanguageServerTools(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HARNESS_ENABLE_LSP", "true")
	script := writeScript(t, `[{"text":"ok"}]`)
	// The explicit disabled option beats the enabled environment default.
	s, err := Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{}, FauxScript: script, LSP: &LSPConfig{Enabled: false}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	names := session.ToolNames(s.core.agent.State().Tools)
	if slices.Contains(names, "lsp_definition") || s.core.lsp != nil || !slices.Contains(names, "manifest_diff") {
		t.Fatalf("tools = %v, manager = %v", names, s.core.lsp)
	}

	// Without an option, the environment default enables the tools.
	enabled, err := Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{}, FauxScript: script})
	if err != nil {
		t.Fatal(err)
	}
	defer enabled.Close(context.Background())
	if !slices.Contains(session.ToolNames(enabled.core.agent.State().Tools), "lsp_rename") || enabled.core.lsp == nil {
		t.Fatal("HARNESS_ENABLE_LSP host default was not applied")
	}
}

func TestOpenRejectsInvalidLanguageServerConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{}, FauxScript: writeScript(t, `[{"text":"ok"}]`),
		LSP: &LSPConfig{Enabled: true, Servers: []LSPServerConfig{{Name: "cobol", Command: "x"}}}})
	var lspErr *lsp.Error
	if !errors.As(err, &lspErr) || lspErr.Code != lsp.CodeConfig {
		t.Fatalf("got %v, want %s", err, lsp.CodeConfig)
	}
}

func TestOpenNormalizesAndDetachesCloudRules(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rules := []CloudRule{{Tool: "kubectl", Context: "prod", Action: "deny"}, {Tool: "helm", Context: "dev", Action: "allow_write"}, {Tool: "kubectl", Context: "prod", Action: "deny"}}
	s, err := Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{}, FauxScript: writeScript(t, `[{"text":"ok"}]`), CloudRules: rules})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	rules[0].Action = "allow_write"
	got := s.core.permissionPolicy.CloudRules
	want := []CloudRule{{Tool: "helm", Context: "dev", Action: "allow_write"}, {Tool: "kubectl", Context: "prod", Action: "deny"}}
	if !slices.Equal(got, want) {
		t.Fatalf("policy rules = %#v, want normalized detached %#v", got, want)
	}
	_, err = Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{}, FauxScript: writeScript(t, `[{"text":"ok"}]`), CloudRules: []CloudRule{{Tool: "kubectl", Context: "prod-*", Action: "deny"}}})
	if err == nil {
		t.Fatal("accepted a pattern context")
	}
}

func TestChildrenNeverInheritIntelligenceTools(t *testing.T) {
	parent := []string{"read", "grep", "find", "ls", "edit", "write", "bash", "todo_write", "manifest_diff"}
	for _, name := range tools.LSPToolNames {
		parent = append(parent, string(name))
	}
	for _, kind := range []string{"explore", "general"} {
		for _, name := range childToolNames(kind, parent) {
			if strings.HasPrefix(name, "lsp_") || name == "manifest_diff" {
				t.Fatalf("%s child selected %s", kind, name)
			}
		}
	}
}

func TestIntelligenceToolKinds(t *testing.T) {
	for _, test := range []struct {
		name, args, title string
		kind              ToolKind
	}{
		{"lsp_definition", `{"path":"a.go","line":3,"column":6}`, "lsp_definition: a.go:3:6", "search"},
		{"lsp_references", `{"path":"a.go","line":3,"column":6}`, "lsp_references: a.go:3:6", "search"},
		{"lsp_rename", `{"path":"a.go","line":3,"column":6,"new_name":"X"}`, "lsp_rename: a.go:3:6", "read"},
		{"lsp_diagnostics", `{"path":"a.go"}`, "lsp_diagnostics: a.go", "read"},
		{"manifest_diff", `{"before":"old","after":"new"}`, "manifest_diff: old -> new", "read"},
		{"edit", `{"path":"a.go"}`, "edit: a.go", "edit"},
	} {
		title, kind := toolDescription(test.name, json.RawMessage(test.args))
		if title != test.title || kind != test.kind {
			t.Errorf("%s: (%q, %q), want (%q, %q)", test.name, title, kind, test.title, test.kind)
		}
	}
}
