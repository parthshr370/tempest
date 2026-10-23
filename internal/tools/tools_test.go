package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/editdiff"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/progress"
	"go.harness.dev/harness/internal/skills"
	"go.harness.dev/harness/internal/toolio"
	"go.harness.dev/harness/internal/truncate"
)

func TestRegistry(t *testing.T) {
	dir := t.TempDir()
	all := AllTools(dir, ToolsOptions{})
	if len(all) != 9 || !AllToolNames[ReadTool] || !AllToolNames[BashTool] || !AllToolNames[TodoTool] || all[ManifestDiffTool].Execute == nil {
		t.Fatalf("registry = %v names=%v", all, AllToolNames)
	}
	for _, name := range LSPToolNames {
		if _, registered := all[name]; registered || !AllToolNames[name] {
			t.Fatalf("%s must be reserved but unregistered without a language-server manager", name)
		}
	}
	if _, err := NewTool("unknown", dir, ToolsOptions{}); err == nil || err.Error() != "Unknown tool name: unknown" {
		t.Fatalf("unknown err = %v", err)
	}
	if got := CodingTools(dir, ToolsOptions{}); names(got) != "read,bash,edit,write" {
		t.Fatalf("CodingTools = %s", names(got))
	}
	if got := ReadOnlyTools(dir, ToolsOptions{}); names(got) != "read,grep,find,ls" {
		t.Fatalf("ReadOnlyTools = %s", names(got))
	}
}

func TestSkillToolServesBody(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(dir, "database", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath, []byte("---\nname: database\ndescription: db skill\n---\nDB INSTRUCTIONS"), 0o644); err != nil {
		t.Fatal(err)
	}
	defs := []skills.Skill{{Name: "database", Description: "db skill", FilePath: skillPath, BaseDir: filepath.Dir(skillPath)}}
	tool, err := NewTool(SkillTool, dir, ToolsOptions{Skills: defs})
	if err != nil {
		t.Fatalf("NewTool(SkillTool) err = %v", err)
	}
	result, err := tool.Execute(context.Background(), "", json.RawMessage(`{"name":"database"}`), nil)
	if err != nil {
		t.Fatalf("skill execute err = %v", err)
	}
	if !strings.Contains(resultText(result), "DB INSTRUCTIONS") {
		t.Fatalf("skill result = %q", resultText(result))
	}
	result, err = tool.Execute(context.Background(), "", json.RawMessage(`{"name":"nope"}`), nil)
	if err != nil {
		t.Fatalf("skill miss err = %v", err)
	}
	if !strings.Contains(resultText(result), "Unknown skill") {
		t.Fatalf("skill miss result = %q", resultText(result))
	}
	if _, err := NewTool(SkillTool, dir, ToolsOptions{}); err == nil {
		t.Fatal("NewTool(SkillTool) without skills err = nil")
	}
}

func TestReadToolResolvesSkillURI(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "alpha")
	asset := filepath.Join(base, "reference.md")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "SKILL.md"), []byte("---\ndescription: alpha\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asset, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	items := []skills.Skill{{Name: "alpha", Description: "alpha", FilePath: filepath.Join(base, "SKILL.md"), BaseDir: base}}
	resolver, err := skills.NewResolver(items)
	if err != nil {
		t.Fatal(err)
	}
	read, err := NewTool(ReadTool, dir, ToolsOptions{ReadResolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	result, err := read.Execute(context.Background(), "", json.RawMessage(`{"path":"skill://alpha/reference.md:2-3"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(result)
	if !strings.Contains(text, "[skill://alpha/reference.md#") || !strings.Contains(text, "2:two") || strings.Contains(text, base) {
		t.Fatalf("skill read = %q", text)
	}
}

func TestReadToolReportsUnavailableSkillResolver(t *testing.T) {
	read, err := NewTool(ReadTool, t.TempDir(), ToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = read.Execute(context.Background(), "", json.RawMessage(`{"path":"skill://alpha/reference.md"}`), nil)
	var resourceErr *ReadResourceError
	if !errors.As(err, &resourceErr) || resourceErr.Code != "resolver_unavailable" {
		t.Fatalf("skill read without resolver error = %v", err)
	}
}

func TestWriteReadAndLsTools(t *testing.T) {
	dir := t.TempDir()
	queue := toolio.NewFileMutationQueue()
	write, _ := NewTool(WriteTool, dir, ToolsOptions{MutationQueue: queue})
	read, _ := NewTool(ReadTool, dir, ToolsOptions{})
	ls, _ := NewTool(LsTool, dir, ToolsOptions{})

	writeResult, err := executeTool(t, write, map[string]any{"path": "nested/file.txt", "content": "line1\nline2\nline3"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(writeResult); got != "Successfully wrote 17 bytes to nested/file.txt" {
		t.Fatalf("write result = %q", got)
	}

	readResult, err := executeTool(t, read, map[string]any{"path": "nested/file.txt", "offset": 2, "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(readResult); got != fmt.Sprintf("[nested/file.txt#%s]\n2:line2\n\n[1 more lines in file. Use offset=3 to continue.]", editdiff.ContentAnchor([]byte("line1\nline2\nline3"))) {
		t.Fatalf("read result = %q", got)
	}

	if err := os.Mkdir(filepath.Join(dir, "nested", "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	lsResult, err := executeTool(t, ls, map[string]any{"path": "nested"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(lsResult); got != "file.txt\nsubdir/" {
		t.Fatalf("ls result = %q", got)
	}
}

func TestLsFollowsSymlinkToDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	ls, _ := NewTool(LsTool, dir, ToolsOptions{})
	result, err := executeTool(t, ls, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(result); !strings.Contains(got, "link/") {
		t.Fatalf("ls result = %q", got)
	}
}

func TestReadToolPreservesSelectorTruncationContinuation(t *testing.T) {
	dir := t.TempDir()
	read, _ := NewTool(ReadTool, dir, ToolsOptions{})

	lineContent := strings.Repeat("x\n", truncate.DefaultMaxLines+1)
	writeFile(t, filepath.Join(dir, "lines.txt"), lineContent)
	lineResult, err := executeTool(t, read, map[string]any{"path": "lines.txt:1-2001"})
	if err != nil {
		t.Fatal(err)
	}
	lineHeader := fmt.Sprintf("[lines.txt#%s]", editdiff.ContentAnchor([]byte(lineContent)))
	lineNumbered := make([]string, truncate.DefaultMaxLines+1)
	for index := range lineNumbered {
		lineNumbered[index] = fmt.Sprintf("%d:x", index+1)
	}
	lineWant := lineHeader + "\n" + strings.Join(lineNumbered[:truncate.DefaultMaxLines], "\n") + "\n\n[Showing lines 1-2000 of selected lines 1-2001. Use lines.txt:2001-2001 to continue.]"
	if got := resultText(lineResult); got != lineWant {
		t.Fatalf("line-cap selector output = %q", got)
	}
	lineContinuation, err := executeTool(t, read, map[string]any{"path": "lines.txt:2001-2001"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(lineContinuation); got != lineHeader+"\n2001:x" {
		t.Fatalf("line-cap continuation = %q", got)
	}

	byteLines := make([]string, 1000)
	for i := range byteLines {
		byteLines[i] = strings.Repeat("z", 100)
	}
	byteContent := strings.Join(byteLines, "\n")
	writeFile(t, filepath.Join(dir, "bytes.txt"), byteContent)
	numbered := make([]string, len(byteLines))
	for i, line := range byteLines {
		numbered[i] = fmt.Sprintf("%d:%s", i+1, line)
	}
	truncated := truncate.Head(strings.Join(numbered, "\n"), truncate.Options{})
	if !truncated.Truncated || truncated.TruncatedBy != truncate.TruncatedByBytes {
		t.Fatalf("byte fixture truncation = %+v", truncated)
	}
	byteResult, err := executeTool(t, read, map[string]any{"path": "bytes.txt:1-1000"})
	if err != nil {
		t.Fatal(err)
	}
	nextLine := truncated.OutputLines + 1
	byteHeader := fmt.Sprintf("[bytes.txt#%s]", editdiff.ContentAnchor([]byte(byteContent)))
	byteWant := fmt.Sprintf("%s\n%s\n\n[Showing lines 1-%d of selected lines 1-1000 (50.0KB limit). Use bytes.txt:%d-1000 to continue.]", byteHeader, truncated.Content, truncated.OutputLines, nextLine)
	if got := resultText(byteResult); got != byteWant {
		t.Fatalf("byte-cap selector output = %q", got)
	}
	byteContinuation, err := executeTool(t, read, map[string]any{"path": fmt.Sprintf("bytes.txt:%d-1000", nextLine)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resultText(byteContinuation), "1001:") {
		t.Fatalf("byte-cap continuation escaped selector: %q", resultText(byteContinuation))
	}

	countResult, err := executeTool(t, read, map[string]any{"path": "lines.txt:1+2001"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(countResult); got != lineWant {
		t.Fatalf("START+COUNT continuation output = %q", got)
	}
}

func TestReadToolGoOutlineRawEscapesAndBinary(t *testing.T) {
	dir := t.TempDir()
	read, _ := NewTool(ReadTool, dir, ToolsOptions{})
	goSource := "package demo\n\nimport \"fmt\"\n\ntype ID = string\n\ntype Celsius float64\n\ntype Set[T comparable] map[T]struct{}\n\ntype Service struct {\n\tname string\n\ttags []string\n}\n\nconst MaxLen = 64\n\nvar client *Service\n\nfunc (s Service) Run(x int) error {\n\treturn fmt.Errorf(\"%d\", x)\n}\n"
	writeFile(t, filepath.Join(dir, "demo.go"), goSource)

	outlineResult, err := executeTool(t, read, map[string]any{"path": "demo.go"})
	if err != nil {
		t.Fatal(err)
	}
	outline := resultText(outlineResult)
	for _, expected := range []string{
		"package demo (line 1)",
		"imports:",
		"5: type ID = string [recover: demo.go:5-5]",
		"7: type Celsius float64 [recover: demo.go:7-7]",
		"9: type Set[T comparable] map[T]struct{} [recover: demo.go:9-9]",
		"11: type Service struct{ /* 2 fields */ } [recover: demo.go:11-14]",
		"16: const MaxLen = 64 [recover: demo.go:16-16]",
		"18: var client *Service [recover: demo.go:18-18]",
		"20: func (s Service) Run(x int) error [recover: demo.go:20-22]",
	} {
		if !strings.Contains(outline, expected) {
			t.Fatalf("outline missing %q: %q", expected, outline)
		}
	}
	if strings.Contains(outline, "return fmt") {
		t.Fatalf("outline leaked function body: %q", outline)
	}
	if strings.Contains(outline, "name string") {
		t.Fatalf("outline leaked struct body: %q", outline)
	}
	for _, testCase := range []struct {
		args        map[string]any
		expectedRaw string
	}{
		{args: map[string]any{"path": "demo.go", "mode": "raw"}, expectedRaw: "return fmt"},
		{args: map[string]any{"path": "demo.go:20-22"}, expectedRaw: "return fmt"},
		{args: map[string]any{"path": "demo.go", "offset": 21}, expectedRaw: "return fmt"},
		{args: map[string]any{"path": "demo.go", "limit": 2}, expectedRaw: "1:package demo"},
	} {
		result, executeErr := executeTool(t, read, testCase.args)
		if executeErr != nil {
			t.Fatal(executeErr)
		}
		if !strings.Contains(resultText(result), testCase.expectedRaw) {
			t.Fatalf("raw escape failed for %#v: %q", testCase.args, resultText(result))
		}
	}

	writeFile(t, filepath.Join(dir, "plain.txt"), "plain\n")
	plain, err := executeTool(t, read, map[string]any{"path": "plain.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(plain); !strings.Contains(got, "1:plain") {
		t.Fatalf("non-Go read was summarized: %q", got)
	}
	binaryContent := []byte{'a', 0, 'b'}
	if err := os.WriteFile(filepath.Join(dir, "binary.bin"), binaryContent, 0o644); err != nil {
		t.Fatal(err)
	}
	binary, err := executeTool(t, read, map[string]any{"path": "binary.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(binary); got != fmt.Sprintf("[binary.bin#%s]\n[binary file, 3 bytes, not shown]", editdiff.ContentAnchor(binaryContent)) {
		t.Fatalf("binary output = %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "image.png"), []byte("not-a-real-png"), 0o644); err != nil {
		t.Fatal(err)
	}
	image, err := executeTool(t, read, map[string]any{"path": "image.png"})
	if err != nil {
		t.Fatal(err)
	}
	if len(image.Content) != 2 || image.Content[0].Text != "Read image file [image/png]" {
		t.Fatalf("image output changed: %+v", image.Content)
	}
}

func TestEditToolRejectsFormerShortAnchorCollisionWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	readContent := []byte("content-83")
	currentContent := []byte("content-194")
	if err := os.WriteFile(file, readContent, 0o644); err != nil {
		t.Fatal(err)
	}
	read, _ := NewTool(ReadTool, dir, ToolsOptions{})
	readResult, err := executeTool(t, read, map[string]any{"path": "file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	header := strings.SplitN(resultText(readResult), "\n", 2)[0]
	anchor := strings.TrimSuffix(strings.TrimPrefix(header, "[file.txt#"), "]")
	if len(anchor) != editdiff.AnchorLength {
		t.Fatalf("read anchor length = %d", len(anchor))
	}
	if err := os.WriteFile(file, currentContent, 0o644); err != nil {
		t.Fatal(err)
	}
	edit, _ := NewTool(EditTool, dir, ToolsOptions{MutationQueue: toolio.NewFileMutationQueue()})
	_, err = executeTool(t, edit, map[string]any{"path": "file.txt", "edits": []map[string]string{{"old_text": "content", "new_text": "changed", "anchor": anchor}}})
	if !errors.Is(err, editdiff.ErrStaleAnchor) {
		t.Fatalf("collision edit error = %v", err)
	}
	data, readErr := os.ReadFile(file)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(data, currentContent) {
		t.Fatalf("collision edit mutated file: %q", data)
	}
}

func TestEditTool(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("one\r\ntwo\r\nthree\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edit, _ := NewTool(EditTool, dir, ToolsOptions{MutationQueue: toolio.NewFileMutationQueue()})
	raw := edit.PrepareArguments(mustJSON(t, map[string]any{"path": "file.txt", "old_text": "two", "new_text": "2"}))
	result, err := edit.Execute(context.Background(), "id", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(result); got != "Successfully replaced 1 block(s) in file.txt." {
		t.Fatalf("edit result = %q", got)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\r\n2\r\nthree\r\n" {
		t.Fatalf("file after edit = %q", string(data))
	}
	if result.Details == nil {
		t.Fatal("expected edit details")
	}
}

func TestReadToolAnchorsAndLineRangeSelector(t *testing.T) {
	dir := t.TempDir()
	content := "one\ntwo\nthree\nfour\n"
	writeFile(t, filepath.Join(dir, "file.txt"), content)
	read, _ := NewTool(ReadTool, dir, ToolsOptions{})

	result, err := executeTool(t, read, map[string]any{"path": "file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	wantHeader := fmt.Sprintf("[file.txt#%s]", editdiff.ContentAnchor([]byte(content)))
	if got := resultText(result); got != wantHeader+"\n1:one\n2:two\n3:three\n4:four" {
		t.Fatalf("read output = %q", got)
	}

	rangeResult, err := executeTool(t, read, map[string]any{"path": "file.txt:2-3"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(rangeResult); got != wantHeader+"\n2:two\n3:three" {
		t.Fatalf("range read output = %q", got)
	}
}

func TestEditToolAppliesCorrectAnchor(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	content := []byte("one\ntwo\n")
	if err := os.WriteFile(file, content, 0o644); err != nil {
		t.Fatal(err)
	}
	edit, _ := NewTool(EditTool, dir, ToolsOptions{MutationQueue: toolio.NewFileMutationQueue()})
	_, err := executeTool(t, edit, map[string]any{
		"path":     "file.txt",
		"old_text": "two",
		"new_text": "2",
		"anchor":   editdiff.ContentAnchor(content),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "one\n2\n" {
		t.Fatalf("file after anchored edit = %q", got)
	}
}

func TestEditToolRejectsStaleAnchorWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	readContent := []byte("one\ntwo\n")
	if err := os.WriteFile(file, readContent, 0o644); err != nil {
		t.Fatal(err)
	}
	currentContent := []byte("one\ntwo\nthree\n")
	if err := os.WriteFile(file, currentContent, 0o644); err != nil {
		t.Fatal(err)
	}
	edit, _ := NewTool(EditTool, dir, ToolsOptions{MutationQueue: toolio.NewFileMutationQueue()})
	_, err := executeTool(t, edit, map[string]any{
		"path": "file.txt",
		"edits": []map[string]string{{
			"old_text": "two",
			"new_text": "2",
			"anchor":   editdiff.ContentAnchor(readContent),
		}},
	})
	var stale *editdiff.StaleAnchorError
	if err == nil || !errors.As(err, &stale) || !errors.Is(err, editdiff.ErrStaleAnchor) {
		t.Fatalf("stale anchor err = %v", err)
	}
	data, readErr := os.ReadFile(file)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != string(currentContent) {
		t.Fatalf("stale edit mutated file: %q", data)
	}
}

func TestEditToolLeavesFileUnchangedWhenAnyHunkIsInvalid(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	content := "one\ntwo\nthree\n"
	writeFile(t, file, content)
	edit, _ := NewTool(EditTool, dir, ToolsOptions{MutationQueue: toolio.NewFileMutationQueue()})
	_, err := executeTool(t, edit, map[string]any{
		"path": "file.txt",
		"edits": []map[string]string{
			{"old_text": "one", "new_text": "1"},
			{"old_text": "missing", "new_text": "x"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "Could not find edits[1]") {
		t.Fatalf("invalid hunk err = %v", err)
	}
	data, readErr := os.ReadFile(file)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != content {
		t.Fatalf("partial edit mutated file: %q", data)
	}
}

func TestBashTool(t *testing.T) {
	dir := t.TempDir()
	bash, _ := NewTool(BashTool, dir, ToolsOptions{})
	result, err := executeTool(t, bash, map[string]any{"command": "printf hello"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(result); got != "hello" {
		t.Fatalf("bash result = %q", got)
	}

	_, err = executeTool(t, bash, map[string]any{"command": "printf fail; exit 7"})
	if err == nil || !strings.Contains(err.Error(), "Command exited with code 7") || !strings.Contains(err.Error(), "fail") {
		t.Fatalf("nonzero err = %v", err)
	}
}

func TestBashToolLargeOutputKeepsTail(t *testing.T) {
	dir := t.TempDir()
	bash, _ := NewTool(BashTool, dir, ToolsOptions{})
	result, err := executeTool(t, bash, map[string]any{"command": "for ((i=1;i<=50000;i++)); do printf '%06d\\n' \"$i\"; done"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(result); !strings.Contains(got, "050000") {
		t.Fatalf("large output tail missing final line: %q", got)
	}
}

func TestBashToolMissingCwdAndTimeoutValidation(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	bash, _ := NewTool(BashTool, missing, ToolsOptions{})
	_, err := executeTool(t, bash, map[string]any{"command": "true"})
	if err == nil || !strings.Contains(err.Error(), "Working directory does not exist") {
		t.Fatalf("missing cwd err = %v", err)
	}
	bash, _ = NewTool(BashTool, t.TempDir(), ToolsOptions{})
	_, err = executeTool(t, bash, map[string]any{"command": "true", "timeout": maxTimeoutSeconds + 1})
	if err == nil || !strings.Contains(err.Error(), "Invalid timeout") {
		t.Fatalf("timeout err = %v", err)
	}
}

func TestGrepAndFindTools(t *testing.T) {
	requireExternalTool(t, "rg")
	requireExternalTool(t, "fd")
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".gitignore"), "node_modules/\nignored/\n")
	writeFile(t, filepath.Join(dir, "a.go"), "package main\nfunc main() {}\n")
	writeFile(t, filepath.Join(dir, "b.txt"), "hello world\n")
	if err := os.Mkdir(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "node_modules", "ignored.go"), "package ignored\n")
	if err := os.MkdirAll(filepath.Join(dir, "ignored"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "ignored", "hidden.go"), "package hidden\n")

	grep, _ := NewTool(GrepTool, dir, ToolsOptions{})
	grepResult, err := executeTool(t, grep, map[string]any{"pattern": "HELLO", "ignore_case": true, "glob": "*.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(grepResult); got != "b.txt:1: hello world" {
		t.Fatalf("grep result = %q", got)
	}

	find, _ := NewTool(FindTool, dir, ToolsOptions{})
	findResult, err := executeTool(t, find, map[string]any{"pattern": "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(findResult); got != "a.go" {
		t.Fatalf("find result = %q", got)
	}
}

func TestGrepLimitCountsMatchesNotContextLines(t *testing.T) {
	requireExternalTool(t, "rg")
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "file.txt"), strings.Join([]string{"before one", "needle one", "after one", "before two", "needle two", "after two", "before three", "needle three"}, "\n"))
	grep, _ := NewTool(GrepTool, dir, ToolsOptions{})
	result, err := executeTool(t, grep, map[string]any{"pattern": "needle", "context": 1, "limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(resultText(result), "\n")
	matchLines := 0
	for _, line := range lines {
		if strings.Contains(line, ": needle") {
			matchLines++
		}
	}
	if matchLines != 2 || !strings.Contains(resultText(result), "before one") || !strings.Contains(resultText(result), "2 matches limit reached") {
		t.Fatalf("grep output = %q", resultText(result))
	}
}

func TestFindPathGlobAndGitignore(t *testing.T) {
	requireExternalTool(t, "fd")
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".gitignore"), "ignored/\n")
	if err := os.MkdirAll(filepath.Join(dir, "src", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "ignored", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "src", "nested", "main.go"), "package main\n")
	writeFile(t, filepath.Join(dir, "ignored", "src", "hidden.go"), "package hidden\n")
	find, _ := NewTool(FindTool, dir, ToolsOptions{})
	result, err := executeTool(t, find, map[string]any{"pattern": "src/**/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(result); got != "src/nested/main.go" {
		t.Fatalf("find result = %q", got)
	}
}

// TestTodoToolCheckpointsOutsideWorkspace guards against todo_write
// dirtying the user's working tree: the checkpoint goes to the session data
// directory, and without one the list stays in memory.
func TestTodoToolCheckpointsOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	sessionDataDir := filepath.Join(t.TempDir(), "session.jsonl.d")
	todos := map[string]any{"todos": []map[string]any{{"id": "a", "content": "implement", "status": "in_progress", "priority": "high"}}}
	todo, _ := NewTool(TodoTool, workspace, ToolsOptions{SessionDataDir: sessionDataDir})
	result, err := executeTool(t, todo, todos)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(sessionDataDir, progress.FileName)
	if got := resultText(result); !strings.Contains(got, "Todo list updated with 1 item") || !strings.Contains(got, checkpoint) {
		t.Fatalf("todo result = %q", got)
	}
	data, err := os.ReadFile(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "implement _(in progress, high priority)_") {
		t.Fatalf("progress.md = %q", string(data))
	}
	memoryOnly, _ := NewTool(TodoTool, workspace, ToolsOptions{})
	if _, err = executeTool(t, memoryOnly, todos); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(workspace); err != nil || len(entries) != 0 {
		t.Fatalf("todo_write wrote into the workspace: %v %v", entries, err)
	}
}

// TestReadSessionArtifacts lets the model read its own job patches and
// progress checkpoint, which live outside the workspace, without opening
// anything else in the session data directory.
func TestReadSessionArtifacts(t *testing.T) {
	workspace := t.TempDir()
	sessionDataDir := filepath.Join(t.TempDir(), "session.jsonl.d")
	patch := filepath.Join(sessionDataDir, "jobs", "job_a", "changes.patch")
	childJournal := filepath.Join(sessionDataDir, "children", "c", "session.jsonl")
	writeFile(t, patch, "diff --git a/x b/x\n")
	writeFile(t, filepath.Join(sessionDataDir, progress.FileName), "# Progress\n")
	writeFile(t, childJournal, "secret child transcript\n")
	if err := os.MkdirAll(filepath.Join(sessionDataDir, "jobs", "job_b"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(childJournal, filepath.Join(sessionDataDir, "jobs", "job_b", "changes.patch")); err != nil {
		t.Fatal(err)
	}
	spill := filepath.Join(sessionDataDir, ToolOutputDirName, "mcp", "x1", "result.json")
	writeFile(t, spill, `{"spilled":true}`)
	outside := filepath.Join(t.TempDir(), "other.txt")
	writeFile(t, outside, "elsewhere\n")

	read, _ := NewTool(ReadTool, workspace, ToolsOptions{SessionDataDir: sessionDataDir})
	for path, want := range map[string]string{patch: "diff --git", filepath.Join(sessionDataDir, progress.FileName): "# Progress", spill: `"spilled"`} {
		result, err := executeTool(t, read, map[string]any{"path": path})
		if err != nil || !strings.Contains(resultText(result), want) {
			t.Fatalf("read %s = %q, %v", path, resultText(result), err)
		}
	}
	for _, path := range []string{childJournal, filepath.Join(sessionDataDir, "jobs", "job_b", "changes.patch"), filepath.Join(sessionDataDir, "jobs", "job_a"), outside} {
		if result, err := executeTool(t, read, map[string]any{"path": path}); err == nil {
			t.Fatalf("read %s outside the artifact scope: %q", path, resultText(result))
		}
	}
	withoutSession, _ := NewTool(ReadTool, workspace, ToolsOptions{})
	if _, err := executeTool(t, withoutSession, map[string]any{"path": patch}); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("read without a session data directory: %v", err)
	}
}

// TestBashFullOutputIsReadable keeps the "Full output: <path>" promise: a
// truncated command's complete output lands in the session's tool-output
// directory, where read can open it.
func TestBashFullOutputIsReadable(t *testing.T) {
	workspace := t.TempDir()
	sessionDataDir := filepath.Join(t.TempDir(), "session.jsonl.d")
	options := ToolsOptions{SessionDataDir: sessionDataDir}
	bash, _ := NewTool(BashTool, workspace, options)
	result, err := executeTool(t, bash, map[string]any{"command": "seq 1 5000"})
	if err != nil {
		t.Fatal(err)
	}
	_, path, ok := strings.Cut(resultText(result), "Full output: ")
	if !ok {
		t.Fatalf("no full-output path: %q", resultText(result))
	}
	path = strings.TrimSuffix(path, "]")
	if !strings.HasPrefix(path, ToolOutputDir(sessionDataDir)+string(filepath.Separator)) {
		t.Fatalf("full output %s is outside %s", path, ToolOutputDir(sessionDataDir))
	}
	read, _ := NewTool(ReadTool, workspace, options)
	readResult, err := executeTool(t, read, map[string]any{"path": path + ":5000-5000"})
	if err != nil || !strings.Contains(resultText(readResult), "5000:5000") {
		t.Fatalf("read full output = %q, %v", resultText(readResult), err)
	}
	if entries, err := os.ReadDir(workspace); err != nil || len(entries) != 0 {
		t.Fatalf("bash spilled into the workspace: %v %v", entries, err)
	}
}

func TestTodoToolValidationErrorsPropagate(t *testing.T) {
	dir := t.TempDir()
	todo, _ := NewTool(TodoTool, dir, ToolsOptions{TodoStore: progress.NewStore(progress.Path(dir))})
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{name: "invalid status", args: map[string]any{"todos": []map[string]any{{"content": "implement", "status": "cancelled"}}}},
		{name: "empty content", args: map[string]any{"todos": []map[string]any{{"content": "", "status": "pending"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := executeTool(t, todo, tc.args)
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestWebToolsAreGated(t *testing.T) {
	dir := t.TempDir()
	all := AllTools(dir, ToolsOptions{})
	if _, ok := all[WebFetch]; ok {
		t.Fatal("web_fetch should be disabled by default")
	}
	if _, ok := all[WebSearch]; ok {
		t.Fatal("web_search should be disabled by default")
	}
}

func TestConfiguredToolsAreRegistered(t *testing.T) {
	dir := t.TempDir()
	configured := agent.AgentTool{Tool: ptypes.Tool{Name: "custom_agent"}, Label: "custom_agent", Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		return agent.AgentToolResult{}, nil
	}}
	all := AllTools(dir, ToolsOptions{ConfiguredTools: []agent.AgentTool{configured}})
	if _, ok := all[ToolName("custom_agent")]; !ok {
		t.Fatalf("configured tool missing from registry: %v", all)
	}
}

func TestConfiguredToolCannotShadowBuiltin(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.txt"), []byte("built-in read"), 0o600); err != nil {
		t.Fatal(err)
	}
	impostorExecuted := false
	configured := agent.AgentTool{
		Tool: ptypes.Tool{Name: string(ReadTool), Description: "configured impostor"},
		Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			impostorExecuted = true
			return agent.AgentToolResult{}, nil
		},
	}
	all := AllTools(dir, ToolsOptions{ConfiguredTools: []agent.AgentTool{configured}})
	read, ok := all[ReadTool]
	if !ok {
		t.Fatal("built-in read tool missing")
	}
	if read.Description == configured.Description {
		t.Fatalf("read description = %q; configured impostor was installed", read.Description)
	}
	if _, err := executeTool(t, read, map[string]any{"path": "sample.txt"}); err != nil {
		t.Fatalf("execute built-in read: %v", err)
	}
	if impostorExecuted {
		t.Fatal("configured read impostor executed")
	}
}

func TestWebFetchTool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello web")
	}))
	defer server.Close()
	tool, err := NewTool(WebFetch, t.TempDir(), ToolsOptions{EnableWeb: true, HTTPClient: rewriteClient(t, server.URL), ResolveIP: publicResolver})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executeTool(t, tool, map[string]any{"url": "http://example.com/fetch"})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(result); !strings.Contains(got, "Status: 200 OK") || !strings.Contains(got, "hello web") {
		t.Fatalf("web_fetch result = %q", got)
	}
}

func TestWebFetchBlocksInternalRedirect(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"http://127.0.0.1/secret"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	tool, err := NewTool(WebFetch, t.TempDir(), ToolsOptions{EnableWeb: true, HTTPClient: client, ResolveIP: publicResolver})
	if err != nil {
		t.Fatal(err)
	}
	_, err = executeTool(t, tool, map[string]any{"url": "http://example.com/redirect"})
	if err == nil || !strings.Contains(err.Error(), "private or local address") {
		t.Fatalf("redirect err = %v", err)
	}
}

func TestWebFetchBlocksInternalURL(t *testing.T) {
	tool, err := NewTool(WebFetch, t.TempDir(), ToolsOptions{EnableWeb: true, HTTPClient: &http.Client{}, ResolveIP: publicResolver})
	if err != nil {
		t.Fatal(err)
	}
	_, err = executeTool(t, tool, map[string]any{"url": "http://127.0.0.1/secret"})
	if err == nil || !strings.Contains(err.Error(), "private or local address") {
		t.Fatalf("internal url err = %v", err)
	}
}

func TestWebFetchClampsOversizedMaxBytes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", maxWebFetchBytes+16)))
	}))
	defer server.Close()
	tool, err := NewTool(WebFetch, t.TempDir(), ToolsOptions{EnableWeb: true, HTTPClient: rewriteClient(t, server.URL), ResolveIP: publicResolver})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executeTool(t, tool, map[string]any{"url": "http://example.com/large", "max_bytes": maxWebFetchBytes * 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(result); !strings.Contains(got, fmt.Sprintf("[Output truncated at %d bytes]", maxWebFetchBytes)) {
		t.Fatal("expected maxBytes to be clamped and reported")
	}
}

func TestWebFetchTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		fmt.Fprint(w, "late")
	}))
	defer server.Close()
	client := rewriteClient(t, server.URL)
	client.Timeout = 20 * time.Millisecond
	tool, err := NewTool(WebFetch, t.TempDir(), ToolsOptions{EnableWeb: true, HTTPClient: client, ResolveIP: publicResolver})
	if err != nil {
		t.Fatal(err)
	}
	_, err = executeTool(t, tool, map[string]any{"url": "http://example.com/slow"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestWebSearchTool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "widgets" || r.URL.Query().Get("limit") != "3" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		fmt.Fprint(w, "result one")
	}))
	defer server.Close()
	tool, err := NewTool(WebSearch, t.TempDir(), ToolsOptions{EnableWeb: true, HTTPClient: rewriteClient(t, server.URL), ResolveIP: publicResolver, SearchURL: "http://example.com/search"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executeTool(t, tool, map[string]any{"query": "widgets", "num_results": 3})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(result); !strings.Contains(got, "Query: widgets") || !strings.Contains(got, "result one") {
		t.Fatalf("web_search result = %q", got)
	}
}

func TestToolCancellation(t *testing.T) {
	dir := t.TempDir()
	bash, _ := NewTool(BashTool, dir, ToolsOptions{})
	start := time.Now()
	_, err := executeTool(t, bash, map[string]any{"command": "sleep 2", "timeout": 1})
	if err == nil || !strings.Contains(err.Error(), "Command timed out after 1 seconds") {
		t.Fatalf("timeout err = %v", err)
	}
	if time.Since(start) > 1800*time.Millisecond {
		t.Fatal("timeout took too long")
	}
}

func executeTool(t *testing.T, tool agent.AgentTool, args map[string]any) (agent.AgentToolResult, error) {
	t.Helper()
	raw := mustJSON(t, args)
	if tool.PrepareArguments != nil {
		raw = tool.PrepareArguments(raw)
	}
	return tool.Execute(context.Background(), "id", raw, nil)
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func resultDetailsMap(t *testing.T, result agent.AgentToolResult) map[string]any {
	t.Helper()
	var details map[string]any
	if err := json.Unmarshal(result.Details, &details); err != nil {
		t.Fatalf("details unmarshal: %v", err)
	}
	return details
}

func resultText(result agent.AgentToolResult) string {
	if len(result.Content) == 0 {
		return ""
	}
	return result.Content[0].Text
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func requireExternalTool(t *testing.T, name string) {
	t.Helper()
	if _, ok := toolio.EnsureTool(name); !ok {
		t.Skipf("%s not found in PATH", name)
	}
}

func names(tools []agent.AgentTool) string {
	parts := make([]string, len(tools))
	for i, tool := range tools {
		parts[i] = tool.Name
	}
	return strings.Join(parts, ",")
}

func publicResolver(context.Context, string) ([]net.IP, error) {
	return []net.IP{net.ParseIP("93.184.216.34")}, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func rewriteClient(t *testing.T, rawTarget string) *http.Client {
	t.Helper()
	target, err := url.Parse(rawTarget)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL.Scheme = target.Scheme
		clone.URL.Host = target.Host
		clone.Host = target.Host
		return transport.RoundTrip(clone)
	})}
}
func TestGoOutlineElidesNestedBodies(t *testing.T) {
	src := []byte("package demo\n\ntype M map[string]struct {\n\tsecret string\n}\n\ntype H interface {\n\tDo(cfg struct{ token string }) error\n}\n\ntype Wrapper[T ~struct{ leak string } | ~int] struct {\n\tv T\n}\n\nfunc Wrap(fn func(struct{ hidden int }) error) {}\n\nvar conf = struct{ password string }{\"p\"}\n\nvar handler = func(k string) { launch() }\n")
	outline, ok := goSourceOutline("demo.go", "demo.go", src)
	if !ok {
		t.Fatal("expected Go outline")
	}
	for _, leak := range []string{"secret", "token", "hidden", "password", "launch", "leak"} {
		if strings.Contains(outline, leak) {
			t.Fatalf("outline leaked nested body %q: %q", leak, outline)
		}
	}
	for _, want := range []string{
		"type M map[string]struct{ /* 1 fields */ }",
		"type H interface{ /* 1 members */ }",
		"type Wrapper[T ~struct{ /* 1 fields */ } | ~int] struct{ /* 1 fields */ }",
		"func Wrap(fn func(struct{ /* 1 fields */ }) error)",
		"var conf = struct{ /* 1 fields */ }{…}",
		"var handler = func(k string) {…}",
	} {
		if !strings.Contains(outline, want) {
			t.Fatalf("outline missing %q: %q", want, outline)
		}
	}
}
func TestTextResultSurfacesMarshalError(t *testing.T) {
	result := textResult("body", make(chan int))
	var payload map[string]string
	if err := json.Unmarshal(result.Details, &payload); err != nil {
		t.Fatalf("details unmarshal: %v", err)
	}
	if payload["details_marshal_error"] == "" {
		t.Fatalf("expected surfaced marshal error, got %v", payload)
	}
}
