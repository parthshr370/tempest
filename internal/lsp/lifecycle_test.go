package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
)

const goSource = "package p\n\nfunc tag() {}\n\nvar x = tag\n"

func TestLSPLazyLifecycle(t *testing.T) {
	t.Run("new is inert and simultaneous queries initialize once", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"init_delay_ms": 100}})
		h.ws.write(t, "a.go", goSource)
		if _, err := os.Stat(h.ledgers["go"]); !errors.Is(err, os.ErrNotExist) || h.ws.readCount() != 0 {
			t.Fatal("New started a server or read the workspace")
		}
		var group sync.WaitGroup
		for range 4 {
			group.Go(func() {
				if _, err := h.query(definitionAt("a.go", 5, 9)); err != nil {
					t.Error(err)
				}
			})
		}
		group.Wait()
		if starts, initializes := h.count("go", "start"), h.count("go", "initialize"); starts != 1 || initializes != 1 {
			t.Fatalf("starts %d initializes %d", starts, initializes)
		}
	})
	t.Run("routes and sessions get separate processes", func(t *testing.T) {
		h := startHarness(t, setup{routes: []string{"go", "yaml"}})
		h.ws.write(t, "a.go", goSource)
		h.ws.write(t, "a.yaml", "tag: tag\n")
		h.mustQuery(definitionAt("a.go", 5, 9))
		h.mustQuery(definitionAt("a.yaml", 1, 6))
		other := startHarness(t, setup{ws: h.ws})
		other.mustQuery(definitionAt("a.go", 5, 9))
		pids := []int{h.pids("go")[0], h.pids("yaml")[0], other.pids("go")[0]}
		if pids[0] == pids[1] || pids[0] == pids[2] || pids[1] == pids[2] {
			t.Fatalf("shared processes: %v", pids)
		}
	})
	t.Run("idle retirement then a fresh process", func(t *testing.T) {
		h := startHarness(t, setup{timing: func(tm *timing) { tm.idle = 150 * time.Millisecond }, fixture: map[string]any{"push": "none"}})
		h.ws.write(t, "a.go", goSource)
		h.mustQuery(definitionAt("a.go", 5, 9))
		first := h.pids("go")[0]
		eventually(t, "idle shutdown", func() bool { return h.count("go", "exit") >= 1 })
		eventually(t, "process exit", func() bool { return processGone(first) })
		if got := h.received("go"); !slices.Contains(got, "shutdown") || got[len(got)-1] != "exit" {
			t.Fatalf("retirement order: %v", got)
		}
		h.mustQuery(definitionAt("a.go", 5, 9))
		if pids := h.pids("go"); len(pids) != 2 || pids[1] == first {
			t.Fatalf("pids %v", pids)
		}
		// The new generation reopens documents from version 1.
		opens := h.params("go", "textDocument/didOpen")
		if len(opens) != 2 || opens[1]["textDocument"].(map[string]any)["version"].(float64) != 1 {
			t.Fatalf("reopen: %v", opens)
		}
	})
	t.Run("close during startup joins the process", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"init_delay_ms": 60000}})
		h.ws.write(t, "a.go", goSource)
		done := make(chan error, 1)
		go func() { _, err := h.query(definitionAt("a.go", 5, 9)); done <- err }()
		eventually(t, "initialize", func() bool { return h.count("go", "initialize") == 1 })
		pid := h.pids("go")[0]
		closeWithin(t, h.m, 5*time.Second)
		wantCode(t, <-done, CodeClosed)
		if !processGone(pid) || !h.ws.closed {
			t.Fatal("close left the process or workspace open")
		}
	})
	t.Run("close during a query joins the process", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"hang": []string{"textDocument/definition"}}})
		h.ws.write(t, "a.go", goSource)
		done := make(chan error, 1)
		go func() { _, err := h.query(definitionAt("a.go", 5, 9)); done <- err }()
		eventually(t, "definition", func() bool { return h.count("go", "textDocument/definition") == 1 })
		pid := h.pids("go")[0]
		closeWithin(t, h.m, 5*time.Second)
		wantCode(t, <-done, CodeClosed)
		if !processGone(pid) {
			t.Fatal("process survived Close")
		}
		if _, err := h.m.Query(context.Background(), definitionAt("a.go", 5, 9)); errorCode(err) != CodeClosed {
			t.Fatalf("query after close: %v", err)
		}
		h.m.Changed("a.go")
		closeWithin(t, h.m, time.Second)
	})
	t.Run("close while idle shuts down gracefully", func(t *testing.T) {
		h := startHarness(t, setup{})
		h.ws.write(t, "a.go", goSource)
		h.mustQuery(definitionAt("a.go", 5, 9))
		pid := h.pids("go")[0]
		closeWithin(t, h.m, 5*time.Second)
		got := h.received("go")
		if got[len(got)-2] != "shutdown" || got[len(got)-1] != "exit" || !processGone(pid) {
			t.Fatalf("shutdown sequence: %v", got)
		}
	})
	t.Run("run cancellation keeps the healthy server", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"hang": []string{"textDocument/definition"}}})
		h.ws.write(t, "a.go", goSource)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := h.queryCtx(ctx, definitionAt("a.go", 5, 9)); done <- err }()
		eventually(t, "definition", func() bool { return h.count("go", "textDocument/definition") == 1 })
		cancel()
		err := <-done
		wantCode(t, err, CodeCancelled)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation must unwrap: %v", err)
		}
		h.mustQuery(Query{Method: MethodReferences, Path: "a.go", Position: Position{5, 9}})
		if starts := h.count("go", "start"); starts != 1 {
			t.Fatalf("cancellation restarted the server: %d starts", starts)
		}
	})
}

func closeWithin(t *testing.T, m *Manager, limit time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestLSPInitializeCapabilities(t *testing.T) {
	t.Run("exact capabilities, root and ordering", func(t *testing.T) {
		h := startHarness(t, setup{
			fixture:  map[string]any{"config_sections": []string{"gopls", "gopls.ui", "missing", ""}, "probe_client": true},
			settings: `{"gopls":{"ui":{"codelens":false}}}`,
			options:  `{"opt":true}`,
		})
		h.ws.write(t, "a.go", goSource)
		h.mustQuery(definitionAt("a.go", 5, 9))
		initialize := h.params("go", "initialize")[0]
		var want map[string]any
		raw, _ := json.Marshal(clientCapabilities())
		_ = json.Unmarshal(raw, &want)
		wantJSON := `{"general":{"positionEncodings":["utf-16"]},"textDocument":{"definition":{"dynamicRegistration":false,"linkSupport":true},"diagnostic":{"dynamicRegistration":false,"relatedDocumentSupport":false},"publishDiagnostics":{"versionSupport":true},"references":{"dynamicRegistration":false},"rename":{"dynamicRegistration":false,"prepareSupport":false},"synchronization":{"didSave":true,"dynamicRegistration":false}},"workspace":{"applyEdit":false,"configuration":true,"workspaceEdit":{"documentChanges":true},"workspaceFolders":true}}`
		var exact map[string]any
		_ = json.Unmarshal([]byte(wantJSON), &exact)
		if !reflect.DeepEqual(initialize["capabilities"], exact) || !reflect.DeepEqual(want, exact) {
			t.Fatalf("capabilities = %v", initialize["capabilities"])
		}
		rootURI := fileURI(h.ws.root)
		folders := initialize["workspaceFolders"].([]any)
		if initialize["rootUri"] != rootURI || len(folders) != 1 || folders[0].(map[string]any)["uri"] != rootURI || initialize["processId"] == nil {
			t.Fatalf("root/folders: %v", initialize)
		}
		if !reflect.DeepEqual(initialize["initializationOptions"], map[string]any{"opt": true}) {
			t.Fatalf("initializationOptions: %v", initialize["initializationOptions"])
		}
		got := h.received("go")
		order := []string{"initialize", "initialized", "workspace/didChangeConfiguration", "textDocument/didOpen", "textDocument/definition"}
		if !reflect.DeepEqual(got[:5], order) {
			t.Fatalf("order %v", got)
		}
		if settings := h.params("go", "workspace/didChangeConfiguration")[0]["settings"]; !reflect.DeepEqual(settings, map[string]any{"gopls": map[string]any{"ui": map[string]any{"codelens": false}}}) {
			t.Fatalf("settings: %v", settings)
		}
		responses := map[string]map[string]any{}
		eventually(t, "probe responses", func() bool {
			responses = map[string]map[string]any{}
			for _, event := range h.events("go") {
				if event["event"] == "response" {
					responses[event["id"].(string)] = event
				}
			}
			return len(responses) == 6
		})
		configuration := responses["srv-1"]["result"]
		if wantConfig := []any{map[string]any{"ui": map[string]any{"codelens": false}}, map[string]any{"codelens": false}, nil, map[string]any{"gopls": map[string]any{"ui": map[string]any{"codelens": false}}}}; !reflect.DeepEqual(configuration, wantConfig) {
			t.Fatalf("configuration = %v", configuration)
		}
		if applied := responses["srv-2"]["result"].(map[string]any); applied["applied"] != false || applied["failureReason"] != "Tempest requires explicit edit tools" {
			t.Fatalf("applyEdit = %v", applied)
		}
		for _, id := range []string{"srv-3", "srv-4", "srv-5"} {
			if e, _ := responses[id]["error"].(map[string]any); e == nil || e["code"].(float64) != -32601 {
				t.Fatalf("%s should be MethodNotFound: %v", id, responses[id])
			}
		}
		if folders := responses["srv-6"]["result"].([]any); len(folders) != 1 || folders[0].(map[string]any)["uri"] != rootURI {
			t.Fatalf("workspaceFolders = %v", folders)
		}
	})
	t.Run("negotiation failures", func(t *testing.T) {
		for name, test := range map[string]struct {
			fixture map[string]any
			code    string
		}{
			"utf-8 encoding":       {map[string]any{"position_encoding": "utf-8"}, CodeUnsupported},
			"no open/close":        {map[string]any{"no_open_close": true}, CodeUnsupported},
			"sync none":            {map[string]any{"sync_kind": -1}, CodeUnsupported},
			"utf-16 encoding ok":   {map[string]any{"position_encoding": "utf-16"}, ""},
			"numeric sync kind ok": {map[string]any{"sync_number": true, "sync_kind": 2}, ""},
		} {
			t.Run(name, func(t *testing.T) {
				h := startHarness(t, setup{fixture: test.fixture})
				h.ws.write(t, "a.go", goSource)
				_, err := h.query(definitionAt("a.go", 5, 9))
				if test.code == "" {
					if err != nil {
						t.Fatal(err)
					}
					return
				}
				wantCode(t, err, test.code)
				if h.count("go", "textDocument/didOpen") != 0 {
					t.Fatal("documents sent to a rejected server")
				}
			})
		}
	})
	t.Run("missing provider is unsupported without sending", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"no_definition": true, "no_rename": true, "no_references": true}})
		h.ws.write(t, "a.go", goSource)
		_, err := h.query(definitionAt("a.go", 5, 9))
		wantCode(t, err, CodeUnsupported)
		_, err = h.query(Query{Method: MethodRename, Path: "a.go", Position: Position{5, 9}, NewName: "z"})
		wantCode(t, err, CodeUnsupported)
		_, err = h.query(Query{Method: MethodReferences, Path: "a.go", Position: Position{5, 9}})
		wantCode(t, err, CodeUnsupported)
		for _, method := range []string{"textDocument/definition", "textDocument/rename", "textDocument/references"} {
			if h.count("go", method) != 0 {
				t.Fatalf("%s sent without capability", method)
			}
		}
		if h.ws.snapshots != 0 {
			t.Fatal("unsupported rename scanned the workspace")
		}
	})
	t.Run("root is the session cwd, never a parent repository", func(t *testing.T) {
		parent := newDirWorkspace(t)
		parent.write(t, "go.mod", "module x\n")
		sub := filepath.Join(parent.root, "sub")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		ws := newDirWorkspace(t)
		r, err := os.OpenRoot(sub)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close() })
		ws.root, ws.r = sub, r
		h := startHarness(t, setup{ws: ws})
		ws.write(t, "a.go", goSource)
		h.mustQuery(definitionAt("a.go", 5, 9))
		if root := h.params("go", "initialize")[0]["rootUri"]; root != fileURI(sub) {
			t.Fatalf("rootUri %v, want %s", root, fileURI(sub))
		}
	})
}
