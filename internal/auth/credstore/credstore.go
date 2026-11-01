// Package credstore keeps owner-only credential files: one 0700 directory,
// files replaced atomically at mode 0600, and a per-name advisory lock that
// serializes refreshes across processes.
package credstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Dir is one credential directory. Label names the credential kind in errors.
type Dir struct {
	Path  string
	Label string
}

// Lock creates the directory when needed, refuses one other users can read,
// and holds an exclusive lock on <name>.lock until the returned func runs.
func (d Dir) Lock(ctx context.Context, name string) (func(), error) {
	if err := os.MkdirAll(d.Path, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(d.Path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("%s credential directory must be owner-only (0700)", d.Label)
	}
	f, err := os.OpenFile(filepath.Join(d.Path, name+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// Read returns a credential file's bytes. A missing file is os.ErrNotExist;
// a file that is not a regular owner-only file is refused.
func (d Dir) Read(name string) ([]byte, error) {
	path := filepath.Join(d.Path, name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("%s credential file must be a regular owner-only file (0600)", d.Label)
	}
	return os.ReadFile(path)
}

// Write replaces name with data through a synced 0600 temporary file and a
// rename, then syncs the directory, so a crash leaves the old or new file.
func (d Dir) Write(name string, data []byte) error {
	f, err := os.CreateTemp(d.Path, ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(d.Path, name))
	}
	if err != nil {
		return err
	}
	return d.sync()
}

// Remove deletes name; a missing file is not an error.
func (d Dir) Remove(name string) error {
	if err := os.Remove(filepath.Join(d.Path, name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return d.sync()
}

func (d Dir) sync() error {
	dir, err := os.Open(d.Path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
