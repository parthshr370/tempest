package skills

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestResolverLocate(t *testing.T) {
	base := t.TempDir()
	mustWrite(t, filepath.Join(base, "SKILL.md"), "skill body")
	resolver, err := NewResolver([]Skill{{Name: "alpha", FilePath: filepath.Join(base, "SKILL.md"), BaseDir: base}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		uri  string
		rel  string
		code ResolveErrorCode
	}{
		{"root", "skill://alpha", "SKILL.md", ""},
		{"root slash", "skill://alpha/", "SKILL.md", ""},
		{"escaped asset", "skill://alpha/notes/a%20b.md", filepath.Join("notes", "a b.md"), ""},
		{"escaped hash", "skill://alpha/hash%23.md", "hash#.md", ""},
		{"dot is the base", "skill://alpha/.", ".", ""},
		{"unknown", "skill://ALPHA", "", ResolveUnknownSkill},
		{"userinfo", "skill://user@alpha", "", ResolveInvalidURI},
		{"query", "skill://alpha?x=1", "", ResolveInvalidURI},
		{"empty query", "skill://alpha?", "", ResolveInvalidURI},
		{"fragment", "skill://alpha#fragment", "", ResolveInvalidURI},
		{"empty fragment", "skill://alpha#", "", ResolveInvalidURI},
		{"bad escape", "skill://alpha/%zz", "", ResolveInvalidURI},
		{"nul", "skill://alpha/%00", "", ResolveInvalidPath},
		{"backslash", "skill://alpha/notes%5Csecret.md", "", ResolveInvalidPath},
		{"raw traversal", "skill://alpha/../secret", "", ResolvePathEscape},
		{"decoded traversal", "skill://alpha/%2e%2e/secret", "", ResolvePathEscape},
		{"absolute path", "skill://alpha//absolute", "", ResolveInvalidPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			location, err := resolver.Locate(tc.uri)
			if tc.code != "" {
				var resolveErr *ResolveError
				if !errors.As(err, &resolveErr) || resolveErr.Code != tc.code {
					t.Fatalf("Locate(%q) error = %v, want %s", tc.uri, err, tc.code)
				}
				if tc.code == ResolveUnknownSkill && !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("unknown skill error does not unwrap fs.ErrNotExist: %v", err)
				}
				return
			}
			if err != nil || location.Rel != tc.rel || location.BaseDir != base || location.Skill != "alpha" {
				t.Fatalf("Locate(%q) = %#v, %v; want rel %q", tc.uri, location, err, tc.rel)
			}
		})
	}
}

func TestTargetEscapesBase(t *testing.T) {
	base, outside := t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(base, "inside.md"), "in")
	mustWrite(t, filepath.Join(outside, "secret.md"), "secret")
	links := map[string]string{
		"escape.md":  filepath.Join(outside, "secret.md"),
		"dangling":   filepath.Join(outside, "missing.md"),
		"sibling.md": filepath.Join(base, "inside.md"),
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(base, name)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	for name, want := range map[string]bool{"escape.md": true, "dangling": true, "sibling.md": false, "inside.md": false, "missing.md": false} {
		if got := TargetEscapesBase(base, name); got != want {
			t.Fatalf("TargetEscapesBase(%s) = %v, want %v", name, got, want)
		}
	}
}

func TestNewResolverRejectsSiblingPrefix(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "skill")
	sibling := base + "-other"
	mustWrite(t, filepath.Join(sibling, "SKILL.md"), "body")
	_, err := NewResolver([]Skill{{Name: "alpha", FilePath: filepath.Join(sibling, "SKILL.md"), BaseDir: base}})
	var resolveErr *ResolveError
	if !errors.As(err, &resolveErr) || resolveErr.Code != ResolveInvalidPath {
		t.Fatalf("NewResolver sibling path error = %v", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
