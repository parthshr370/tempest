package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/hashline"
	"go.harness.dev/harness/internal/truncate"
)

func TestReadPathsReturnsOneTaggedSectionPerFile(t *testing.T) {
	dir := t.TempDir()
	alpha := "one\ntwo\nthree\nfour\nfive\n"
	beta := "package beta\n"
	writeFile(t, filepath.Join(dir, "a.txt"), alpha)
	writeFile(t, filepath.Join(dir, "pkg", "b.go"), beta)
	all := AllTools(dir, ToolsOptions{})
	result, err := executeTool(t, all[ReadTool], map[string]any{"paths": []string{"a.txt:2-3", "pkg/b.go", "missing.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("[a.txt#%s]\n2:two\n3:three\n\n[pkg/b.go#%s]\n1:package beta\n\n[missing.txt: ", hashline.Tag([]byte(alpha)), hashline.Tag([]byte(beta)))
	if text := resultText(result); !strings.HasPrefix(text, want) {
		t.Fatalf("batched read =\n%s\nwant prefix\n%s", text, want)
	}
	var details readBatchDetails
	if err := json.Unmarshal(result.Details, &details); err != nil || len(details.Files) != 3 || details.Files[2].Error == "" || details.Files[0].Error != "" {
		t.Fatalf("details = %+v, %v", details, err)
	}

	// Each section records only the rows it displayed: rows 2-3 of a.txt
	// are editable under its tag, row 5 is not.
	tag := hashline.Tag([]byte(alpha))
	if _, err := runHashline(t, all[HashlineTool], fmt.Sprintf("[a.txt#%s]\nPUT 2.=2:\n+TWO\n", tag)); err != nil {
		t.Fatalf("edit of a displayed row: %v", err)
	}
	var unseen *hashline.UnseenLinesError
	if _, err := runHashline(t, all[HashlineTool], fmt.Sprintf("[a.txt#%s]\nPUT 5.=5:\n+FIVE\n", hashline.Tag([]byte("one\nTWO\nthree\nfour\nfive\n")))); !errors.As(err, &unseen) {
		t.Fatalf("edit of an undisplayed row = %v", err)
	}
}

func TestReadPathsShareOneOutputBudget(t *testing.T) {
	dir := t.TempDir()
	rows := make([]string, 1500)
	for index := range rows {
		rows[index] = fmt.Sprintf("row %d", index+1)
	}
	content := strings.Join(rows, "\n") + "\n"
	paths := []string{"a.txt", "b.txt", "c.txt"}
	for _, name := range paths {
		writeFile(t, filepath.Join(dir, name), content)
	}
	read, err := NewTool(ReadTool, dir, ToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executeTool(t, read, map[string]any{"paths": paths})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(result)
	if lines := strings.Count(text, "\n") + 1; lines > truncate.DefaultMaxLines+10 || len(text) > truncate.DefaultMaxBytes+1024 {
		t.Fatalf("batched read returned %d lines, %d bytes", lines, len(text))
	}
	// a.txt fits whole; b.txt gets the rest of the budget and continues by
	// selector, since a batch has no offset; c.txt is named, not read.
	if !strings.Contains(text, "1500:row 1500\n\n[b.txt#") || !strings.Contains(text, "Use b.txt:") || !strings.Contains(text, "[c.txt: not read; this call's output budget") {
		t.Fatalf("budget sections:\n%s", text[max(0, len(text)-600):])
	}
}

func TestReadPathAndPathsAreExclusive(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "a\n")
	read, err := NewTool(ReadTool, dir, ToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range []map[string]any{
		{"path": "a.txt", "paths": []string{"a.txt"}},
		{"paths": []string{"a.txt"}, "offset": 2},
		{},
		{"paths": strings.Split(strings.Repeat("a.txt,", maxReadPaths+1), ",")[:maxReadPaths+1]},
		{"paths": []string{"missing.txt"}},
	} {
		if _, err := executeTool(t, read, args); err == nil {
			t.Fatalf("%v: expected an error", args)
		}
	}
}

func TestGrepPathsSearchesEveryEntryOnce(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "one", "a.txt"), "needle one\n")
	writeFile(t, filepath.Join(dir, "two", "b.txt"), "needle two\n")
	writeFile(t, filepath.Join(dir, "three", "c.txt"), "needle three\n")
	grep, err := NewTool(GrepTool, dir, ToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executeTool(t, grep, map[string]any{"pattern": "needle", "paths": []string{"one", "two/b.txt", "two"}})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(result)
	if !strings.Contains(text, "needle one") || strings.Count(text, "needle two") != 1 || strings.Contains(text, "needle three") {
		t.Fatalf("grep paths =\n%s", text)
	}
	for _, args := range []map[string]any{
		{"pattern": "needle", "path": "one", "paths": []string{"two"}},
		{"pattern": "needle", "paths": []string{"missing"}},
		{"pattern": "needle", "paths": []string{"local://x"}},
	} {
		if _, err := executeTool(t, grep, args); err == nil {
			t.Fatalf("%v: expected an error", args)
		}
	}
}
