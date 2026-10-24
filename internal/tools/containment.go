// Containment keeps every file tool inside the working directory. The OS
// refuses traversal through os.Root, so escaping paths fail structurally
// instead of by string comparison. The one exception is read-only: read may
// open the session's own progress checkpoint, job artifacts and spilled tool
// output, which live in the session data directory outside the workspace.
package tools

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.harness.dev/harness/internal/editbatch"
	"go.harness.dev/harness/internal/pathutil"
	"go.harness.dev/harness/internal/progress"
	"go.harness.dev/harness/internal/work"
)

// ErrOutsideRoot reports a path that resolves outside the tool's working
// directory. File tools return it (wrapped) for absolute paths outside cwd,
// for ../-style escapes, and for symlinks inside cwd that point outside. The
// sentinel lets callers classify containment failures without matching on
// error text.
var ErrOutsideRoot = errors.New("path resolves outside the working directory")

// openRoot opens the working directory once per tool and reuses the handle,
// so every file operation below goes through the same kernel-enforced root.
// os.Root is safe for concurrent use.
func openRoot(cwd string) (*os.Root, error) {
	return os.OpenRoot(cwd)
}

// rootOnce turns openRoot into a per-tool lazy singleton.
func rootOnce(cwd string) func() (*os.Root, error) {
	var once sync.Once
	var root *os.Root
	var err error
	return func() (*os.Root, error) {
		once.Do(func() { root, err = openRoot(cwd) })
		return root, err
	}
}

// resolveRootPath resolves a model-supplied path the same way the tools did
// before containment (tilde expansion, unicode normalization), then converts
// it to a root-relative name for the os.Root methods. Absolute paths inside
// cwd are rewritten to their relative form. A path that escapes cwd fails
// with ErrOutsideRoot before any IO happens.
func resolveRootPath(cwd string, requested string) (string, error) {
	absolute := pathutil.ResolveToCwd(requested, cwd)
	return rootRelPath(cwd, absolute)
}

// rootRelPath converts an already-absolute path into a root-relative name,
// refusing escapes with ErrOutsideRoot. Symlinks are resolved so a link
// inside cwd pointing outside is refused too; a missing final component
// (write creating a new file) falls back to evaluating its parent.
func rootRelPath(cwd string, absolute string) (string, error) {
	relative, _, err := rootRelPaths(cwd, absolute)
	return relative, err
}

// rootRelPaths is rootRelPath that also returns the canonical name: the
// symlink-resolved path relative to the symlink-resolved cwd. The canonical
// name identifies the file a write actually changes, which is how a
// language server workspace keys its documents.
func rootRelPaths(cwd string, absolute string) (relative string, canonical string, err error) {
	// macOS temp dirs live behind a symlinked prefix (/var -> /private/var),
	// so symlink evaluation must be anchored at an equally resolved cwd.
	relative, err = relInside(cwd, absolute)
	if err != nil {
		return "", "", err
	}
	evaluatedCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		evaluatedCwd = cwd
	}
	evaluated, evalErr := filepath.EvalSymlinks(absolute)
	if evalErr != nil {
		// The final components may not exist yet (write creating a new
		// file). Resolve the deepest existing ancestor and rejoin the tail.
		candidate := filepath.Clean(filepath.Dir(absolute))
		tail := filepath.Base(absolute)
		for {
			if ev, err := filepath.EvalSymlinks(candidate); err == nil {
				evaluated = filepath.Join(ev, tail)
				break
			}
			parent := filepath.Dir(candidate)
			if parent == candidate {
				evaluated = absolute
				break
			}
			tail = filepath.Join(filepath.Base(candidate), tail)
			candidate = parent
		}
	}
	canonical, err = relInside(evaluatedCwd, evaluated)
	if err != nil {
		return "", "", fmt.Errorf("%w: %s escapes the working directory through a symlink", ErrOutsideRoot, absolute)
	}
	return filepath.ToSlash(relative), filepath.ToSlash(canonical), nil
}

// relInside reports the path of absolute relative to cwd, or ErrOutsideRoot
// when the path does not sit inside it. This is a pre-flight check only; the
// os.Root methods remain the structural guarantee.
func relInside(cwd string, absolute string) (string, error) {
	relative, err := filepath.Rel(cwd, absolute)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoot, absolute)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoot, absolute)
	}
	return relative, nil
}

// readFileInRoot reads a file through the root so the kernel, not a string
// check, refuses any escape that slips past the pre-flight path resolution.
func readFileInRoot(root *os.Root, name string) ([]byte, error) {
	data, err := root.ReadFile(name)
	if err != nil {
		return nil, wrapOutsideRoot(name, err)
	}
	return data, nil
}

func readFileInRootLimit(root *os.Root, name string, limit int64) ([]byte, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, wrapOutsideRoot(name, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%s exceeds the %d MiB read limit", name, limit>>20)
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("%s exceeds the %d MiB read limit", name, limit>>20)
	}
	return content, nil
}

// ToolOutputDirName is the directory inside a session data directory that
// holds spilled tool output: truncated bash output, tool results cleared from
// context, and oversized MCP results.
const ToolOutputDirName = "tool-output"

// ToolOutputDir returns the spill directory of sessionDataDir, or "" when the
// session has no data directory.
func ToolOutputDir(sessionDataDir string) string {
	if sessionDataDir == "" {
		return ""
	}
	return filepath.Join(sessionDataDir, ToolOutputDirName)
}

// sessionArtifactName returns the name of absolute inside sessionDataDir when
// it is the session's progress checkpoint, a file of one of its jobs
// (jobs/<job id>/<file>) or spilled tool output (tool-output/...). Child
// journals, worktrees and compaction snapshots in the same directory stay
// refused. The check runs on the symlink-resolved path, so a link cannot
// dress another file up as an artifact, and the read itself goes through an
// os.Root on the data directory.
func sessionArtifactName(sessionDataDir, absolute string) (string, bool) {
	if sessionDataDir == "" {
		return "", false
	}
	evaluatedDir, err := filepath.EvalSymlinks(sessionDataDir)
	if err != nil {
		return "", false
	}
	evaluated, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", false
	}
	name, err := relInside(evaluatedDir, evaluated)
	if err != nil {
		return "", false
	}
	name = filepath.ToSlash(name)
	parts := strings.Split(name, "/")
	isJobArtifact := len(parts) == 3 && parts[0] == work.JobArtifactsDir
	isToolOutput := len(parts) >= 2 && parts[0] == ToolOutputDirName
	if name != progress.FileName && !isJobArtifact && !isToolOutput {
		return "", false
	}
	return name, true
}

// readSessionArtifact reads a name approved by sessionArtifactName through a
// root on the session data directory.
func readSessionArtifact(sessionDataDir, name string) ([]byte, error) {
	root, err := os.OpenRoot(sessionDataDir)
	if err != nil {
		return nil, err
	}
	data, err := readFileInRoot(root, name)
	return data, errors.Join(err, root.Close())
}

// writeFileInRoot replaces a file through the root atomically (staged file,
// sync, rename; see editbatch.WriteFile), creating parent directories inside
// the root as needed and keeping an existing file's permission bits. name is
// the symlink-resolved path, so the rename replaces the real file.
func writeFileInRoot(root *os.Root, name string, content []byte) error {
	return wrapOutsideRoot(name, editbatch.WriteFile(root, name, content, 0))
}

// statInRoot stats a path through the root.
func statInRoot(root *os.Root, name string) (fs.FileInfo, error) {
	info, err := root.Stat(name)
	if err != nil {
		return nil, wrapOutsideRoot(name, err)
	}
	return info, nil
}

// wrapOutsideRoot marks os.Root refusals with the shared sentinel so callers
// see one containment error shape even when the OS is what said no.
func wrapOutsideRoot(name string, err error) error {
	if errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("%w: %s", ErrOutsideRoot, name)
	}
	return err
}
