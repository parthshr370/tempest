package lsp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestGoplsIntegration drives an installed gopls over a tiny offline module.
// Opt in with HARNESS_TEST_GOPLS=1; it never installs anything or uses the network.
func TestGoplsIntegration(t *testing.T) {
	if os.Getenv("HARNESS_TEST_GOPLS") != "1" {
		t.Skip("set HARNESS_TEST_GOPLS=1 to run against an installed gopls")
	}
	gopls, err := exec.LookPath("gopls")
	if err != nil {
		t.Skip("prerequisite missing: gopls is not on PATH")
	}
	if version, err := exec.Command(gopls, "version").CombinedOutput(); err == nil {
		t.Logf("gopls version: %s", strings.TrimSpace(string(version)))
	}
	ws := newDirWorkspace(t)
	ws.write(t, "go.mod", "module example.com/m\n\ngo 1.22\n")
	ws.write(t, "a.go", "package m\n\n// Hello greets.\nfunc Hello() string { return \"hi\" }\n")
	ws.write(t, "b.go", "package m\n\nfunc Use() string { return Hello() + Hello() }\n")
	env := map[string]string{"GOTOOLCHAIN": "local", "GOPROXY": "off", "GOFLAGS": "-mod=mod"}
	m, err := New(ws, Config{Enabled: true, Servers: []ServerConfig{{Name: "go", Command: "gopls", Env: env}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	query := func(q Query) map[string]any {
		t.Helper()
		result, err := m.Query(context.Background(), q)
		if err != nil {
			t.Fatalf("%s: %v", q.Method, err)
		}
		var value map[string]any
		_ = json.Unmarshal(result.Value, &value)
		return value
	}
	definition := query(Query{Method: MethodDefinition, Path: "b.go", Position: Position{3, 28}})
	if got := locationList(t, definition); !reflect.DeepEqual(got, []string{"a.go:4:6-4:11"}) {
		t.Fatalf("definition %v", got)
	}
	references := query(Query{Method: MethodReferences, Path: "a.go", Position: Position{4, 6}, IncludeDeclaration: true})
	if got := locationList(t, references); !reflect.DeepEqual(got, []string{"a.go:4:6-4:11", "b.go:3:28-3:33", "b.go:3:38-3:43"}) {
		t.Fatalf("references %v", got)
	}
	before := ws.hashes(t)
	plan := renamePlanOf(t, query(Query{Method: MethodRename, Path: "a.go", Position: Position{4, 6}, NewName: "Greet"}))
	if !reflect.DeepEqual(before, ws.hashes(t)) || plan.FileCount != 2 {
		t.Fatalf("rename plan %+v or workspace changed", plan)
	}
	for _, file := range plan.Files {
		applyLikeEditTool(t, ws, file)
		m.Changed(file.Path)
	}
	if data, _ := os.ReadFile(filepath.Join(ws.root, "b.go")); string(data) != "package m\n\nfunc Use() string { return Greet() + Greet() }\n" {
		t.Fatalf("b.go after edits: %q", data)
	}
	ws.write(t, "c.go", "package m\n\nvar broken int = \"text\"\n")
	m.Changed("c.go")
	diagnostics := query(Query{Method: MethodDiagnostics, Path: "c.go", Wait: 10 * time.Second, Severity: "error"})
	t.Logf("diagnostics before fix: %s", diagnosticSummary(diagnostics))
	if diagnostics["total"].(float64) < 1 || diagnostics["fresh"] != true {
		t.Fatalf("type error not reported as fresh: %s", diagnosticSummary(diagnostics))
	}
	ws.write(t, "c.go", "package m\n\nvar fixed int = 1\n")
	m.Changed("c.go")
	diagnostics = query(Query{Method: MethodDiagnostics, Path: "c.go", Wait: 10 * time.Second, Severity: "error"})
	t.Logf("diagnostics after fix: %s", diagnosticSummary(diagnostics))
	if diagnostics["total"].(float64) != 0 || diagnostics["fresh"] != true {
		t.Fatalf("fixed file still reports errors: %s", diagnosticSummary(diagnostics))
	}
}
