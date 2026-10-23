package lsp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/editdiff"
)

// applyLikeEditTool applies one plan file exactly as the edit tool does:
// anchor check on raw bytes, BOM strip, LF normalization, exact unique
// matching, original line endings restored.
func applyLikeEditTool(t *testing.T, ws *dirWorkspace, file planFile) {
	t.Helper()
	full := filepath.Join(ws.root, filepath.FromSlash(file.Path))
	raw, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range file.Edits {
		if edit.Anchor != editdiff.ContentAnchor(raw) || file.Anchor != edit.Anchor {
			t.Fatalf("%s: stale or missing anchor", file.Path)
		}
	}
	bom, content := editdiff.StripBOM(string(raw))
	ending := editdiff.DetectLineEnding(content)
	applied, err := editdiff.ApplyEditsToNormalizedContent(editdiff.NormalizeToLF(content), file.Edits, file.Path)
	if err != nil {
		t.Fatalf("edit tool rejects the plan for %s: %v", file.Path, err)
	}
	if err = os.WriteFile(full, []byte(bom+editdiff.RestoreLineEndings(applied.NewContent, ending)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func renamePlanOf(t *testing.T, value map[string]any) renamePlan {
	t.Helper()
	raw, _ := json.Marshal(value)
	var plan renamePlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

func renameAt(path string, line, column int, name string) Query {
	return Query{Method: MethodRename, Path: path, Position: Position{line, column}, NewName: name}
}

func TestLSPRenamePlanOnly(t *testing.T) {
	sources := map[string]string{
		"a.go": "package p\n\nfunc tag() int { return tag2 }\n\nvar x = tag() + tag()\nvar y = tag()\nvar y = tag()\n",
		"b.go": "package p\r\n\r\nvar z = tag()\r\n",
		"c.go": "\ufeffpackage p\n// tag here\nvar w = tag\n",
		"d.go": "package p\n\nvar unrelated = 1\n",
	}
	for _, form := range []string{"changes", "document_changes"} {
		t.Run("multi-file plan applies through the edit path: "+form, func(t *testing.T) {
			h := startHarness(t, setup{fixture: map[string]any{"rename_form": form}})
			for path, source := range sources {
				h.ws.write(t, path, source)
			}
			before := h.ws.hashes(t)
			value := h.mustQuery(renameAt("a.go", 5, 9, "renamed"))
			if after := h.ws.hashes(t); !reflect.DeepEqual(before, after) {
				t.Fatal("lsp_rename changed workspace bytes")
			}
			plan := renamePlanOf(t, value)
			if plan.Applied || plan.Server != "go" || plan.FileCount != 3 || len(plan.Files) != 3 || plan.NoChanges {
				t.Fatalf("plan %+v", plan)
			}
			edits := 0
			for i, file := range plan.Files {
				if want := []string{"a.go", "b.go", "c.go"}[i]; file.Path != want || file.Diff == "" {
					t.Fatalf("file %d = %s", i, file.Path)
				}
				edits += len(file.Edits)
				applyLikeEditTool(t, h.ws, file)
			}
			if edits != plan.EditCount {
				t.Fatalf("edit_count %d, edits %d", plan.EditCount, edits)
			}
			for path, source := range sources {
				data, _ := os.ReadFile(filepath.Join(h.ws.root, path))
				want := strings.NewReplacer("tag()", "renamed()", "tag\n", "renamed\n", "tag here", "renamed here").Replace(source)
				if string(data) != want {
					t.Fatalf("%s = %q, want %q", path, data, want)
				}
			}
		})
	}
	edit := func(line, start, end int, text string) string {
		return fmt.Sprintf(`{"range":{"start":{"line":%d,"character":%d},"end":{"line":%d,"character":%d}},"newText":%q}`, line, start, line, end, text)
	}
	t.Run("insertions, several edits per line and repeated lines merge into unique hunks", func(t *testing.T) {
		response := `{"changes":{"${ROOT}/a.go":[` + strings.Join([]string{edit(2, 0, 0, "X"), edit(2, 1, 2, "Y"), edit(0, 3, 3, "!"), edit(3, 0, 1, "😀")}, ",") + `]}}`
		h := startHarness(t, setup{fixture: map[string]any{"responses": map[string]any{"textDocument/rename": json.RawMessage(response)}}})
		h.ws.write(t, "a.go", "abc\nabc\nxyz\nq\nabc\n")
		plan := renamePlanOf(t, h.mustQuery(renameAt("a.go", 1, 1, "n")))
		if len(plan.Files) != 1 {
			t.Fatalf("plan %+v", plan)
		}
		for _, e := range plan.Files[0].Edits {
			if strings.Count("abc\nabc\nxyz\nq\nabc\n", e.OldText) != 1 {
				t.Fatalf("non-unique old_text %q", e.OldText)
			}
		}
		applyLikeEditTool(t, h.ws, plan.Files[0])
		if data, _ := os.ReadFile(filepath.Join(h.ws.root, "a.go")); string(data) != "abc!\nabc\nXxYz\n😀\nabc\n" {
			t.Fatalf("result %q", data)
		}
	})
	t.Run("whole plan rejections", func(t *testing.T) {
		outside := `{"changes":{"file:///etc/hosts":[` + edit(0, 0, 1, "x") + `]}}`
		for name, test := range map[string]struct {
			response string
			code     string
		}{
			"overlapping ranges":       {`{"changes":{"${ROOT}/a.go":[` + edit(0, 0, 2, "x") + `,` + edit(0, 1, 3, "y") + `]}}`, CodeRenameRejected},
			"same-offset insertions":   {`{"changes":{"${ROOT}/a.go":[` + edit(0, 1, 1, "x") + `,` + edit(0, 1, 1, "y") + `]}}`, CodeRenameRejected},
			"range beyond the line":    {`{"changes":{"${ROOT}/a.go":[` + edit(0, 0, 40, "x") + `]}}`, CodeRenameRejected},
			"outside-root URI":         {outside, CodeRenameRejected},
			"non-file URI":             {`{"changes":{"untitled:1":[` + edit(0, 0, 1, "x") + `]}}`, CodeRenameRejected},
			"resource operation":       {`{"documentChanges":[{"kind":"create","uri":"${ROOT}/new.go"}]}`, CodeRenameRejected},
			"both forms":               {`{"changes":{},"documentChanges":[]}`, CodeRenameRejected},
			"needs confirmation":       {`{"changes":{"${ROOT}/a.go":[` + edit(0, 0, 1, "x") + `]},"changeAnnotations":{"a":{"label":"l","needsConfirmation":true}}}`, CodeRenameRejected},
			"duplicate target":         {`{"documentChanges":[{"textDocument":{"uri":"${ROOT}/a.go","version":1},"edits":[]},{"textDocument":{"uri":"${ROOT}/a.go","version":1},"edits":[]}]}`, CodeRenameRejected},
			"empty file insertion":     {`{"changes":{"${ROOT}/empty.go":[` + edit(0, 0, 0, "x") + `]}}`, CodeRenameRejected},
			"target outside snapshot":  {`{"changes":{"${ROOT}/notes.txt":[` + edit(0, 0, 1, "x") + `]}}`, CodeRenameRejected},
			"stale document version":   {`{"documentChanges":[{"textDocument":{"uri":"${ROOT}/a.go","version":99},"edits":[` + edit(0, 0, 1, "x") + `]}]}`, CodeContentModified},
			"unopened version claimed": {`{"documentChanges":[{"textDocument":{"uri":"${ROOT}/b.go","version":1},"edits":[` + edit(0, 0, 1, "x") + `]}]}`, CodeContentModified},
			"too many edits":           {`{"changes":{"${ROOT}/a.go":[` + strings.TrimSuffix(strings.Repeat(edit(0, 0, 0, "x")+",", maxRenameEdits+1), ",") + `]}}`, CodeOutputLimit},
			"BOM edit":                 {`{"changes":{"${ROOT}/bom.go":[` + edit(0, 0, 1, "") + `]}}`, CodeRenameRejected},
			"line endings only":        {`{"changes":{"${ROOT}/a.go":[{"range":{"start":{"line":0,"character":3},"end":{"line":1,"character":0}},"newText":"\r\n"}]}}`, CodeRenameRejected},
		} {
			t.Run(name, func(t *testing.T) {
				h := startHarness(t, setup{fixture: map[string]any{"responses": map[string]any{"textDocument/rename": json.RawMessage(test.response)}}})
				h.ws.write(t, "a.go", "abc\nabc\n")
				h.ws.write(t, "b.go", "abc\n")
				h.ws.write(t, "empty.go", "")
				h.ws.write(t, "bom.go", "\ufeffabc\n")
				h.ws.write(t, "notes.txt", "abc\n")
				before := h.ws.hashes(t)
				_, err := h.query(renameAt("a.go", 1, 1, "n"))
				wantCode(t, err, test.code)
				if !reflect.DeepEqual(before, h.ws.hashes(t)) {
					t.Fatal("a rejected rename changed bytes")
				}
			})
		}
	})
	t.Run("empty WorkspaceEdit is an explicit no_changes plan", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"responses": map[string]any{"textDocument/rename": json.RawMessage(`{"changes":{}}`)}}})
		h.ws.write(t, "a.go", "abc\n")
		value := h.mustQuery(renameAt("a.go", 1, 1, "n"))
		if value["no_changes"] != true || value["applied"] != false || len(value["files"].([]any)) != 0 {
			t.Fatalf("no_changes plan: %v", value)
		}
	})
	t.Run("targets changed during the request invalidate the plan", func(t *testing.T) {
		for name, announce := range map[string]bool{"unannounced disk write": false, "Changed hook": true} {
			t.Run(name, func(t *testing.T) {
				h := startHarness(t, setup{fixture: map[string]any{"delay_ms": map[string]int{"textDocument/rename": 300}}})
				h.ws.write(t, "a.go", sources["a.go"])
				h.ws.write(t, "b.go", sources["b.go"])
				go func() {
					for h.count("go", "textDocument/rename") == 0 {
						time.Sleep(5 * time.Millisecond)
					}
					h.ws.write(t, "b.go", sources["b.go"]+"// edited\r\n")
					if announce {
						h.m.Changed("b.go")
					}
				}()
				_, err := h.query(renameAt("a.go", 5, 9, "renamed"))
				wantCode(t, err, CodeContentModified)
			})
		}
	})
	t.Run("snapshot bounds fail before dispatch", func(t *testing.T) {
		h := startHarness(t, setup{})
		h.ws.write(t, "a.go", "tag\n")
		h.ws.snapshotErr = &Error{Code: CodeRenameScopeLimit, Detail: "more than 256 files"}
		_, err := h.query(renameAt("a.go", 1, 1, "n"))
		wantCode(t, err, CodeRenameScopeLimit)
		h.ws.snapshotErr = nil
		for i := range maxSnapshotFiles {
			h.ws.write(t, fmt.Sprintf("gen/f%03d.go", i), "package gen\n")
		}
		_, err = h.query(renameAt("a.go", 1, 1, "n"))
		wantCode(t, err, CodeRenameScopeLimit)
		if h.count("go", "textDocument/rename") != 0 {
			t.Fatal("rename dispatched despite an unbounded snapshot")
		}
	})
	t.Run("oversize complete plan returns no partial plan", func(t *testing.T) {
		h := startHarness(t, setup{})
		var source strings.Builder
		source.WriteString("package p\n")
		for i := range 1500 {
			fmt.Fprintf(&source, "var v%04d = tag // padding padding padding padding padding padding\n", i)
		}
		h.ws.write(t, "a.go", source.String())
		_, err := h.query(renameAt("a.go", 2, 13, "renamed"))
		lspErr := wantCode(t, err, CodeOutputLimit)
		if !strings.Contains(lspErr.Detail, "no partial plan") {
			t.Fatalf("detail %q", lspErr.Detail)
		}
	})
	t.Run("only rename scans the workspace", func(t *testing.T) {
		h := startHarness(t, setup{})
		h.ws.write(t, "a.go", goSource)
		h.mustQuery(definitionAt("a.go", 5, 9))
		h.mustQuery(Query{Method: MethodReferences, Path: "a.go", Position: Position{5, 9}})
		h.mustQuery(Query{Method: MethodDiagnostics, Path: "a.go"})
		if h.ws.snapshots != 0 {
			t.Fatalf("%d snapshots outside rename", h.ws.snapshots)
		}
	})
}

func TestPlanEditsFallsBackToWholeFile(t *testing.T) {
	// A CR inserted before an LF composes into CRLF in the raw bytes; per-hunk
	// normalization can't reproduce that, so the plan must still be exact.
	original := []byte("a\nb\n")
	index := newLineIndex(original)
	text := "x\r"
	edits := []textEdit{{Range: &lspRange{Start: lspPosition{0, 1}, End: lspPosition{0, 1}}, NewText: &text}}
	updated, _, err := applyServerEdits("f.go", original, index, edits)
	if err != nil {
		t.Fatal(err)
	}
	planned, normOld, normNew, err := planEdits("f.go", original, updated, index, edits)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := editdiff.ApplyEditsToNormalizedContent(normOld, planned, "f.go")
	if err != nil || applied.NewContent != normNew || normNew != "ax\nb\n" {
		t.Fatalf("plan %+v gives %q (%v), want %q", planned, applied.NewContent, err, normNew)
	}
}
