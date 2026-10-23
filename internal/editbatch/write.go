package editbatch

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// defaultPerm is a new file's permission before the umask, matching
// os.WriteFile callers in the file tools.
const defaultPerm fs.FileMode = 0o644

// tempSuffixBytes sizes the random part of a staged file's name.
const tempSuffixBytes = 6

// maxTempAttempts bounds retries when a staged name is already taken.
const maxTempAttempts = 8

// WriteFile replaces name inside root with data atomically: the bytes are
// staged in a temporary file in the same directory, synced, renamed over the
// target and the directory synced. A crash leaves either the old file or the
// new one, never a truncated mix. perm 0 keeps an existing file's permission
// bits and creates a new file with 0o644 before the umask; a nonzero perm is
// the file's exact final permission. name must not be a symlink: callers pass
// the symlink-resolved path so the rename replaces the real file.
func WriteFile(root *os.Root, name string, data []byte, perm fs.FileMode) error {
	return writeFile(root, name, "", data, perm)
}

// writeFile is WriteFile staging through the given name, or a random one
// when staged is empty. A batch passes the name it recorded in its journal
// so recovery can remove a staged file a crash left behind.
func writeFile(root *os.Root, name, staged string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(name)
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	finalPerm := perm
	info, err := root.Lstat(name)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", name)
		}
		if finalPerm == 0 {
			finalPerm = info.Mode().Perm()
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	createPerm := defaultPerm
	if perm != 0 {
		createPerm = perm
	}
	staged, file, err := createStaged(root, dir, filepath.Base(name), staged, createPerm)
	if err != nil {
		return err
	}
	if err := publishStaged(root, file, staged, name, data, finalPerm); err != nil {
		return errors.Join(err, removeStaged(root, staged))
	}
	return syncDir(root, dir)
}

// publishStaged fills the staged file, makes it durable and renames it over
// name. The caller removes the staged file when this fails.
func publishStaged(root *os.Root, file *os.File, staged, name string, data []byte, perm fs.FileMode) error {
	_, err := file.Write(data)
	if err == nil && perm != 0 {
		// The create mode went through the umask; chmod sets the exact bits.
		err = file.Chmod(perm)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return root.Rename(staged, name)
}

// removeStaged deletes a staged file left by a failed publication. A rename
// that already happened leaves nothing to remove.
func removeStaged(root *os.Root, staged string) error {
	if err := root.Remove(staged); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove staged file %s: %w", staged, err)
	}
	return nil
}

// stagedName is the recorded staging name of a batch entry: hidden, next to
// the target, unique to the batch and entry.
func stagedName(name, batchID string, index int) string {
	return filepath.Join(filepath.Dir(name), fmt.Sprintf(".%s.tempest-%s-%d", filepath.Base(name), batchID, index))
}

// createStaged opens a new, exclusive temporary file next to base in dir:
// staged when given, else a fresh random name.
func createStaged(root *os.Root, dir, base, staged string, perm fs.FileMode) (string, *os.File, error) {
	if staged != "" {
		file, err := root.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		return staged, file, err
	}
	var lastErr error
	for range maxTempAttempts {
		suffix := make([]byte, tempSuffixBytes)
		if _, err := rand.Read(suffix); err != nil {
			return "", nil, err
		}
		candidate := filepath.Join(dir, "."+base+".tempest-"+hex.EncodeToString(suffix))
		file, err := root.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err == nil {
			return candidate, file, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", nil, err
		}
		lastErr = err
	}
	return "", nil, fmt.Errorf("stage %s: %w", filepath.Join(dir, base), lastErr)
}

// Remove deletes name inside root and syncs its directory, so the removal
// survives a crash like a WriteFile does.
func Remove(root *os.Root, name string) error {
	if err := root.Remove(name); err != nil {
		return err
	}
	return syncDir(root, filepath.Dir(name))
}

// syncDir makes a directory's entries (a rename or removal) durable.
func syncDir(root *os.Root, dir string) error {
	handle, err := root.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(handle.Sync(), handle.Close())
}
