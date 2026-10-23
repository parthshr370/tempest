package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"go.harness.dev/harness/internal/lsp"
)

// fixtureServer is the language-server fixture from internal/lsp, built
// once for this package's tests.
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
	build := exec.Command("go", "build", "-o", fixtureServer, "../lsp/testdata/server")
	if output, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build LSP fixture: %v\n%s", err, output)
		return 1
	}
	return m.Run()
}

// ledgerEvent is one line of the fixture's ledger.
type ledgerEvent struct {
	Event   string `json:"event"`
	Method  string `json:"method"`
	Version int    `json:"version"`
	Params  struct {
		TextDocument struct {
			Version int `json:"version"`
		} `json:"textDocument"`
	} `json:"params"`
}

func readLedger(t *testing.T, path string) []ledgerEvent {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var events []ledgerEvent
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<24)
	for scanner.Scan() {
		var event ledgerEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("ledger line %q: %v", scanner.Text(), err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

// TestLSPToolsDriveARealServerAndSeeEdits wires the tools exactly as a
// session does: one manager, its Changed hook on edit, a real child process.
// An edit made through the edit tool must reach the server as didChange
// before the next query, and results come back as the manager's JSON.
func TestLSPToolsDriveARealServerAndSeeEdits(t *testing.T) {
	cwd := t.TempDir()
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	writeTestFile(t, filepath.Join(cwd, "a.go"), "package a\n\nfunc Hello() {}\n\nvar _ = Hello\n")
	manager := newLSPManager(t, cwd, lspFixtureConfig(ledger))
	registry := AllTools(cwd, ToolsOptions{LSP: manager, OnFileChanged: manager.Changed})

	result, err := registry[LSPDefinitionTool].Execute(context.Background(), "call", json.RawMessage(`{"path":"a.go","line":5,"column":10}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Details) != resultText(result) {
		t.Fatalf("details must equal the text JSON")
	}
	var definition struct {
		Server    string `json:"server"`
		Path      string `json:"path"`
		Locations []struct {
			Path   string `json:"path"`
			Line   int    `json:"line"`
			Column int    `json:"column"`
		} `json:"locations"`
	}
	if err := json.Unmarshal(result.Details, &definition); err != nil {
		t.Fatal(err)
	}
	if definition.Server != "go" || len(definition.Locations) != 1 || definition.Locations[0].Path != "a.go" || definition.Locations[0].Line != 3 || definition.Locations[0].Column != 6 {
		t.Fatalf("definition = %s", result.Details)
	}

	if _, err := executeTool(t, registry[EditTool], map[string]any{"path": "a.go", "edits": []map[string]any{{"old_text": "var _ = Hello", "new_text": "var _ = Hello // ERR"}}}); err != nil {
		t.Fatal(err)
	}
	result, err = registry[LSPDiagnosticsTool].Execute(context.Background(), "call", json.RawMessage(`{"path":"a.go","wait_ms":5000}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics struct {
		Version     int    `json:"version"`
		Fresh       bool   `json:"fresh"`
		Freshness   string `json:"freshness"`
		Diagnostics []struct {
			Line     int    `json:"line"`
			Severity string `json:"severity"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(result.Details, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if !diagnostics.Fresh || diagnostics.Freshness != "current" || diagnostics.Version != 2 || len(diagnostics.Diagnostics) != 1 || diagnostics.Diagnostics[0].Line != 5 || diagnostics.Diagnostics[0].Severity != "error" {
		t.Fatalf("diagnostics after edit = %s", result.Details)
	}
	sawChange := false
	for _, event := range readLedger(t, ledger) {
		if event.Method == "textDocument/didChange" && event.Params.TextDocument.Version == 2 {
			sawChange = true
		}
		if event.Method == "textDocument/diagnostic" && !sawChange {
			t.Fatal("a query reached the server before the edit's didChange")
		}
	}
	if !sawChange {
		t.Fatal("the edit never reached the server as didChange version 2")
	}

	// The rename tool only plans: the file is untouched afterwards.
	before, err := os.ReadFile(filepath.Join(cwd, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	result, err = registry[LSPRenameTool].Execute(context.Background(), "call", json.RawMessage(`{"path":"a.go","line":3,"column":6,"new_name":"Greet"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Applied bool `json:"applied"`
		Files   []struct {
			Path   string            `json:"path"`
			Anchor string            `json:"anchor"`
			Edits  []json.RawMessage `json:"edits"`
		} `json:"files"`
	}
	if err := json.Unmarshal(result.Details, &plan); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(cwd, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Applied || len(plan.Files) != 1 || string(before) != string(after) {
		t.Fatalf("rename must only plan: %s; file changed: %v", result.Details, string(before) != string(after))
	}
	// Applying the plan through the ordinary edit tool yields the rename.
	edits, err := json.Marshal(plan.Files[0].Edits)
	if err != nil {
		t.Fatal(err)
	}
	apply := fmt.Sprintf(`{"path":%q,"edits":%s}`, plan.Files[0].Path, edits)
	if _, err := registry[EditTool].Execute(context.Background(), "call", json.RawMessage(apply), nil); err != nil {
		t.Fatalf("plan edits do not apply: %v", err)
	}
	renamed, err := os.ReadFile(filepath.Join(cwd, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(renamed) != "package a\n\nfunc Greet() {}\n\nvar _ = Greet // ERR\n" {
		t.Fatalf("renamed file = %q", renamed)
	}
}

func lspFixtureConfig(ledger string) lsp.Config {
	return lsp.Config{Enabled: true, Servers: []lsp.ServerConfig{{Name: "go", Command: fixtureServer, Args: []string{"-ledger", ledger}}}}
}
