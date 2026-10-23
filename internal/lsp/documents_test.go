package lsp

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/editdiff"
)

// textEvents returns the fixture's (version, sha256) after each open/change of rel.
func (h *harness) textEvents(route, rel string) []string {
	uri := fileURI(filepath.ToSlash(filepath.Join(h.ws.root, rel)))
	var out []string
	for _, event := range h.events(route) {
		if event["event"] == "text" && event["uri"] == uri {
			out = append(out, fmt.Sprintf("%v:%v", event["version"], event["sha256"]))
		}
	}
	return out
}

func anchorOf(content string) string { return editdiff.ContentAnchor([]byte(content)) }

func TestLSPDocumentVersions(t *testing.T) {
	t.Run("didOpen v1, Changed emits didChange before the next query, coalescing keeps the latest", func(t *testing.T) {
		h := startHarness(t, setup{})
		h.ws.write(t, "a.go", goSource)
		h.mustQuery(definitionAt("a.go", 5, 9))
		if got := h.textEvents("go", "a.go"); !slices.Equal(got, []string{"1:" + anchorOf(goSource)}) {
			t.Fatalf("open: %v", got)
		}
		// A write whose tool then observed cancellation still calls Changed:
		// the worker must deliver it without any query.
		second := goSource + "// two\n"
		h.ws.write(t, "a.go", second)
		h.m.Changed("a.go")
		eventually(t, "worker didChange", func() bool { return len(h.textEvents("go", "a.go")) == 2 })
		final := ""
		for i := range 20 {
			final = goSource + strings.Repeat("// edit\n", i+3)
			h.ws.write(t, "a.go", final)
			h.m.Changed("a.go")
		}
		h.mustQuery(definitionAt("a.go", 5, 9))
		texts := h.textEvents("go", "a.go")
		last := texts[len(texts)-1]
		if !strings.HasSuffix(last, ":"+anchorOf(final)) {
			t.Fatalf("stale content won: %v", texts)
		}
		versions := h.params("go", "textDocument/didChange")
		for i, params := range versions {
			if got := params["textDocument"].(map[string]any)["version"].(float64); int(got) != i+2 {
				t.Fatalf("didChange %d has version %v: versions must increase by one", i, got)
			}
		}
		received := h.received("go")
		lastChange := slices.Index(received, "textDocument/didChange")
		for i, method := range received {
			if method == "textDocument/didChange" {
				lastChange = i
			}
		}
		if lastDefinition := len(received) - 1; received[lastDefinition] != "textDocument/definition" || lastChange > lastDefinition {
			t.Fatalf("query must follow synchronization: %v", received)
		}
	})
	t.Run("bash edits to open files are reconciled before a query", func(t *testing.T) {
		h := startHarness(t, setup{})
		h.ws.write(t, "a.go", goSource)
		h.ws.write(t, "b.go", "package p\n")
		h.mustQuery(definitionAt("a.go", 5, 9))
		h.mustQuery(definitionAt("b.go", 1, 1))
		external := "package p\n\nvar y = 1\n"
		h.ws.write(t, "b.go", external) // no Changed: an external program wrote it
		restored := goSource
		h.ws.write(t, "a.go", goSource+"// x\n")
		h.mustQuery(definitionAt("a.go", 5, 9))
		h.ws.write(t, "a.go", restored)
		h.mustQuery(definitionAt("a.go", 5, 9))
		if got := h.textEvents("go", "b.go"); len(got) != 2 || got[1] != "2:"+anchorOf(external) {
			t.Fatalf("b.go: %v", got)
		}
		// Restoring old content still moves the version forward.
		if got := h.textEvents("go", "a.go"); len(got) != 3 || got[2] != "3:"+anchorOf(restored) {
			t.Fatalf("a.go: %v", got)
		}
	})
	t.Run("incremental sync replaces the whole document using its old UTF-16 end", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"sync_kind": 2}})
		old := "package p\r\n\r\n// 😀 é\r\nfunc tag() {}\r\nvar x = tag 😀"
		h.ws.write(t, "a.go", old)
		h.mustQuery(definitionAt("a.go", 5, 9))
		updated := "package p\n\nfunc tag() {}\nvar x = tag\n"
		h.ws.write(t, "a.go", updated)
		h.mustQuery(definitionAt("a.go", 4, 9))
		change := h.params("go", "textDocument/didChange")[0]["contentChanges"].([]any)[0].(map[string]any)
		r := change["range"].(map[string]any)
		end := r["end"].(map[string]any)
		start := r["start"].(map[string]any)
		if start["line"].(float64) != 0 || start["character"].(float64) != 0 || end["line"].(float64) != 4 || end["character"].(float64) != 14 {
			t.Fatalf("range %v", r)
		}
		// The fixture applied the range with its own UTF-16 arithmetic.
		if got := h.textEvents("go", "a.go"); got[len(got)-1] != "2:"+anchorOf(updated) || h.count("go", "bad_change_range") != 0 {
			t.Fatalf("incremental application: %v", got)
		}
	})
	t.Run("full sync sends an unranged change", func(t *testing.T) {
		h := startHarness(t, setup{})
		h.ws.write(t, "a.go", goSource)
		h.mustQuery(definitionAt("a.go", 5, 9))
		h.ws.write(t, "a.go", goSource+"\n")
		h.mustQuery(definitionAt("a.go", 5, 9))
		change := h.params("go", "textDocument/didChange")[0]["contentChanges"].([]any)[0].(map[string]any)
		if _, ranged := change["range"]; ranged || change["text"] != goSource+"\n" {
			t.Fatalf("full change: %v", change)
		}
	})
	t.Run("didSave follows disk-backed changes per sync options", func(t *testing.T) {
		for name, test := range map[string]struct {
			save     any
			saves    int
			withText bool
		}{
			"include text": {map[string]any{"includeText": true}, 1, true},
			"bare save":    {true, 1, false},
			"no save":      {nil, 0, false},
		} {
			t.Run(name, func(t *testing.T) {
				fixture := map[string]any{}
				if test.save != nil {
					fixture["save"] = test.save
				}
				h := startHarness(t, setup{fixture: fixture})
				h.ws.write(t, "a.go", goSource)
				h.mustQuery(definitionAt("a.go", 5, 9))
				h.ws.write(t, "a.go", goSource+"\n")
				h.mustQuery(definitionAt("a.go", 5, 9))
				saves := h.params("go", "textDocument/didSave")
				if len(saves) != test.saves {
					t.Fatalf("saves %v", saves)
				}
				if test.saves == 1 {
					if _, hasText := saves[0]["text"]; hasText != test.withText {
						t.Fatalf("save text: %v", saves[0])
					}
					received := h.received("go")
					if slices.Index(received, "textDocument/didSave") < slices.Index(received, "textDocument/didChange") {
						t.Fatalf("didSave before didChange: %v", received)
					}
				}
			})
		}
	})
	t.Run("LRU closes the least recently used unpinned document", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"push": "none"}})
		for i := range maxOpenDocuments + 1 {
			h.ws.write(t, fmt.Sprintf("f%02d.go", i), "package p\n")
		}
		for i := range maxOpenDocuments {
			h.mustQuery(Query{Method: MethodDiagnostics, Path: fmt.Sprintf("f%02d.go", i)})
		}
		// Touch f00 so f01 becomes least recently used.
		h.mustQuery(Query{Method: MethodDiagnostics, Path: "f00.go"})
		h.mustQuery(Query{Method: MethodDiagnostics, Path: fmt.Sprintf("f%02d.go", maxOpenDocuments)})
		closes := h.params("go", "textDocument/didClose")
		if len(closes) != 1 || !strings.HasSuffix(closes[0]["textDocument"].(map[string]any)["uri"].(string), "/f01.go") {
			t.Fatalf("closes %v", closes)
		}
		if len(h.m.slots["go"].srv.openPaths()) != maxOpenDocuments {
			t.Fatal("open document count exceeds the cap")
		}
	})
	t.Run("dirty overflow reconciles every open document", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"hang": []string{"textDocument/definition"}}})
		h.ws.write(t, "a.go", goSource)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _, _ = h.queryCtx(ctx, definitionAt("a.go", 5, 9)) }()
		eventually(t, "hung query holds the lane", func() bool { return h.count("go", "textDocument/definition") == 1 })
		// a.go changes without its own notification; only unrelated paths are
		// announced, more than the dirty set can hold while the worker waits.
		h.ws.write(t, "a.go", goSource+"// changed\n")
		for i := range maxDirtyPaths + 10 {
			h.m.Changed(fmt.Sprintf("notes/%03d.txt", i))
		}
		h.m.mu.Lock()
		overflowed, size := h.m.slots["go"].dirtyAll, len(h.m.slots["go"].dirty)
		h.m.mu.Unlock()
		if !overflowed || size > maxDirtyPaths {
			t.Fatalf("dirty overflow %v with %d paths", overflowed, size)
		}
		cancel()
		<-done
		eventually(t, "reconcile-all didChange", func() bool {
			got := h.textEvents("go", "a.go")
			return len(got) == 2 && got[1] == "2:"+anchorOf(goSource+"// changed\n")
		})
	})
	t.Run("deleted open files are closed", func(t *testing.T) {
		h := startHarness(t, setup{})
		h.ws.write(t, "a.go", goSource)
		h.ws.write(t, "b.go", "package p\n")
		h.mustQuery(definitionAt("b.go", 1, 1))
		if err := os.Remove(filepath.Join(h.ws.root, "b.go")); err != nil {
			t.Fatal(err)
		}
		h.mustQuery(definitionAt("a.go", 5, 9))
		closes := h.params("go", "textDocument/didClose")
		if len(closes) != 1 || !strings.HasSuffix(closes[0]["textDocument"].(map[string]any)["uri"].(string), "/b.go") {
			t.Fatalf("closes %v", closes)
		}
	})
	t.Run("version exhaustion reopens in a fresh generation", func(t *testing.T) {
		h := startHarness(t, setup{})
		h.ws.write(t, "a.go", goSource)
		h.mustQuery(definitionAt("a.go", 5, 9))
		srv := h.m.slots["go"].srv
		srv.mu.Lock()
		srv.docs["a.go"].version = math.MaxInt32
		srv.mu.Unlock()
		h.ws.write(t, "a.go", goSource+"\n")
		h.mustQuery(definitionAt("a.go", 5, 9))
		if pids := h.pids("go"); len(pids) != 2 || h.count("go", "shutdown") != 1 {
			t.Fatalf("expected graceful retire and restart: pids %v", pids)
		}
		opens := h.params("go", "textDocument/didOpen")
		if len(opens) != 2 || opens[1]["textDocument"].(map[string]any)["version"].(float64) != 1 || len(h.params("go", "textDocument/didChange")) != 0 {
			t.Fatal("version must restart at 1 in the new generation, never wrap")
		}
	})
	t.Run("escaped outbound limit fails clearly instead of dropping a frame", func(t *testing.T) {
		h := startHarness(t, setup{})
		// 400 KiB of control characters escape to 2.4 MB of JSON.
		h.ws.write(t, "a.go", "package p\n// "+strings.Repeat("\x01", 400<<10)+"\n")
		_, err := h.query(definitionAt("a.go", 1, 1))
		wantCode(t, err, CodeOutputLimit)
		if h.count("go", "textDocument/didOpen") != 0 || h.count("go", "tempest/frame_dropped") != 0 {
			t.Fatal("oversized didOpen reached the server")
		}
		h.ws.write(t, "a.go", goSource)
		h.mustQuery(definitionAt("a.go", 5, 9))
	})
	t.Run("document bounds and encoding", func(t *testing.T) {
		h := startHarness(t, setup{})
		h.ws.write(t, "big.go", strings.Repeat("a", maxDocumentBytes+1))
		h.ws.write(t, "latin.go", "package p // caf\xe9\n")
		h.ws.write(t, "nul.go", "package p\x00\n")
		_, err := h.query(definitionAt("big.go", 1, 1))
		wantCode(t, err, CodeDocumentLimit)
		_, err = h.query(definitionAt("latin.go", 1, 1))
		wantCode(t, err, CodeInvalidDocument)
		_, err = h.query(definitionAt("nul.go", 1, 1))
		wantCode(t, err, CodeInvalidDocument)
		if h.count("go", "start") != 0 {
			t.Fatal("invalid documents started a server")
		}
	})
}
