package contextfile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

// Imports stay inside the root: an absolute path, a ../ path, a symlink out
// of the root and a ~ import whose home is outside the root all stay literal.
func TestExpandKeepsImportsInsideRoot(t *testing.T) {
	const (
		secret     = "SYNTHETIC_OUTSIDE_SECRET"
		roomForAll = 1 << 20
	)
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.md"), secret)
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "inside.md"), "inside")
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
	upward, err := filepath.Rel(dir, filepath.Join(outside, "secret.md"))
	if err != nil {
		t.Fatal(err)
	}
	source := "@./inside.md @" + filepath.Join(outside, "secret.md") + " @" + upward + " @./link.md @~/secret.md"
	got, complete, err := Expand(context.Background(), openRoot(t, dir), filepath.Join(dir, "AGENTS.md"), []byte(source), ExpandOptions{HomeDir: outside, MaxBytes: roomForAll})
	if err != nil || !complete {
		t.Fatalf("expand: complete=%v err=%v", complete, err)
	}
	if !strings.HasPrefix(got, "inside ") || strings.Contains(got, secret) {
		t.Fatalf("expansion = %q", got)
	}
}

// Imports read no more than the byte limit together; a cut import marks the
// expansion incomplete and later imports are not opened.
func TestExpandBoundsImportBytes(t *testing.T) {
	const (
		limit     = 16
		bigImport = 4 << 10
	)
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "big.md"), strings.Repeat("B", bigImport))
	mustWrite(t, filepath.Join(dir, "late.md"), "LATE")
	got, complete, err := Expand(context.Background(), openRoot(t, dir), filepath.Join(dir, "AGENTS.md"), []byte("@./big.md @./late.md"), ExpandOptions{MaxBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	if complete || strings.Count(got, "B") != limit || strings.Contains(got, "LATE") {
		t.Fatalf("expansion = %q (complete=%v), want %d bytes of big.md and late.md unread", got, complete, limit)
	}
}
