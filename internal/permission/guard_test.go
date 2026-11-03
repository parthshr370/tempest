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
		{"git checkout -b nested", false},
		{"git checkout -b fix origin/main", false},
		{"git checkout -", false},
		{"git checkout HEAD~1", false},
		{"git switch main", false},
		{"git switch -c keep.txt", false},
		{"git restore --staged keep.txt", false},
		{"git rm --cached keep.txt", false},
		{"git rm -n keep.txt", false},
		{"git clean --dry-run -d", false},
		{"git status", false},
		{"git diff -- keep.txt", false},
		{"git log -p -- keep.txt", false},
		{"git show HEAD:keep.txt", false},
		{"git -C nested status", false},
		{"git -c color.ui=always log", false},
		{"git push origin HEAD:refs/heads/topic", false},
		{"git push -n --force origin main", false},
		{"git stash pop", false},
		{"git stash push -m drop", false},
		{"git worktree remove ../wt", false},
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
		{"git checkout .", true},
		{"git checkout keep.txt", true},
		{"git checkout 'with space.txt'", true},
		{"git checkout main keep.txt", true},
		{"git checkout HEAD~1 -- keep.txt", true},
		{"git checkout '*.txt'", true},
		{"git checkout ':(glob)*.txt'", true},
		{"git checkout \"$FILE\"", true},
		{"git checkout -p", true},
		{"git checkout --force main", true},
		{"git -C nested checkout keep.txt", true},
		{"git -c core.pager=cat checkout keep.txt", true},
		{"git --git-dir=.git --no-pager checkout .", true},
		{"GIT_DIR=.git git checkout .", true},
		{"git rm -q keep.txt", true},
		{"git rm -r nested", true},
		{"git switch -f main", true},
		{"git switch -f -C main", true},
		{"git switch -fC main", true},
		{"git switch --discard-changes main", true},
		{"git clean -d", true},
		{"git clean -xdf", true},
		{"git restore --staged --worktree keep.txt", true},
		{"git restore -SW keep.txt", true},
		{"git restore --source=HEAD~1 keep.txt", true},
		{"git reset --merge", true},
		{"git stash drop stash@{0}", true},
		{"git branch -d -f feature", true},
		{"git branch -M main", true},
		{"git push origin :feature", true},
		{"git push --delete origin feature", true},
		{"git push -d origin feature", true},
		{"git push --mirror backup", true},
		{"git worktree remove --force ../wt", true},
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
