package permission

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scopeFixture builds the tree the fail-closed directory rows run in. Every
// x holds uncommitted work; a/, sub/ and sub/a/ have none, so a guard that
// believes the command moved into one of them would allow git checkout x.
//
//	root/x  root/a/  root/a/link -> root/other/inner  root/other/x
//	root/other/inner/  root/b/x  root/sub/a/  root/sub/b/
func scopeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"a", "other/inner", "b", "sub/a", "sub/b"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"x", "other/x", "b/x"} {
		if err := os.WriteFile(filepath.Join(root, file), []byte("dirty"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "other", "inner"), filepath.Join(root, "a", "link")); err != nil {
		t.Fatal(err)
	}
	return root
}

func clearShellEnvironment(t *testing.T) {
	for _, name := range []string{"CDPATH", "BASH_ENV", "SHELLOPTS"} {
		t.Setenv(name, "")
	}
}

func checkGuarded(t *testing.T, dir, command string, guarded bool) {
	t.Helper()
	for _, mode := range []Mode{ModeDefault, ModeAcceptEdits} {
		out := GateInDirectory(context.Background(), mode, "bash", commandArgs(command), &Policy{}, dir)
		if out.Allow == guarded || guarded && out.Source != SourceGuard {
			t.Errorf("dir=%s mode=%s outcome=%+v; guarded=%v", filepath.Base(dir), mode, out, guarded)
		}
	}
}

// TestDestructiveGuardDirectoryFailsClosed covers the walker's fail-closed
// rule: it follows only a plain cd or pushd it fully understands, anything
// else that may change the directory leaves it unknown, and no relative
// path climbs out of an unknown directory.
func TestDestructiveGuardDirectoryFailsClosed(t *testing.T) {
	clearShellEnvironment(t)
	root := scopeFixture(t)
	sub := filepath.Join(root, "sub")
	tests := []struct {
		dir, command string
		guarded      bool
	}{
		// An unknown directory absorbs "..": -C .., ../x and cd .. stay unknown.
		{sub, "cd a; cd -; git -C .. checkout x", true},
		{sub, "cd a; cd -; git checkout ../x", true},
		{sub, "cd -; cd ..; git checkout x", true},
		{sub, "cd -; echo hi > ../x", true},
		{sub, "cd -; git --work-tree=.. checkout x", true},
		// A redirection can fail and skip the cd; options are not followed.
		{root, "cd a < missing-input; git checkout x", true},
		{root, "cd a 2>missing-dir/error; git checkout x", true},
		{root, "{ cd a; } < missing-input; git checkout x", true},
		{root, "cd -Z a; git checkout x", true},
		{root, "cd -L -P a; git checkout x", true},
		{root, "pushd -n a; git checkout x", true},
		// Physical traversal and a ".." after a missing component.
		{root, "cd -P a/link/..; git checkout x", true},
		{root, "set -P; cd a/link/..; git checkout x", true},
		{root, "set -o physical; cd a/link/..; git checkout x", true},
		{root, "cd a/missing/..; git checkout x", true},
		// A ".." that leaves a symlink reaches other/, not a/.
		{root, "cd a/link/.. && git checkout x", true},
		{root, "echo hi > a/link/../x", true},
		{root, "cp b/x a/link/../x", true},
		{root, "git -C a/link/.. checkout x", true},
		// A wrapper or a prefix the walker does not follow hides the cd.
		{sub, "time cd ../b; git checkout x", true},
		{sub, "time -p cd ../b; git checkout x", true},
		{sub, "command builtin cd ../b; git checkout x", true},
		{sub, "nohup cd ../b; git checkout x", true},
		{sub, "A=1 cd ../b; git checkout x", true},
		{root, "cd a; popd; git checkout x", true},
		// Text run as commands, later traps, replaced builtins and functions.
		{sub, "eval cd ../b; git checkout x", true},
		{sub, "source ./env.sh; git checkout x", true},
		{sub, ". ./env.sh; git checkout x", true},
		{sub, "trap 'cd ../b' DEBUG; git checkout x", true},
		{sub, "c=cd; $c ../b; git checkout x", true},
		{root, "enable -n cd; cd a; git checkout x", true},
		{root, "shopt -s expand_aliases; alias cd=:\ncd a; git checkout x", true},
		{root, "cd() { :; }; cd a; git checkout x", true},
		{root, "f() { :; }; cd a && git checkout x", true},
		{root, "for i in 1 2; do cd " + filepath.Join(root, "a") + "; git checkout x; eval \"$1\"; done", true},
		// CDPATH sends cd b to ../b, not the empty sub/b.
		{sub, "export CDPATH=..; cd b; git checkout x", true},
		{sub, "CDPATH=.. cd b && git checkout x", true},
		// Plain forms are followed, and an absolute cd is known again.
		{root, "cd sub && git checkout main", false},
		{root, "cd sub && go test ./...", false},
		{root, "cd -L sub && git checkout main", false},
		{root, "command cd sub && git checkout main", false},
		{root, "builtin cd sub; git checkout x", false},
		{root, "pushd sub && git checkout x", false},
		{sub, "cd -; cd " + filepath.Join(root, "a") + " && git checkout x", false},
		{root, "cd sub/a/.. && git checkout x", false},
		{root, "cd sub && echo hi > ../fresh", false},
	}
	for _, test := range tests {
		start := "root"
		if test.dir == sub {
			start = "sub"
		}
		t.Run(start+": "+test.command, func(t *testing.T) {
			checkGuarded(t, test.dir, test.command, test.guarded)
		})
	}
}

// TestDestructiveGuardShellEnvironment covers the inherited environment
// that changes what cd means before the command runs.
func TestDestructiveGuardShellEnvironment(t *testing.T) {
	root := scopeFixture(t)
	sub := filepath.Join(root, "sub")
	tests := []struct {
		name, value, dir, command string
	}{
		{"CDPATH", root, sub, "cd b && git checkout x"},
		{"BASH_ENV", os.DevNull, sub, "git checkout x"},
		{"SHELLOPTS", "braceexpand:physical", root, "cd a/link/.. && git checkout x"},
		{"BASH_FUNC_cd%%", "() { :; }", root, "cd a && git checkout x"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearShellEnvironment(t)
			t.Setenv(test.name, test.value)
			checkGuarded(t, test.dir, test.command, true)
		})
	}
}

// TestDestructiveGuardBoundsWork covers inputs too large to classify within
// the walker's bounds: they ask at once instead of spending unbounded time.
func TestDestructiveGuardBoundsWork(t *testing.T) {
	clearShellEnvironment(t)
	root := scopeFixture(t)
	var redirects strings.Builder
	redirects.WriteString("if true; then cd a; fi")
	for i := range 2000 {
		fmt.Fprintf(&redirects, "; echo hi > fresh%d", i)
	}
	tests := []struct {
		name, command string
		guarded       bool
	}{
		{"here-document past the size limit", "cat > fresh.txt <<'EOF'\n" + strings.Repeat("plain text line\n", 600000) + "EOF\n", true},
		{"nesting past the depth limit", strings.Repeat("(", 100) + "true" + strings.Repeat(")", 100), true},
		{"candidate copies past the work limit", redirects.String(), true},
		{"here-document within the limits", "cat > fresh.txt <<'EOF'\n" + strings.Repeat("plain text line\n", 6000) + "EOF\n", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isDestructiveCommand(test.command, root); got != test.guarded {
				t.Errorf("isDestructiveCommand = %v, want %v", got, test.guarded)
			}
		})
	}
}
