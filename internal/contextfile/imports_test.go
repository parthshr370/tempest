package contextfile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// expandIn expands content as dir/AGENTS.md with dir as the root and no byte
// limit on imports.
func expandIn(ctx context.Context, t *testing.T, dir, content, home string) (string, error) {
	t.Helper()
	text, _, err := Expand(ctx, openRoot(t, dir), filepath.Join(dir, "AGENTS.md"), []byte(content), ExpandOptions{HomeDir: home, MaxBytes: -1})
	return text, err
}

func TestExpandImportsAndCRLF(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "part.md"), "expanded")
	got, err := expandIn(context.Background(), t, dir, "before @./part.md\r\nafter", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "before expanded\r\nafter" {
		t.Fatalf("CRLF expansion = %q", got)
	}
}

func TestExpandLeavesProtectedAndRepeatedImportsLiteral(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "part.md"), "expanded")
	content := "@./part.md @./part.md\n`@./part.md`\n```\n@./part.md\n```\ngit@host:x user@example.com"
	got, err := expandIn(context.Background(), t, dir, content, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"expanded @./part.md", "`@./part.md`", "```\n@./part.md\n```", "git@host:x", "user@example.com"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expansion missing %q: %q", want, got)
		}
	}
}

func TestExpandBoundsCycle(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.md"), "@./b.md")
	mustWrite(t, filepath.Join(dir, "b.md"), "@./a.md")
	got, err := expandIn(context.Background(), t, dir, "@./a.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "@./a.md" {
		t.Fatalf("cycle expansion = %q", got)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// An absolute import and a ~ import inside the root expand; imports stop
// after MaxImportDepth edges.
func TestExpandAbsoluteHomePunctuationAndHopLimit(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	absolute := filepath.Join(dir, "absolute.md")
	mustWrite(t, absolute, "absolute")
	mustWrite(t, filepath.Join(home, "home.md"), "home")
	got, err := expandIn(context.Background(), t, dir, "@"+absolute+", @~/home.md! @./missing.md", home)
	if err != nil {
		t.Fatal(err)
	}
	if got != "absolute, home! @./missing.md" {
		t.Fatalf("path expansion = %q", got)
	}
	for index := 1; index <= 6; index++ {
		content := "leaf"
		if index < 6 {
			content = "@./f" + string(rune('0'+index+1)) + ".md"
		}
		mustWrite(t, filepath.Join(dir, "f"+string(rune('0'+index))+".md"), content)
	}
	got, err = expandIn(context.Background(), t, dir, "@./f1.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "@./f6.md" {
		t.Fatalf("hop limited expansion = %q", got)
	}
}

func TestExpandNormativeImportMatrix(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(nested, "two.md"), "nested body")
	mustWrite(t, filepath.Join(nested, "one.md"), "one @./two.md")
	mustWrite(t, filepath.Join(dir, "part.md"), "part")
	if err := os.Mkdir(filepath.Join(dir, "directory"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("relative paths follow each importer and preserve punctuation", func(t *testing.T) {
		got, err := expandIn(context.Background(), t, dir, "a @./nested/one.md b @./part.md.,;:!?)]}\"'", "")
		if err != nil {
			t.Fatal(err)
		}
		if want := "a one nested body b part.,;:!?)]}\"'"; got != want {
			t.Fatalf("Expand() = %q, want %q", got, want)
		}
	})
	t.Run("multiple imports and bare home stay literal", func(t *testing.T) {
		got, err := expandIn(context.Background(), t, dir, "@./part.md @./part.md @~", dir)
		if err != nil {
			t.Fatal(err)
		}
		if got != "part @./part.md @~" {
			t.Fatalf("Expand() = %q", got)
		}
	})
	t.Run("fences and multi backticks stay literal", func(t *testing.T) {
		content := "~~~~\n@./part.md\n~~~~~\n````\n@./part.md\n`````\n``@./part.md`` `@./part.md`"
		got, err := expandIn(context.Background(), t, dir, content, "")
		if err != nil {
			t.Fatal(err)
		}
		if got != content {
			t.Fatalf("protected imports changed: %q", got)
		}
	})
	t.Run("direct and indirect cycles remain literal", func(t *testing.T) {
		mustWrite(t, filepath.Join(dir, "direct.md"), "@./direct.md")
		mustWrite(t, filepath.Join(dir, "a.md"), "@./b.md")
		mustWrite(t, filepath.Join(dir, "b.md"), "@./a.md")
		for _, tc := range []struct {
			path string
			want string
		}{{"direct.md", "@./direct.md"}, {"a.md", "@./a.md"}} {
			got, err := expandIn(context.Background(), t, dir, "@./"+tc.path, "")
			if err != nil || got != tc.want {
				t.Fatalf("%s = %q, %v; want %q", tc.path, got, err, tc.want)
			}
		}
	})
	t.Run("missing root directory and cancellation", func(t *testing.T) {
		if _, _, err := Expand(context.Background(), nil, filepath.Join(dir, "AGENTS.md"), []byte("x"), ExpandOptions{}); err == nil {
			t.Fatal("expansion without a root succeeded")
		} else {
			var importErr *Error
			if !errors.As(err, &importErr) || importErr.Code != InvalidOptions {
				t.Fatalf("missing root error = %v", err)
			}
		}
		got, err := expandIn(context.Background(), t, dir, "@./directory", "")
		if err != nil || got != "@./directory" {
			t.Fatalf("directory target = %q, %v", got, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = expandIn(ctx, t, dir, "@./part.md", "")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled expansion error = %v", err)
		}
	})
}
