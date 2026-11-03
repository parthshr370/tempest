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
		// A cd reaches later statements only where the shell keeps it: out
		// has no keep.txt, nested and the starting directory have one.
		{"(cd out); git checkout keep.txt", true},
		{"(cd out) && git checkout keep.txt", true},
		{"cd nested && (cd ../out); git checkout keep.txt", true},
		{"false && cd out; git checkout keep.txt", true},
		{"true || cd out; git checkout keep.txt", true},
		{"cd out | cat; git checkout keep.txt", true},
		{"cd out & git checkout keep.txt", true},
		{"echo $(cd out); git checkout keep.txt", true},
		{"if true; then cd out; fi; git checkout keep.txt", true},
		{"if true; then cd out; else git checkout keep.txt; fi", true},
		{"case z in y) cd out;; esac; git checkout keep.txt", true},
		{"cd out; { cd ../nested; }; git checkout keep.txt", true},
		{"cd out; for i in 1 2; do git checkout keep.txt; cd ..; done", true},
		{"cd out; while true; do git checkout keep.txt; cd ..; done", true},
		{"cd out; f() { cd ..; }; f; git checkout keep.txt", true},
		{"cd out; cd; git checkout keep.txt", true},
		{"cd out; pushd ../nested; git checkout keep.txt", true},
		{"cd out; export GIT_WORK_TREE=../nested; (export GIT_WORK_TREE=.); git checkout keep.txt", true},
		{"cat <<EOF\ncd out\nEOF\ngit checkout keep.txt", true},
		{"(cd out); echo hi > keep.txt", true},
		{"false && cd out; echo hi > keep.txt", true},
		{"cd out && git checkout keep.txt", false},
		{"cd out; git checkout keep.txt", false},
		{"cd out &&\ngit checkout keep.txt", false},
		{"cd out && git checkout main", false},
		{"cd nested && git status", false},
		{"cd out; (cd ../nested); git checkout keep.txt", false},
		{"(cd nested && git status) && cd out && git checkout keep.txt", false},
		{"if true; then cd out && git checkout keep.txt; fi", false},
		{"cd out; for i in 1 2; do git checkout keep.txt; done", false},
		{"for d in out nested; do (cd $d && ls); done; git checkout main", false},
		{"cd out; f() { git status; }; f; git checkout keep.txt", true},
		{"cd out; echo $(cd ..) > keep.txt", false},
		{"false && cd out; echo hi > " + filepath.Join(cwd, "new.txt"), false},
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

// TestGitGuardResolvesOperandsInTheSelectedWorkTree covers a checkout whose
// one operand names a file only in the work tree --work-tree or
// GIT_WORK_TREE selects, not in the directory git starts from.
func TestGitGuardResolvesOperandsInTheSelectedWorkTree(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "dir", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"x", filepath.Join("sub", "y")} {
		if err := os.WriteFile(filepath.Join(cwd, "dir", name), []byte("dirty"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		command string
		guarded bool
	}{
		{"git --git-dir=dir/.git --work-tree=dir checkout x", true},
		{"git --git-dir dir/.git --work-tree dir checkout x", true},
		{"GIT_DIR=dir/.git GIT_WORK_TREE=dir git checkout x", true},
		{"git --work-tree=" + filepath.Join(cwd, "dir") + " checkout x", true},
		{"git --work-tree=dir checkout sub/y", true},
		// A relative work tree resolves after -C.
		{"git --work-tree=. -C dir checkout x", true},
		{"GIT_WORK_TREE=. git -C dir checkout x", true},
		// Inside the work tree git reads operands from the current directory.
		{"git -C dir/sub --work-tree=.. checkout y", true},
		{"env GIT_WORK_TREE=dir git checkout x", true},
		{"GIT_WORK_TREE=dir env git checkout x", true},
		{"GIT_WORK_TREE=dir command git checkout x", true},
		{"env -u HOME GIT_WORK_TREE=dir git checkout x", true},
		{"env -C dir git checkout x", true},
		{"env --chdir=dir git checkout x", true},
		{"export GIT_WORK_TREE=dir; git checkout x", true},
		{"export GIT_DIR=dir/.git GIT_WORK_TREE=dir && git checkout x", true},
		{"GIT_WORK_TREE=dir; git checkout x", true},
		// An expanded work tree is unknown, so any one-operand checkout asks.
		{`git --work-tree="$TREE" checkout main`, true},
		{"git --work-tree $TREE checkout main", true},
		{"GIT_WORK_TREE=$TREE git checkout main", true},
		{"export GIT_WORK_TREE=$TREE; git checkout main", true},
		{"git --work-tree=dir checkout main", false},
		{"GIT_WORK_TREE=dir git checkout main", false},
		{"export GIT_WORK_TREE=dir; git checkout main", false},
		// Assignments before a command reach that command only.
		{"GIT_WORK_TREE=dir true; git checkout x", false},
		// GIT_DIR alone makes the current directory the work tree's top.
		{"git --git-dir=dir/.git checkout x", false},
		{"GIT_DIR=dir/.git git checkout x", false},
	}
	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			out := GateInDirectory(t.Context(), ModeDefault, "bash", commandArgs(test.command), &Policy{}, cwd)
			if out.Allow == test.guarded || test.guarded && out.Source != SourceGuard {
				t.Errorf("outcome=%+v; guarded=%v", out, test.guarded)
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
