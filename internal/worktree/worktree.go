// Package worktree owns marked, detached worktrees and complete patch artifacts.
package worktree

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.harness.dev/harness/internal/work"
)

const MaxPatchBytes int64 = 32 << 20

var ErrLimit = errors.New("artifact output limit")

type marker struct {
	Workspace work.Workspace
	Digest    string
	JobID     string
}

// PrivateDir refuses symlinks below the OS-owned temporary-directory aliases.
func PrivateDir(path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(path, current), string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err = os.Mkdir(current, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		// macOS exposes its private temp directory through these root-owned aliases.
		if info.Mode()&os.ModeSymlink != 0 && (current == "/var" || current == "/tmp") {
			info, err = os.Stat(current)
			if err != nil {
				return err
			}
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe directory %s", current)
		}
	}
	return nil
}

func command(ctx context.Context, cwd string, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_DIR=", "GIT_WORK_TREE=", "GIT_COMMON_DIR=", "GIT_INDEX_FILE=")
	// Empty GIT_DIR isn't the same as unset, so remove inherited git routing.
	filtered := cmd.Env[:0]
	for _, variable := range cmd.Env {
		if strings.HasPrefix(variable, "GIT_DIR=") || strings.HasPrefix(variable, "GIT_WORK_TREE=") || strings.HasPrefix(variable, "GIT_COMMON_DIR=") || strings.HasPrefix(variable, "GIT_INDEX_FILE=") {
			continue
		}
		filtered = append(filtered, variable)
	}
	cmd.Env = append(filtered, env...)
	cmd.WaitDelay = 2 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return cmd
}
func git(ctx context.Context, cwd string, env []string, args ...string) (string, error) {
	var out bytes.Buffer
	var diagnostic bytes.Buffer
	cmd := command(ctx, cwd, env, args...)
	cmd.Stdout = &boundedWriter{w: &out, left: 1 << 20}
	cmd.Stderr = &boundedWriter{w: &diagnostic, left: 4096}
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, diagnostic.String())
	}
	return strings.TrimSpace(out.String()), nil
}

type boundedWriter struct {
	w    io.Writer
	left int64
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.left {
		return 0, ErrLimit
	}
	n, e := w.w.Write(p)
	w.left -= int64(n)
	return n, e
}

// Plan pins the parent's current nonignored file state without changing its index.
// A nil workspace means the cwd requires shared isolation.
func Plan(ctx context.Context, cwd, journal, jobID, digest string) (*work.Workspace, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := exec.LookPath("git"); err != nil {
		return nil, nil
	}
	root, err := git(ctx, cwd, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	head, err := git(ctx, cwd, nil, "rev-parse", "--verify", "HEAD")
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil
	}
	bare, err := git(ctx, cwd, nil, "rev-parse", "--is-bare-repository")
	if err != nil || bare != "false" {
		return nil, nil
	}
	sparse, _ := git(ctx, cwd, nil, "config", "--bool", "core.sparseCheckout")
	if sparse == "true" {
		return nil, nil
	}
	sub, _ := git(ctx, cwd, nil, "rev-parse", "--show-superproject-working-tree")
	if sub != "" {
		return nil, nil
	}
	// Any gitlink is a submodule, even when its worktree is not initialized.
	hasSubmodules, err := hasGitlinks(ctx, root)
	if err != nil {
		return nil, err
	}
	if hasSubmodules {
		return nil, nil
	}
	realCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, realCwd)
	if err != nil {
		return nil, err
	}
	journalDir, err := filepath.EvalSymlinks(filepath.Dir(journal))
	if err != nil {
		return nil, err
	}
	target := filepath.Join(journalDir, filepath.Base(journal)+".d", "worktrees", jobID, "tree")
	inside, err := filepath.Rel(root, target)
	if err != nil {
		return nil, err
	}
	if inside != ".." && !strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return nil, errors.New("worktree sidecar must be outside parent repository")
	}
	parent := filepath.Dir(target)
	if err = PrivateDir(parent); err != nil {
		return nil, err
	}
	common, err := git(ctx, root, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil {
		return nil, err
	}
	dirty, err := hasStatus(ctx, root, false)
	if err != nil {
		return nil, err
	}
	if dirty {
		index, err := privateIndex(parent)
		if err != nil {
			return nil, err
		}
		defer os.Remove(index)
		env := []string{"GIT_INDEX_FILE=" + index}
		if _, err = git(ctx, root, env, "read-tree", "HEAD"); err != nil {
			return nil, err
		}
		if _, err = git(ctx, root, env, "add", "-A", "--", "."); err != nil {
			return nil, err
		}
		tree, err := git(ctx, root, env, "write-tree")
		if err != nil {
			return nil, err
		}
		identity := append(env, "GIT_AUTHOR_NAME=Tempest", "GIT_AUTHOR_EMAIL=tempest@localhost", "GIT_COMMITTER_NAME=Tempest", "GIT_COMMITTER_EMAIL=tempest@localhost")
		head, err = git(ctx, root, identity, "commit-tree", tree, "-p", head, "-m", "tempest-snapshot")
		if err != nil {
			return nil, err
		}
		if _, err = git(ctx, root, nil, "update-ref", "refs/tempest/jobs/"+jobID, head, strings.Repeat("0", len(head))); err != nil {
			return nil, err
		}
	}
	return &work.Workspace{Repo: root, CommonDir: common, BaseCommit: head, RelativeCwd: rel, Path: target, Marker: filepath.Join(parent, "owner.json")}, nil
}
func privateIndex(dir string) (string, error) {
	f, err := os.CreateTemp(dir, "index-*")
	if err != nil {
		return "", err
	}
	p := f.Name()
	if err = f.Close(); err != nil {
		return "", err
	}
	if err = os.Remove(p); err != nil {
		return "", err
	}
	return p, nil
}

func checkMarker(ws work.Workspace, jobID, digest string) error {
	f, err := os.OpenFile(ws.Marker, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	var m marker
	if err = json.NewDecoder(io.LimitReader(f, 16385)).Decode(&m); err != nil {
		return err
	}
	if m != (marker{ws, digest, jobID}) {
		return errors.New("worktree ownership marker mismatch")
	}
	return nil
}

// Ensure only adopts the exact marked path and pinned base.
func Ensure(ctx context.Context, ws work.Workspace, jobID, digest string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := PrivateDir(filepath.Dir(ws.Path)); err != nil {
		return err
	}
	if err := checkMarker(ws, jobID, digest); errors.Is(err, os.ErrNotExist) {
		if _, e := os.Lstat(ws.Path); !errors.Is(e, os.ErrNotExist) {
			return errors.New("foreign worktree path exists")
		}
		data, _ := json.Marshal(marker{ws, digest, jobID})
		if _, err = WriteArtifact(ws.Marker, bytes.NewReader(data), 16384); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := os.Lstat(ws.Path); err == nil {
		info, e := os.Lstat(ws.Path)
		if e != nil {
			return e
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe worktree path")
		}
		common, e := git(ctx, ws.Path, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if e == nil {
			common, e = filepath.EvalSymlinks(common)
		}
		head, he := git(ctx, ws.Path, nil, "rev-parse", "HEAD")
		registered, registrationErr := isRegistered(ctx, ws)
		if e == nil && he == nil && common == ws.CommonDir && registrationErr == nil && registered {
			base, e := git(ctx, ws.Path, nil, "merge-base", ws.BaseCommit, head)
			if e == nil && base == ws.BaseCommit {
				return checkCwd(ws)
			}
		}
		entries, e := os.ReadDir(ws.Path)
		if e != nil {
			return e
		}
		if len(entries) != 0 {
			return errors.New("mismatched nonempty worktree")
		}
		if e = os.Remove(ws.Path); e != nil {
			return e
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	registered, err := isRegistered(ctx, ws)
	if err != nil {
		return err
	}
	if registered {
		// Only our marker authorizes repairing this exact, absent registration.
		if _, err = git(ctx, ws.Repo, nil, "worktree", "remove", "--force", ws.Path); err != nil {
			return err
		}
	}
	if _, err := git(ctx, ws.Repo, nil, "worktree", "add", "--detach", ws.Path, ws.BaseCommit); err != nil {
		return err
	}
	return checkCwd(ws)
}
func checkCwd(ws work.Workspace) error {
	info, err := os.Stat(filepath.Join(ws.Path, ws.RelativeCwd))
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("child cwd is not a directory")
	}
	return nil
}

func isRegistered(ctx context.Context, ws work.Workspace) (bool, error) {
	listing, err := git(ctx, ws.Repo, nil, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, err
	}
	for _, field := range strings.Split(listing, "\x00") {
		if field == "worktree "+ws.Path {
			return true, nil
		}
	}
	return false, nil
}

// Export includes commits and all nonignored working files, without staging the child's index.
func Export(ctx context.Context, ws work.Workspace, path string) (work.Artifact, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conflicts, err := git(ctx, ws.Path, nil, "ls-files", "--unmerged")
	if err != nil {
		return work.Artifact{}, err
	}
	if conflicts != "" {
		return work.Artifact{}, errors.New("unresolved merge conflicts")
	}
	index, err := privateIndex(filepath.Dir(ws.Path))
	if err != nil {
		return work.Artifact{}, err
	}
	defer os.Remove(index)
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err = git(ctx, ws.Path, env, "read-tree", ws.BaseCommit); err != nil {
		return work.Artifact{}, err
	}
	if _, err = git(ctx, ws.Path, env, "add", "-A", "--", "."); err != nil {
		return work.Artifact{}, err
	}
	cmd := command(ctx, ws.Path, env, "diff", "--cached", "--binary", "--full-index", ws.BaseCommit, "--")
	return writeArtifactCommand(path, cmd, MaxPatchBytes)
}
func writeArtifactCommand(path string, cmd *exec.Cmd, limit int64) (work.Artifact, error) {
	return writeArtifact(path, limit, func(w io.Writer) error {
		cmd.Stdout = w
		var stderr bytes.Buffer
		cmd.Stderr = &boundedWriter{w: &stderr, left: 4096}
		err := cmd.Run()
		if err != nil {
			return fmt.Errorf("export patch: %w: %s", err, stderr.String())
		}
		return nil
	})
}

// WriteArtifact publishes only complete, synced files; a failed write never replaces evidence.
func WriteArtifact(path string, r io.Reader, limit int64) (work.Artifact, error) {
	return writeArtifact(path, limit, func(w io.Writer) error { _, err := io.Copy(w, r); return err })
}
func writeArtifact(path string, limit int64, produce func(io.Writer) error) (work.Artifact, error) {
	if err := PrivateDir(filepath.Dir(path)); err != nil {
		return work.Artifact{}, err
	}
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return work.Artifact{}, errors.New("unsafe artifact path")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return work.Artifact{}, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".artifact-*")
	if err != nil {
		return work.Artifact{}, err
	}
	defer os.Remove(f.Name())
	hash := sha256.New()
	w := &boundedWriter{w: io.MultiWriter(f, hash), left: limit}
	err = produce(w)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return work.Artifact{}, err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return work.Artifact{}, err
	}
	if err = syncDir(filepath.Dir(path)); err != nil {
		return work.Artifact{}, err
	}
	return work.Artifact{Path: path, SHA256: hex.EncodeToString(hash.Sum(nil)), Bytes: limit - w.left}, nil
}
func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func Verify(a work.Artifact) error {
	f, e := os.OpenFile(a.Path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(f, a.Bytes+1))
	if e != nil {
		return e
	}
	if n != a.Bytes || hex.EncodeToString(h.Sum(nil)) != a.SHA256 {
		return errors.New("artifact hash/length mismatch")
	}
	return nil
}
func Pristine(ctx context.Context, ws work.Workspace) (bool, error) {
	dirty, err := hasStatus(ctx, ws.Path, true)
	return !dirty, err
}

// Remove refuses unique ignored files and requires a complete patch for dirty trees.
func Remove(ctx context.Context, ws work.Workspace, jobID, digest string, patch *work.Artifact, automatic bool) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := checkMarker(ws, jobID, digest); err != nil {
		return err
	}
	if _, err := os.Lstat(ws.Path); errors.Is(err, os.ErrNotExist) {
		registered, registrationErr := isRegistered(ctx, ws)
		if registrationErr != nil {
			return registrationErr
		}
		if registered {
			if _, removeErr := git(ctx, ws.Repo, nil, "worktree", "remove", "--force", ws.Path); removeErr != nil {
				return removeErr
			}
		}
		return DropPin(ctx, ws, jobID)
	} else if err != nil {
		return err
	}
	common, err := git(ctx, ws.Path, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil || common != ws.CommonDir {
		return errors.New("worktree registration mismatch")
	}
	registered, err := isRegistered(ctx, ws)
	if err != nil {
		return err
	}
	if !registered {
		return errors.New("worktree isn't registered at its marked path")
	}
	ignored, err := git(ctx, ws.Path, nil, "ls-files", "--others", "--ignored", "--exclude-standard")
	if err != nil {
		return err
	}
	if ignored != "" {
		return fmt.Errorf("ignored outputs prevent cleanup: %.512s", ignored)
	}
	clean, err := Pristine(ctx, ws)
	if err != nil {
		return err
	}
	if !clean && automatic {
		return errors.New("worktree retained: contains changes")
	}
	head, err := git(ctx, ws.Path, nil, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if !clean || head != ws.BaseCommit || patch != nil {
		if patch == nil {
			return errors.New("missing durable patch")
		}
		if err = Verify(*patch); err != nil {
			return err
		}
		fresh, err := Export(ctx, ws, filepath.Join(filepath.Dir(ws.Path), "cleanup-check.patch"))
		if err != nil {
			return err
		}
		defer os.Remove(fresh.Path)
		if fresh.SHA256 != patch.SHA256 || fresh.Bytes != patch.Bytes {
			return errors.New("worktree changed after patch export")
		}
	}
	if _, err = git(ctx, ws.Repo, nil, "worktree", "remove", "--force", ws.Path); err != nil {
		return err
	}
	return DropPin(ctx, ws, jobID)
}
func DropPin(ctx context.Context, ws work.Workspace, jobID string) error {
	ref := "refs/tempest/jobs/" + jobID
	value, err := git(ctx, ws.Repo, nil, "rev-parse", "--verify", ref)
	if err != nil {
		return nil
	}
	if value != ws.BaseCommit {
		return errors.New("snapshot pin mismatch")
	}
	_, err = git(ctx, ws.Repo, nil, "update-ref", "-d", ref, ws.BaseCommit)
	return err
}
