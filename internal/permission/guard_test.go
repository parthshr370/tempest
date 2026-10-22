package permission

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDestructiveGuardPolicy(t *testing.T) {
	cwd := t.TempDir()
	for _, name := range []string{"keep.txt", "app.log", "out.txt", "with space.txt"} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(cwd, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cwd, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "nested", "keep.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		command string
		guarded bool
	}{
		{"echo hi > fresh.txt", false},
		{"echo hi >> app.log", false},
		{"echo hi >> fresh.txt", false},
		{"go version | tee v.txt", false},
		{"go version | tee -a app.log", false},
		{"go version | tee --append app.log", false},
		{"cp keep.txt out/", false},
		{"mv keep.txt fresh.txt", false},
		{"cp keep.txt fresh.txt", false},
		{"mkdir -p out && cp keep.txt out/", false},
		{"git stash", false},
		{"git stash push -m backup", false},
		{"git add keep.txt && git commit -m saved", false},
		{"git push origin main", false},
		{"git checkout feature", false},
		{"git branch -d merged", false},
		{"git branch --delete merged", false},
		{"git clean -n", false},
		{"git reset --soft HEAD~1", false},
		{"touch fresh.txt && chmod +x fresh.txt", false},
		{"echo rm cp mv tee", false},
		{"printf '%s' 'a > out.txt'", false},
		{"python3 -c \"print('hello')\"", false},
		{"python3 -c \"open('keep.txt', 'r').read()\"", false},
		{`python3 -c "open('app.log', 'a').write('hello')"`, false},
		{`python3 -c "open('fresh.txt', 'x').write('hello')"`, false},
		{"dd if=keep.txt", false},
		{"echo hi > /dev/null", false},
		{"echo hi 2>&1", false},
		{"find . -name '*.tmp' -delete", true},
		{"find . -name '*.tmp' -exec rm {} \\;", true},
		{"find . -exec /bin/rm -f {} +", true},
		{"find . -print | xargs rm", true},
		{"find . -print0 | xargs -0 -n 1 /bin/rm", true},
		{"sed -i 's/a/b/' keep.txt", true},
		{"sed -i '' 's/a/b/' keep.txt", true},
		{"sed --in-place=.bak 's/a/b/' keep.txt", true},
		{"perl -pi -e 's/a/b/' keep.txt", true},
		{"truncate -s 0 keep.txt", true},
		{"dd if=/dev/zero of=keep.txt", true},
		{"shred keep.txt", true},
		{"rm keep.txt", true},
		{"rmdir out", true},
		{"unlink keep.txt", true},
		{"echo hi > keep.txt", true},
		{"echo hi >| keep.txt", true},
		{"echo hi &>out.txt", true},
		{"echo hi > 'with space.txt'", true},
		{"echo hi > \"$OUTPUT\"", true},
		{"echo hi > $(pwd)/fresh.txt", true},
		{"echo hi > missing/../keep.txt", true},
		{"go version | tee keep.txt", true},
		{"go version | tee fresh.txt keep.txt", true},
		{"cp keep.txt out.txt", true},
		{"cp keep.txt out.txt 2>/dev/null", true},
		{"mv keep.txt out.txt", true},
		{"cp nested/keep.txt ./", true},
		{"cp -t . nested/keep.txt", true},
		{"cd nested && echo hi > keep.txt", true},
		{"cd nested && echo hi > ../fresh.txt", false},
		{"echo hi > fresh.txt; echo hi > fresh.txt", true},
		{"echo hi >> fresh.txt; echo hi > fresh.txt", true},
		{"cd missing; echo hi > keep.txt", true},
		{"git reset --hard", true},
		{"git clean -fd", true},
		{"git checkout -- keep.txt", true},
		{"git checkout -f feature", true},
		{"git restore keep.txt", true},
		{"git push --force origin main", true},
		{"git push --force-with-lease origin main", true},
		{"git push -f origin main", true},
		{"git push origin +main", true},
		{"git branch -D feature", true},
		{"git -C nested reset --hard", true},
		{"git stash drop", true},
		{"git stash clear", true},
		{`python3 -c "import shutil; shutil.rmtree('out')"`, true},
		{`python -c "import os; os.remove('keep.txt')"`, true},
		{`python3 -c "from pathlib import Path; Path('keep.txt').unlink()"`, true},
		{`python3 -c "import os; os.rmdir('out')"`, true},
		{`python3 -c "open('out.txt','w').write('built')"`, true},
		{`python3 -c "open('out.txt', mode='wb').write(b'built')"`, true},
		{`node -e "require('fs').rmSync('out', {recursive:true})"`, true},
		{`node -e "fs.rm('out', callback)"`, true},
		{`node -e "fs.unlinkSync('keep.txt')"`, true},
		{`perl -e 'unlink "keep.txt"'`, true},
		{`ruby -e 'File.unlink("keep.txt")'`, true},
		{`ruby -e 'File.delete("keep.txt")'`, true},
	}
	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			for _, mode := range []Mode{ModeDefault, ModeAcceptEdits} {
				out := GateInDirectory(context.Background(), mode, "bash", commandArgs(test.command), &Policy{}, cwd)
				if out.Allow == test.guarded || test.guarded && out.Source != SourceGuard {
					t.Errorf("mode=%s outcome=%+v; guarded=%v", mode, out, test.guarded)
				}
			}
		})
	}
}

func TestDestructiveGuardUnknownTarget(t *testing.T) {
	cwd := t.TempDir()
	if err := os.Symlink("loop", filepath.Join(cwd, "loop")); err != nil {
		t.Fatal(err)
	}
	out := GateInDirectory(t.Context(), ModeDefault, "bash", commandArgs("echo hi > loop/file"), &Policy{}, cwd)
	if out.Allow || out.Source != SourceGuard {
		t.Fatalf("unknown target outcome=%+v", out)
	}
}

func TestChildGuardUsesChildDirectory(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "keep.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	prompts := 0
	policy := &Policy{Prompter: choicePrompter(func(_ context.Context, req PromptRequest) (Choice, error) {
		prompts++
		if req.Cwd != cwd || req.Reason == "" {
			t.Errorf("request=%+v", req)
		}
		return RejectOnce, nil
	})}
	ceiling := CaptureCeiling(ModeDefault, []string{"bash"}, policy)
	out := GateChild(t.Context(), ceiling, ModeDefault, []string{"bash"}, policy, "bash", commandArgs("echo hi > keep.txt"), ChildScope{Cwd: cwd})
	if out.Allow || prompts != 1 {
		t.Fatalf("outcome=%+v prompts=%d", out, prompts)
	}
}

func TestPlanStillRejectsNonDestructiveWrites(t *testing.T) {
	for _, command := range []string{"echo hi > fresh.txt", "echo hi >> app.log", "echo hi | tee -a app.log", "find . -exec touch {} \\;", "git branch new", "git stash"} {
		if IsSafeCommand(command) {
			t.Errorf("plan considers %q read-only", command)
		}
	}
}

func TestBashGuardUsesWorkspaceNotUnrecognizedArgument(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "keep.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"command":"echo hi > keep.txt","cwd":"/not/the/workspace"}`)
	out := GateInDirectory(t.Context(), ModeDefault, "bash", args, &Policy{}, cwd)
	if out.Allow || out.Source != SourceGuard {
		t.Fatalf("outcome=%+v", out)
	}
}
