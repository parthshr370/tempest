// Containment keeps every file tool inside the working directory. The OS
// refuses traversal through os.Root, so escaping paths fail structurally
// instead of by string comparison.
package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.harness.dev/harness/internal/pathutil"
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
	// macOS temp dirs live behind a symlinked prefix (/var -> /private/var),
	// so symlink evaluation must be anchored at an equally resolved cwd.
	relative, err := relInside(cwd, absolute)
	if err != nil {
		return "", err
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
	if _, err := relInside(evaluatedCwd, evaluated); err != nil {
		return "", fmt.Errorf("%w: %s escapes the working directory through a symlink", ErrOutsideRoot, absolute)
	}
	return filepath.ToSlash(relative), nil
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

// writeFileInRoot writes a file through the root, creating parent directories
// inside the root as needed.
func writeFileInRoot(root *os.Root, name string, content []byte) error {
	if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return wrapOutsideRoot(name, err)
	}
	if err := root.WriteFile(name, content, 0o644); err != nil {
		return wrapOutsideRoot(name, err)
	}
	return nil
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
