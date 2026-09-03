// Containment tests prove that file tools refuse paths outside the working
// directory structurally: through os.Root and the ErrOutsideRoot sentinel.
// The pure-Go grep and find fallbacks are compared against rg and fd when
// those binaries are installed.
package tools

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/toolio"
)

// writeTempFile creates a file inside dir with content.
func writeTempFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// escapeTargets covers the shapes a model might use to reach outside cwd.
func escapeTargets() map[string]string {
	return map[string]string{
		"parent escape":     "../../etc/passwd",
		"absolute outside":  "/etc/passwd",
		"sub then up":       "sub/../../outside.txt",
		"dot-segment walk":  "a/../../../../outside.txt",
		"symlink escape":    "link/outside.txt",
		"symlink to secret": "secret-link",
	}
}

func TestReadRefusesPathsOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	writeTempFile(t, outside, "outside.txt", "secret")
	writeTempFile(t, dir, "inner.txt", "hello")
	if err := os.Symlink(filepath.Join(outside, "outside.txt"), filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "outside.txt"), filepath.Join(dir, "secret-link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	read, err := NewTool(ReadTool, dir, ToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range escapeTargets() {
		t.Run(name, func(t *testing.T) {
			_, err := executeTool(t, read, map[string]any{"path": target})
			if err == nil {
				t.Fatalf("read %q unexpectedly succeeded", target)
			}
			if !errors.Is(err, ErrOutsideRoot) {
				t.Fatalf("read %q error = %v, want ErrOutsideRoot", target, err)
			}
		})
	}
}

func TestWriteAndEditRefusePathsOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	writeTempFile(t, dir, "inner.txt", "one\ntwo\n")
	writeTempFile(t, outside, "victim.txt", "original\n")
	if err := os.Symlink(filepath.Join(outside, "victim.txt"), filepath.Join(dir, "victim-link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	write, err := NewTool(WriteTool, dir, ToolsOptions{MutationQueue: toolio.NewFileMutationQueue()})
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"parent escape":    "../escaped.txt",
		"absolute outside": filepath.Join(outside, "write-escaped.txt"),
		"sub then up":      "sub/../../write-escaped.txt",
		"symlink escape":   "victim-link",
	} {
		t.Run("write "+name, func(t *testing.T) {
			_, err := executeTool(t, write, map[string]any{"path": target, "content": "nope"})
			if !errors.Is(err, ErrOutsideRoot) {
				t.Fatalf("write %q error = %v, want ErrOutsideRoot", target, err)
			}
		})
	}

	edit, err := NewTool(EditTool, dir, ToolsOptions{MutationQueue: toolio.NewFileMutationQueue()})
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"parent escape":  "../inner.txt",
		"symlink escape": "victim-link",
	} {
		t.Run("edit "+name, func(t *testing.T) {
			_, err := executeTool(t, edit, map[string]any{
				"path":  target,
				"edits": []map[string]string{{"oldText": "one", "newText": "1"}},
			})
			if !errors.Is(err, ErrOutsideRoot) {
				t.Fatalf("edit %q error = %v, want ErrOutsideRoot", target, err)
			}
		})
	}

	// The escapes must not have written anything.
	data, err := os.ReadFile(filepath.Join(outside, "victim.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original\n" {
		t.Fatalf("victim file modified: %q", data)
	}
}

func TestLsRefusesPathsOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	ls, err := NewTool(LsTool, dir, ToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"../../", "/etc", ".."} {
		_, err := executeTool(t, ls, map[string]any{"path": target})
		if !errors.Is(err, ErrOutsideRoot) {
			t.Fatalf("ls %q error = %v, want ErrOutsideRoot", target, err)
		}
	}
}

func TestGrepAndFindRefusePathsOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "a.txt", "needle\n")
	for _, name := range []ToolName{GrepTool, FindTool} {
		tool, err := NewTool(name, dir, ToolsOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range []string{"../", "/etc"} {
			args := map[string]any{"pattern": "needle", "path": target}
			if _, err := executeTool(t, tool, args); !errors.Is(err, ErrOutsideRoot) {
				t.Fatalf("%s path %q error = %v, want ErrOutsideRoot", name, target, err)
			}
		}
	}
}

func TestReadInsideRootStillWorks(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "inner.txt", "hello world\n")
	read, err := NewTool(ReadTool, dir, ToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Absolute path inside the root is rewritten to its relative form.
	absolute := filepath.Join(dir, "inner.txt")
	result, err := executeTool(t, read, map[string]any{"path": absolute})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultText(result), "hello world") {
		t.Fatalf("absolute in-root read failed: %q", resultText(result))
	}
}

func TestWriteCreatesInsideRoot(t *testing.T) {
	dir := t.TempDir()
	write, err := NewTool(WriteTool, dir, ToolsOptions{MutationQueue: toolio.NewFileMutationQueue()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executeTool(t, write, map[string]any{"path": "a/b/c.txt", "content": "ok"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "a", "b", "c.txt"))
	if err != nil || string(data) != "ok" {
		t.Fatalf("nested write failed: %q %v", data, err)
	}
}

func TestGrepFindFallbackBackendDetail(t *testing.T) {
	dir := t.TempDir()
	// Force the pure-Go fallback even when rg/fd are installed.
	t.Setenv("PATH", "")
	writeTempFile(t, dir, "a.txt", "needle one\nneedle two\n")
	if err := os.MkdirAll(filepath.Join(dir, "ignored"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTempFile(t, dir, ".gitignore", "ignored/\n")
	// rg (and so the go grep fallback) honours .gitignore only inside a
	// Git worktree; a bare .git directory is enough to count as one.
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTempFile(t, dir, "ignored/hidden.txt", "needle three\n")
	writeTempFile(t, dir, "binary.txt", "ok\x00needle four\n")
	writeTempFile(t, dir, ".gitignore", "ignored/\n")

	grep, err := NewTool(GrepTool, dir, ToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	grepResult, err := executeTool(t, grep, map[string]any{"pattern": "needle"})
	if err != nil {
		t.Fatal(err)
	}
	grepDetails := resultDetailsMap(t, grepResult)
	if grepDetails["backend"] != "go" {
		t.Fatalf("grep backend = %v, want go", grepDetails["backend"])
	}
	text := resultText(grepResult)
	if !strings.Contains(text, "a.txt:1: needle one") || !strings.Contains(text, "a.txt:2: needle two") {
		t.Fatalf("go grep output = %q", text)
	}
	if strings.Contains(text, "hidden.txt") || strings.Contains(text, "binary.txt") {
		t.Fatalf("go grep leaked ignored or binary file: %q", text)
	}

	find, err := NewTool(FindTool, dir, ToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	findResult, err := executeTool(t, find, map[string]any{"pattern": "*.txt"})
	if err != nil {
		t.Fatal(err)
	}
	findDetails := resultDetailsMap(t, findResult)
	if findDetails["backend"] != "go" {
		t.Fatalf("find backend = %v, want go", findDetails["backend"])
	}
	findText := resultText(findResult)
	if strings.Contains(findText, "hidden.txt") {
		t.Fatalf("go find leaked ignored file: %q", findText)
	}
	if !strings.Contains(findText, "a.txt") {
		t.Fatalf("go find output = %q", findText)
	}
}

func TestGoGrepMatchingModes(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "file.txt", "before\nneedle here\nafter\nneedle there\nNeedle tail\n")
	writeTempFile(t, dir, "meta.txt", "a.b*c\n")
	root, err := openRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	matches, limitReached, err := goGrep(context.Background(), root, ".", "needle", "", false, false, 100)
	if err != nil || len(matches) != 2 || limitReached {
		t.Fatalf("case-sensitive go grep = %v, %v, %v", matches, limitReached, err)
	}
	if matches[0].lineNumber != 2 || matches[1].lineNumber != 4 {
		t.Fatalf("go grep line numbers = %d, %d", matches[0].lineNumber, matches[1].lineNumber)
	}
	matches, _, err = goGrep(context.Background(), root, ".", "needle", "", true, false, 100)
	if err != nil || len(matches) != 3 {
		t.Fatalf("case-insensitive go grep = %v, %v", matches, err)
	}
	// Literal mode treats regex metacharacters as text.
	matches, _, err = goGrep(context.Background(), root, ".", "a.b*c", "", false, true, 100)
	if err != nil || len(matches) != 1 {
		t.Fatalf("literal go grep = %v, %v", matches, err)
	}
	matches, _, err = goGrep(context.Background(), root, ".", "a.b*c", "", false, false, 100)
	if err != nil || len(matches) != 0 {
		t.Fatalf("regex go grep should not match literal dots: %v, %v", matches, err)
	}
}

func TestGoGrepLimitStopsWalk(t *testing.T) {
	dir := t.TempDir()
	for i := range 10 {
		writeTempFile(t, dir, "f"+string(rune('a'+i))+".txt", "needle\n")
	}
	root, err := openRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	matches, limitReached, err := goGrep(context.Background(), root, ".", "needle", "", false, false, 3)
	if err != nil || !limitReached || len(matches) != 3 {
		t.Fatalf("go grep limit = %d matches, limitReached=%v, err=%v", len(matches), limitReached, err)
	}
}

func TestGoSearchContextCancel(t *testing.T) {
	dir := t.TempDir()
	for i := range 100 {
		writeTempFile(t, dir, "f"+string(rune('a'+i%26))+string(rune('a'+i/26))+".txt", strings.Repeat("needle\n", 100))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root, err := openRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := goGrep(ctx, root, ".", "needle", "", false, false, 1000); err == nil {
		t.Fatal("canceled go grep should fail")
	}
	if _, _, err := goFind(ctx, root, ".", "*", 1000); err == nil {
		t.Fatal("canceled go find should fail")
	}
}

func TestGoGrepGlobFilter(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "a.go", "needle\n")
	writeTempFile(t, dir, "b.txt", "needle\n")
	writeTempFile(t, dir, "sub/c.go", "needle\n")
	root, err := openRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	matches, _, err := goGrep(context.Background(), root, ".", "needle", "*.go", false, false, 100)
	if err != nil || len(matches) != 2 {
		t.Fatalf("go grep glob = %v, %v", matches, err)
	}
}

func TestGrepFindFallbackMatchesFastPath(t *testing.T) {
	if _, ok := toolio.EnsureTool("rg"); !ok {
		t.Skip("rg not installed")
	}
	if _, ok := toolio.EnsureTool("fd"); !ok {
		t.Skip("fd not installed")
	}
	dir := t.TempDir()
	writeTempFile(t, dir, ".gitignore", "ignored/\n")
	writeTempFile(t, dir, "a.txt", "hello world\nsecond line\n")
	writeTempFile(t, dir, "b.txt", "HELLO there\n")
	writeTempFile(t, dir, "a.go", "package main\n")
	if err := os.MkdirAll(filepath.Join(dir, "ignored"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTempFile(t, dir, "ignored/hidden.txt", "hello ignored\n")
	writeTempFile(t, dir, "binary.dat", "text\x00hello binary\n")

	root, err := openRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	matches, _, err := goGrep(context.Background(), root, ".", "hello", "*.txt", true, false, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, match := range matches {
		got = append(got, formatSearchPath(dir, match.filePath, true)+":"+strconv.Itoa(match.lineNumber)+":"+match.lineText)
	}
	rgOut, err := execTool("rg", []string{"--line-number", "--color=never", "--hidden", "--ignore-case", "--glob", "*.txt", "--", "hello", dir})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{}
	separator := dir + string(filepath.Separator)
	for _, line := range nonEmptyLines(rgOut) {
		want = append(want, strings.Replace(line, separator, "", 1))
	}
	if diff := sortedDiff(got, want); diff != "" {
		t.Fatalf("go grep differs from rg:\n%s", diff)
	}

	found, _, err := goFind(context.Background(), root, ".", "*.txt", 1000)
	if err != nil {
		t.Fatal(err)
	}
	gotFind := []string{}
	for _, path := range found {
		gotFind = append(gotFind, formatSearchPath(dir, path, true))
	}
	fdOut, err := execTool("fd", []string{"--glob", "--color=never", "--hidden", "--no-require-git", "--", "*.txt", dir})
	if err != nil {
		t.Fatal(err)
	}
	wantFind := []string{}
	for _, line := range nonEmptyLines(fdOut) {
		wantFind = append(wantFind, formatSearchPath(dir, line, true))
	}
	if diff := sortedDiff(gotFind, wantFind); diff != "" {
		t.Fatalf("go find differs from fd:\n%s", diff)
	}
}

func TestGitignoreMatcher(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		rules   string
		path    string
		isDir   bool
		ignored bool
	}{
		{name: "dir rule", rules: "ignored/\n", path: "ignored", isDir: true, ignored: true},
		{name: "dir rule child", rules: "ignored/\n", path: "ignored/x.txt", isDir: false, ignored: true},
		{name: "dir rule nested", rules: "ignored/\n", path: "x/ignored/y.txt", isDir: false, ignored: true},
		{name: "file rule", rules: "ignored\n", path: "ignored", isDir: false, ignored: true},
		{name: "file rule nested", rules: "ignored\n", path: "sub/ignored", isDir: false, ignored: true},
		{name: "extension", rules: "*.log\n", path: "a/b/debug.log", isDir: false, ignored: true},
		{name: "extension no prefix", rules: "*.log\n", path: "a/b/debug.logx", isDir: false, ignored: false},
		{name: "anchored", rules: "/rooted.txt\n", path: "rooted.txt", isDir: false, ignored: true},
		{name: "anchored not nested", rules: "/rooted.txt\n", path: "sub/rooted.txt", isDir: false, ignored: false},
		{name: "doublestar", rules: "build/**/*.o\n", path: "build/x/y/z.o", isDir: false, ignored: true},
		{name: "doublestar miss", rules: "build/**/*.o\n", path: "build/x/y/z.c", isDir: false, ignored: false},
		{name: "comments and blanks", rules: "# comment\nkeep\n\n", path: "keep", isDir: false, ignored: true},
		{name: "basename not prefix", rules: "keep\n", path: "keep.txt", isDir: false, ignored: false},
		{name: "negation rule dropped", rules: "!special\nignored\n", path: "ignored", isDir: false, ignored: true},
		{name: "negation target not ignored", rules: "!special\nignored\n", path: "special", isDir: false, ignored: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			matcher := parseGitignore(testCase.rules)
			if got := matcher.Ignore(testCase.path, testCase.isDir); got != testCase.ignored {
				t.Fatalf("Ignore(%q, %v) with rules %q = %v, want %v", testCase.path, testCase.isDir, testCase.rules, got, testCase.ignored)
			}
		})
	}
}

// execTool runs an external search binary and returns its stdout. rg and fd
// exit 1 on no matches, which counts as valid empty output.
func execTool(name string, args []string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return string(out), nil
		}
		return "", err
	}
	return string(out), nil
}

// nonEmptyLines splits command output into trimmed non-blank lines.
func nonEmptyLines(out string) []string {
	lines := []string{}
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// sortedDiff compares two string sets and reports both, or "".
func sortedDiff(got, want []string) string {
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") == strings.Join(want, "\n") {
		return ""
	}
	return "got:\n" + strings.Join(got, "\n") + "\nwant:\n" + strings.Join(want, "\n")
}
