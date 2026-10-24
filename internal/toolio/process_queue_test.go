package toolio

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestProcessMutationQueuesShareFileLock(t *testing.T) {
	dir := t.TempDir()
	first, err := NewProcessFileMutationQueue(filepath.Join(dir, "locks"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewProcessFileMutationQueue(filepath.Join(dir, "locks"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := first.Do(path, func() error {
		entries, err := os.ReadDir(first.processDir)
		if err != nil {
			return err
		}
		if len(entries) != 1 {
			t.Fatalf("lockfiles: %v", entries)
		}
		file, err := os.OpenFile(filepath.Join(second.processDir, entries[0].Name()), os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer file.Close()
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
			t.Fatal("another daemon can enter while mutation is live")
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := second.Do(path, func() error { return os.WriteFile(path, []byte("after"), 0600) }); err != nil {
		t.Fatal(err)
	}
}

func TestProcessMutationKeyStableAcrossCreationThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	real, link := filepath.Join(dir, "real"), filepath.Join(dir, "link")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(link, "new-file")
	before, err := mutationQueueKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	after, err := mutationQueueKey(path)
	if err != nil || before != after {
		t.Fatalf("lock key changed after publication: %q != %q, %v", before, after, err)
	}
}
