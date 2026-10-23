package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/editdiff"
	"go.harness.dev/harness/internal/hashline"
)

var headerTag = regexp.MustCompile(`\[([^\]#]+)#([0-9A-F]{4})\]`)

// tagOf returns the tag of path's header in a tool result.
func tagOf(t *testing.T, text, path string) string {
	t.Helper()
	for _, match := range headerTag.FindAllStringSubmatch(text, -1) {
		if match[1] == path {
			return match[2]
		}
	}
	t.Fatalf("no [%s#TAG] header in %q", path, text)
	return ""
}

// hashlineTools builds the tools of one session, sharing a store.
func hashlineTools(t *testing.T, dir string, changed *[]string) map[ToolName]agent.AgentTool {
	t.Helper()
	var mu sync.Mutex
	return AllTools(dir, ToolsOptions{OnFileChanged: func(path string) {
		mu.Lock()
		defer mu.Unlock()
		if changed != nil {
			*changed = append(*changed, path)
		}
	}})
}

func runHashline(t *testing.T, tool agent.AgentTool, input string) (string, error) {
	t.Helper()
	result, err := executeTool(t, tool, map[string]any{"input": input})
	if err != nil {
		return "", err
	}
	return resultText(result), nil
}

const goSource = `package demo

// Add sums two ints.
func Add(a, b int) int {
	return a + b
}

func Sub(a, b int) int {
	return a - b
}
`

func TestHashlineToolEditsMovesAndChains(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "demo.go"), goSource)
	var changed []string
	all := hashlineTools(t, dir, &changed)
	read, err := executeTool(t, all[ReadTool], map[string]any{"path": "demo.go", "mode": "raw"})
	if err != nil {
		t.Fatal(err)
	}
	tag := tagOf(t, resultText(read), "demo.go")
	out, err := runHashline(t, all[HashlineTool], fmt.Sprintf("[demo.go#%s]\nPUT 8*:\n+func Sub(a, b int) int {\n+\treturn b - a\n+}\nCUT 3* @add\nPUT >$ @add\nPUT >1:\n+\n+// Demo.\n", tag))
	if err != nil {
		t.Fatal(err)
	}
	want := "package demo\n\n// Demo.\n\n\nfunc Sub(a, b int) int {\n\treturn b - a\n}\n// Add sums two ints.\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"
	got, _ := os.ReadFile(filepath.Join(dir, "demo.go"))
	if string(got) != want {
		t.Fatalf("file = %q", got)
	}
	next := tagOf(t, out, "demo.go")
	if next != hashline.Tag(got) || !strings.Contains(out, "PUT 8*: -> lines 8-10") || !strings.Contains(out, "3:// Demo.") {
		t.Fatalf("result = %q", out)
	}
	if !slices.Equal(changed, []string{"demo.go"}) {
		t.Fatalf("OnFileChanged = %v", changed)
	}
	// Chain from the result: line 7 is `return b - a` in the new numbering.
	if _, err := runHashline(t, all[HashlineTool], fmt.Sprintf("[demo.go#%s]\nPUT 7.=7:\n+\treturn a - b\n", next)); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(filepath.Join(dir, "demo.go"))
	if !strings.Contains(string(got), "\treturn a - b\n") {
		t.Fatalf("chained edit: %q", got)
	}
	// The old tag is stale now.
	_, err = runHashline(t, all[HashlineTool], fmt.Sprintf("[demo.go#%s]\nPUT 1.=1:\n+package x\n", tag))
	var stale *hashline.StaleTagError
	if !errors.As(err, &stale) || !stale.Known {
		t.Fatalf("stale tag: %v", err)
	}
}

func TestHashlineToolIsAtomicAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "a1\na2\n")
	writeFile(t, filepath.Join(dir, "b.txt"), "b1\n")
	all := hashlineTools(t, dir, nil)
	tagA := tagOf(t, readThrough(t, all, "a.txt"), "a.txt")
	tagB := tagOf(t, readThrough(t, all, "b.txt"), "b.txt")
	_, err := runHashline(t, all[HashlineTool], fmt.Sprintf("[a.txt#%s]\nCUT 1.=1 @x\n[b.txt#%s]\nPUT 5.=5 @x\n", tagA, tagB))
	if err == nil || !strings.Contains(err.Error(), "b.txt has 1 lines") {
		t.Fatalf("err = %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(data) != "a1\na2\n" {
		t.Fatalf("a.txt written although b.txt failed: %q", data)
	}
	// MV outside the workspace is refused before anything lands.
	_, err = runHashline(t, all[HashlineTool], fmt.Sprintf("[a.txt#%s]\nPUT 1.=1:\n+A1\nMV ../escape.txt\n", tagA))
	if !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("escape err = %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(data) != "a1\na2\n" {
		t.Fatalf("a.txt written although MV was refused: %q", data)
	}
	// A cross-file move with MV and REM.
	out, err := runHashline(t, all[HashlineTool], fmt.Sprintf("[a.txt#%s]\nCUT 2.=2 @x\nMV \"sub dir/a.txt\"\n[b.txt#%s]\nREM\n", tagA, tagB))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("b.txt survived REM: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "sub dir", "a.txt")); string(data) != "a1\n" {
		t.Fatalf("moved file = %q (result %q)", data, out)
	}
	if !strings.Contains(out, "Deleted b.txt") || !strings.Contains(out, "Moved from a.txt") {
		t.Fatalf("result = %q", out)
	}
}

func readThrough(t *testing.T, all map[ToolName]agent.AgentTool, path string) string {
	t.Helper()
	result, err := executeTool(t, all[ReadTool], map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	return resultText(result)
}

func TestGrepTagsGateHashlineEdits(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	lines[19] = "needle"
	writeFile(t, filepath.Join(dir, "sub", "f.txt"), strings.Join(lines, "\n")+"\n")
	all := hashlineTools(t, dir, nil)
	grep, err := executeTool(t, all[GrepTool], map[string]any{"pattern": "needle", "path": "sub"})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(grep)
	tag := tagOf(t, text, "sub/f.txt")
	if !strings.Contains(text, "\n20:needle") {
		t.Fatalf("grep output = %q", text)
	}
	// The hit line is editable under the grep tag.
	if _, err := runHashline(t, all[HashlineTool], fmt.Sprintf("[sub/f.txt#%s]\nPUT 20.=20:\n+found\n", tag)); err != nil {
		t.Fatal(err)
	}
	// Lines grep never displayed are not.
	grep, _ = executeTool(t, all[GrepTool], map[string]any{"pattern": "found", "path": "sub"})
	tag = tagOf(t, resultText(grep), "sub/f.txt")
	_, err = runHashline(t, all[HashlineTool], fmt.Sprintf("[sub/f.txt#%s]\nPUT 5.=5:\n+five\n", tag))
	var unseen *hashline.UnseenLinesError
	if !errors.As(err, &unseen) || !slices.Equal(unseen.Lines, []int{5}) {
		t.Fatalf("unseen edit: %v", err)
	}
}

func TestEditAndWriteAcceptAndReturnTags(t *testing.T) {
	dir := t.TempDir()
	all := hashlineTools(t, dir, nil)
	write, err := executeTool(t, all[WriteTool], map[string]any{"path": "w.txt", "content": "one\ntwo\n"})
	if err != nil {
		t.Fatal(err)
	}
	tag := tagOf(t, resultText(write), "w.txt")
	if tag != hashline.Tag([]byte("one\ntwo\n")) || !strings.Contains(resultText(write), "1:one\n2:two") {
		t.Fatalf("write result = %q", resultText(write))
	}
	// Written content is known whole, so hashline may edit it unread.
	if _, err := runHashline(t, all[HashlineTool], fmt.Sprintf("[w.txt#%s]\nPUT 2.=2:\n+TWO\n", tag)); err != nil {
		t.Fatal(err)
	}
	tag = tagOf(t, readThrough(t, all, "w.txt"), "w.txt")
	edit, err := executeTool(t, all[EditTool], map[string]any{"path": "w.txt", "edits": []map[string]string{{"old_text": "one", "new_text": "ONE", "anchor": strings.ToLower(tag)}}})
	if err != nil {
		t.Fatal(err)
	}
	if out := resultText(edit); !strings.Contains(out, "[w.txt#"+hashline.Tag([]byte("ONE\nTWO\n"))+"]\n1:ONE") {
		t.Fatalf("edit result = %q", out)
	}
	_, err = executeTool(t, all[EditTool], map[string]any{"path": "w.txt", "edits": []map[string]string{{"old_text": "ONE", "new_text": "x", "anchor": tag}}})
	if !errors.Is(err, editdiff.ErrStaleAnchor) {
		t.Fatalf("stale 4-hex anchor: %v", err)
	}
	// The 64-hex anchor lsp_rename plans carry still works.
	data, _ := os.ReadFile(filepath.Join(dir, "w.txt"))
	if _, err := executeTool(t, all[EditTool], map[string]any{"path": "w.txt", "edits": []map[string]string{{"old_text": "ONE", "new_text": "1", "anchor": editdiff.ContentAnchor(data)}}}); err != nil {
		t.Fatal(err)
	}
}

func TestHashlineConcurrentCrossFilePatchesDoNotDeadlock(t *testing.T) {
	dir := t.TempDir()
	all := hashlineTools(t, dir, nil)
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, err := executeTool(t, all[WriteTool], map[string]any{"path": name, "content": "x\n"}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, order := range [][2]string{{"a.txt", "b.txt"}, {"b.txt", "a.txt"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			input := fmt.Sprintf("[%s#%s]\nPUT >$:\n+%s\n[%s#%s]\nPUT >$:\n+%s\n", order[0], hashline.Tag([]byte("x\n")), order[1], order[1], hashline.Tag([]byte("x\n")), order[0])
			_, err := runHashline(t, all[HashlineTool], input)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	succeeded := 0
	for err := range errs {
		var stale *hashline.StaleTagError
		switch {
		case err == nil:
			succeeded++
		case !errors.As(err, &stale):
			t.Fatalf("unexpected error: %v", err)
		}
	}
	// Both patches name the same pre-edit tags, so exactly one lands.
	if succeeded != 1 {
		t.Fatalf("%d patches applied", succeeded)
	}
}
