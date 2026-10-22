package worktree

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.test", "GIT_CONFIG_NOSYSTEM=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
func newRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitTest(t, repo, "init")
	writeTest(t, filepath.Join(repo, "tracked"), []byte("base\n"))
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "base")
	return repo
}
func writeTest(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
}
func indexHash(t *testing.T, repo string) string {
	t.Helper()
	index, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(index)
	return hex.EncodeToString(sum[:])
}
func planTest(t *testing.T, repo string) (context.Context, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	journal := filepath.Join(t.TempDir(), "parent.jsonl")
	return ctx, journal
}

func TestCleanAndDirtySnapshotLeaveParentUntouched(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		name := "clean"
		if dirty {
			name = "dirty"
		}
		t.Run(name, func(t *testing.T) {
			repo := newRepo(t)
			writeTest(t, filepath.Join(repo, "sub", "anchor"), []byte("sub\n"))
			gitTest(t, repo, "add", ".")
			gitTest(t, repo, "commit", "-m", "sub")
			if dirty {
				writeTest(t, filepath.Join(repo, "tracked"), []byte("staged\n"))
				gitTest(t, repo, "add", "tracked")
				writeTest(t, filepath.Join(repo, "tracked"), []byte("unstaged\n"))
				writeTest(t, filepath.Join(repo, "untracked"), []byte("untracked\n"))
			}
			beforeIndex := indexHash(t, repo)
			beforeStatus := gitTest(t, repo, "status", "--porcelain", "--untracked-files=all")
			beforeRefs := gitTest(t, repo, "show-ref", "--heads")
			head := gitTest(t, repo, "rev-parse", "HEAD")
			ctx, journal := planTest(t, repo)
			workspace, err := Plan(ctx, filepath.Join(repo, "sub"), journal, "job_test", "digest")
			if err != nil || workspace == nil {
				t.Fatal(workspace, err)
			}
			if dirty && workspace.BaseCommit == head || !dirty && workspace.BaseCommit != head {
				t.Fatal(workspace)
			}
			if err = Ensure(ctx, *workspace, "job_test", "digest"); err != nil {
				t.Fatal(err)
			}
			childContent, err := os.ReadFile(filepath.Join(workspace.Path, "tracked"))
			if err != nil {
				t.Fatal(err)
			}
			expected := "base\n"
			if dirty {
				expected = "unstaged\n"
			}
			if string(childContent) != expected {
				t.Fatal(string(childContent))
			}
			if dirty {
				content, err := os.ReadFile(filepath.Join(workspace.Path, "untracked"))
				if err != nil || string(content) != "untracked\n" {
					t.Fatal(string(content), err)
				}
			}
			if filepath.Join(workspace.Path, workspace.RelativeCwd) != filepath.Join(workspace.Path, "sub") {
				t.Fatal(workspace)
			}
			if indexHash(t, repo) != beforeIndex || gitTest(t, repo, "status", "--porcelain", "--untracked-files=all") != beforeStatus || gitTest(t, repo, "show-ref", "--heads") != beforeRefs {
				t.Fatal("parent index/files/refs changed")
			}
			if err = Remove(ctx, *workspace, "job_test", "digest", nil, true); err != nil {
				t.Fatal(err)
			}
			if _, err = os.Lstat(workspace.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("tree not removed", err)
			}
			cmd := exec.Command("git", "rev-parse", "--verify", "refs/tempest/jobs/job_test")
			cmd.Dir = repo
			if err = cmd.Run(); err == nil {
				t.Fatal("snapshot ref was not deleted")
			}
		})
	}
}
func TestPatchContainsCommittedUntrackedBinaryModeSymlinkDeletion(t *testing.T) {
	repo := newRepo(t)
	writeTest(t, filepath.Join(repo, "deleted"), []byte("delete\n"))
	writeTest(t, filepath.Join(repo, "binary"), []byte{0, 1, 2, 3})
	writeTest(t, filepath.Join(repo, "executable"), []byte("#!/bin/sh\n"))
	writeTest(t, filepath.Join(repo, ".gitignore"), []byte("ignored\n"))
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "files")
	beforeIndex := indexHash(t, repo)
	beforeHead := gitTest(t, repo, "rev-parse", "HEAD")
	ctx, journal := planTest(t, repo)
	workspace, err := Plan(ctx, repo, journal, "job_patch", "digest")
	if err != nil {
		t.Fatal(err)
	}
	if err = Ensure(ctx, *workspace, "job_patch", "digest"); err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(workspace.Path, "tracked"), []byte("committed\n"))
	gitTest(t, workspace.Path, "add", "tracked")
	gitTest(t, workspace.Path, "commit", "-m", "child")
	writeTest(t, filepath.Join(workspace.Path, "binary"), []byte{0, 9, 8, 7, 6})
	if err = os.Remove(filepath.Join(workspace.Path, "deleted")); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(filepath.Join(workspace.Path, "executable"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("tracked", filepath.Join(workspace.Path, "link")); err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(workspace.Path, "added"), []byte("new\n"))
	writeTest(t, filepath.Join(workspace.Path, "ignored"), []byte("unique\n"))
	childIndex := indexHashPath(t, gitTest(t, workspace.Path, "rev-parse", "--path-format=absolute", "--git-path", "index"))
	patch, err := Export(ctx, *workspace, filepath.Join(filepath.Dir(workspace.Path), "changes.patch"))
	if err != nil {
		t.Fatal(err)
	}
	if err = Verify(patch); err != nil {
		t.Fatal(err)
	}
	if indexHash(t, repo) != beforeIndex || gitTest(t, repo, "rev-parse", "HEAD") != beforeHead || gitTest(t, repo, "status", "--porcelain") != "" {
		t.Fatal("parent changed")
	}
	if indexHashPath(t, gitTest(t, workspace.Path, "rev-parse", "--path-format=absolute", "--git-path", "index")) != childIndex {
		t.Fatal("child index changed")
	}
	apply := t.TempDir()
	gitTest(t, apply, "clone", repo, ".")
	gitTest(t, apply, "apply", "--check", patch.Path)
	gitTest(t, apply, "apply", patch.Path)
	for _, file := range []string{"tracked", "binary", "added", "executable"} {
		want, err := os.ReadFile(filepath.Join(workspace.Path, file))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(apply, file))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal(file, err)
		}
	}
	if _, err = os.Stat(filepath.Join(apply, "deleted")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	link, err := os.Readlink(filepath.Join(apply, "link"))
	if err != nil || link != "tracked" {
		t.Fatal(link, err)
	}
	info, err := os.Stat(filepath.Join(apply, "executable"))
	if err != nil || info.Mode()&0100 == 0 {
		t.Fatal(info, err)
	}
	if err = Remove(ctx, *workspace, "job_patch", "digest", &patch, false); err == nil || !strings.Contains(err.Error(), "ignored") {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(workspace.Path, "ignored")); err != nil {
		t.Fatal(err)
	}
	if err = Remove(ctx, *workspace, "job_patch", "digest", &patch, true); err == nil {
		t.Fatal("automatic cleanup removed dirty tree")
	}
	if err = Remove(ctx, *workspace, "job_patch", "digest", &patch, false); err != nil {
		t.Fatal(err)
	}
	if err = Verify(patch); err != nil {
		t.Fatal("cleanup removed patch", err)
	}
}
func indexHashPath(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
func TestIneligibleAndForeignPaths(t *testing.T) {
	nongit := t.TempDir()
	unborn := t.TempDir()
	gitTest(t, unborn, "init")
	sparse := newRepo(t)
	gitTest(t, sparse, "config", "core.sparseCheckout", "true")
	for _, repo := range []string{nongit, unborn, sparse} {
		ctx, journal := planTest(t, repo)
		workspace, err := Plan(ctx, repo, journal, "job_shared", "digest")
		if err != nil || workspace != nil {
			t.Fatal(workspace, err)
		}
	}
	repo := newRepo(t)
	ctx, journal := planTest(t, repo)
	workspace, err := Plan(ctx, repo, journal, "job_foreign", "digest")
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(workspace.Path, "foreign"), []byte("do not remove"))
	if err = Ensure(ctx, *workspace, "job_foreign", "digest"); err == nil {
		t.Fatal("adopted foreign path")
	}
	if content, err := os.ReadFile(filepath.Join(workspace.Path, "foreign")); err != nil || string(content) != "do not remove" {
		t.Fatal("deleted foreign evidence", err)
	}
}
func TestArtifactLimitsAndSymlinks(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "result")
	artifact, err := WriteArtifact(path, strings.NewReader("complete"), 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = WriteArtifact(path, strings.NewReader("overflow!"), 8); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if err = Verify(artifact); err != nil {
		t.Fatal("failed export damaged prior evidence", err)
	}
	link := filepath.Join(root, "link")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err = WriteArtifact(link, strings.NewReader("replace"), 8); err == nil {
		t.Fatal("followed symlink")
	}
	if err = PrivateDir(filepath.Join(link, "child")); err == nil {
		t.Fatal("followed directory symlink")
	}
}
func TestWorktreeCancellationJoinsGit(t *testing.T) {
	repo := newRepo(t)
	ctx, journal := planTest(t, repo)
	workspace, err := Plan(ctx, repo, journal, "job_cancel", "digest")
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err = Ensure(cancelled, *workspace, "job_cancel", "digest"); err == nil {
		t.Fatal("cancelled git started")
	}
}

func TestOwnedRecoveryCleanupAndCorruptPatchGuard(t *testing.T) {
	repo := newRepo(t)
	ctx, journal := planTest(t, repo)
	workspace, err := Plan(ctx, repo, journal, "job_recovery", "digest")
	if err != nil {
		t.Fatal(err)
	}
	if err = Ensure(ctx, *workspace, "job_recovery", "digest"); err != nil {
		t.Fatal(err)
	}
	if err = Ensure(ctx, *workspace, "job_recovery", "foreign-digest"); err == nil {
		t.Fatal("adopted foreign marker")
	}
	writeTest(t, filepath.Join(workspace.Path, "tracked"), []byte("committed evidence\n"))
	gitTest(t, workspace.Path, "add", ".")
	gitTest(t, workspace.Path, "commit", "-m", "child commit")
	if err = Ensure(ctx, *workspace, "job_recovery", "digest"); err != nil {
		t.Fatal("couldn't reattach owned child commit", err)
	}
	patch, err := Export(ctx, *workspace, filepath.Join(filepath.Dir(workspace.Path), "changes.patch"))
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(patch.Path)
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, patch.Path, []byte("corrupt"))
	if err = Remove(ctx, *workspace, "job_recovery", "digest", &patch, true); err == nil {
		t.Fatal("removed unique committed evidence with bad patch")
	}
	writeTest(t, patch.Path, original)
	if err = Remove(ctx, *workspace, "job_recovery", "digest", &patch, true); err != nil {
		t.Fatal(err)
	}
	if err = Remove(ctx, *workspace, "job_recovery", "digest", &patch, true); err != nil {
		t.Fatal("absent exact path wasn't idempotent", err)
	}
}

func TestDirtySnapshotPatchIsRelativeToPinnedParentState(t *testing.T) {
	repo := newRepo(t)
	writeTest(t, filepath.Join(repo, "tracked"), []byte("parent dirty\n"))
	writeTest(t, filepath.Join(repo, "parent-untracked"), []byte("parent only\n"))
	ctx, journal := planTest(t, repo)
	workspace, err := Plan(ctx, repo, journal, "job_snapshot", "digest")
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(repo, "tracked"), []byte("parent changed after admission\n"))
	if err = Ensure(ctx, *workspace, "job_snapshot", "digest"); err != nil {
		t.Fatal(err)
	}
	pinned, err := os.ReadFile(filepath.Join(workspace.Path, "tracked"))
	if err != nil || string(pinned) != "parent dirty\n" {
		t.Fatal(string(pinned), err)
	}
	writeTest(t, filepath.Join(workspace.Path, "tracked"), []byte("child delta\n"))
	patch, err := Export(ctx, *workspace, filepath.Join(filepath.Dir(workspace.Path), "changes.patch"))
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(patch.Path)
	if err != nil || !bytes.Contains(content, []byte("-parent dirty")) || bytes.Contains(content, []byte("parent-untracked")) {
		t.Fatal(string(content), err)
	}
	if err = Remove(ctx, *workspace, "job_snapshot", "digest", &patch, false); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(filepath.Join(repo, "tracked"))
	if err != nil || string(current) != "parent changed after admission\n" {
		t.Fatal("parent overwritten", err)
	}
}

func TestInsideRepoLayoutRefusedBeforeWritingParent(t *testing.T) {
	repo := newRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	journal := filepath.Join(repo, "parent.jsonl")
	if _, err := Plan(ctx, repo, journal, "job_inside", "digest"); err == nil {
		t.Fatal("accepted session sidecar inside repo")
	}
	if _, err := os.Lstat(journal + ".d"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected layout changed parent", err)
	}
}

func TestMissingGitBareAndSubmoduleUseSharedIsolation(t *testing.T) {
	repo := newRepo(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	gitTest(t, repo, "init", "--bare", bare)
	submodule := newRepo(t)
	gitTest(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", submodule, "sub")
	gitTest(t, repo, "commit", "-m", "submodule")
	for _, cwd := range []string{repo, filepath.Join(repo, "sub"), bare} {
		ctx, journal := planTest(t, cwd)
		workspace, err := Plan(ctx, cwd, journal, "job_shared", "digest")
		if err != nil || workspace != nil {
			t.Fatal(workspace, err)
		}
	}
	t.Setenv("PATH", t.TempDir())
	ctx, journal := planTest(t, repo)
	if workspace, err := Plan(ctx, repo, journal, "job_nogit", "digest"); err != nil || workspace != nil {
		t.Fatal(workspace, err)
	}
}

func TestGitCancellationJoinsStartedProcessAndReaders(t *testing.T) {
	repo := newRepo(t)
	wrapperDir := t.TempDir()
	barrier := filepath.Join(wrapperDir, "started")
	script := "#!/bin/sh\necho ready > \"" + barrier + "\"\nexec /bin/sleep 300\n"
	if err := os.WriteFile(filepath.Join(wrapperDir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapperDir)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	journal := filepath.Join(t.TempDir(), "parent.jsonl")
	go func() { _, err := Plan(ctx, repo, journal, "job_cancel_started", "digest"); finished <- err }()
	joined := false
	defer func() {
		cancel()
		if !joined {
			<-finished
		}
	}()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(barrier); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("git process never started")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case err := <-finished:
		joined = true
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("git command or output reader failed to join")
	}
}

func TestGitlinkDetectionAcrossReadBoundaries(t *testing.T) {
	index := "100644 " + strings.Repeat("a", 40) + " 0\tordinary\n160000 " + strings.Repeat("b", 40) + " 0\tsubmodule\n"
	for size := 1; size < len(index); size++ {
		writer := &gitlinkWriter{}
		for offset := 0; offset < len(index); offset += size {
			end := min(offset+size, len(index))
			if _, err := writer.Write([]byte(index[offset:end])); err != nil {
				t.Fatal(err)
			}
		}
		if !writer.found {
			t.Fatal("missed split gitlink mode", size)
		}
	}
	writer := &gitlinkWriter{}
	if _, err := writer.Write([]byte("100644 abc 0\t160000 name\n")); err != nil || writer.found {
		t.Fatal("filename mistaken for submodule", err)
	}
}
