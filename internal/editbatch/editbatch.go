// Package editbatch applies a multi-file workspace edit as one recoverable
// unit with a rollback journal, the discipline of SQLite's atomic commit
// (https://sqlite.org/atomiccommit.html): every path's prior state is made
// durable before the first write, and deleting the journal record is the
// commit. A write failure rolls the batch back in process; a process that
// dies mid-batch leaves the record behind, and Recover restores each path it
// can prove is in the batch's pre or post state while preserving the rest as
// a conflict.
//
// This is crash recovery, not isolation: other processes can observe the
// intermediate files while a batch runs.
package editbatch

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// DirName is the session data directory's subdirectory holding the records
// of batches in flight: <journal>.d/edit-rollback/<batch id>/.
const DirName = "edit-rollback"

// ConflictsDirName holds batches recovery could not fully restore, with
// their pre-batch copies, under DirName.
const ConflictsDirName = "conflicts"

const (
	recordName = "record.json"
	// conflictRecordName is the record's name once a batch is retired
	// under conflicts/, where it is kept for reference and never replayed.
	conflictRecordName = "conflict.json"
	// priorsName holds every path's pre-batch bytes back to back.
	priorsName = "prior.bin"
	// conflictCopiesDirName holds a retired batch's conflicted paths'
	// pre-batch bytes as ordinary files, mirroring the workspace layout.
	conflictCopiesDirName = "prior"
	recordVersion         = 1
	batchIDBytes          = 8
	// journalPerm keeps pre-batch copies, which may hold secrets, private.
	journalDirPerm  fs.FileMode = 0o700
	journalFilePerm fs.FileMode = 0o600
)

// ErrInterrupted is returned by ApplyInterrupted: the batch stopped as a
// killed process would, with its record left for Recover.
var ErrInterrupted = errors.New("edit batch interrupted")

// Change is one path's final state in a batch.
type Change struct {
	// Path is relative to the batch root and must not be a symlink.
	Path string
	// Delete removes the file; otherwise Data is its new content.
	Delete bool
	Data   []byte
	// Perm 0 keeps an existing file's permission bits; see WriteFile.
	Perm fs.FileMode
}

// Journal applies batches, recording them under a session data directory.
type Journal struct {
	// dir is <session data dir>/edit-rollback; empty keeps the record in
	// memory only, which still rolls back a failed write in process but
	// cannot survive a crash.
	dir string
}

// Dir returns the rollback directory of sessionDataDir.
func Dir(sessionDataDir string) string {
	if sessionDataDir == "" {
		return ""
	}
	return filepath.Join(sessionDataDir, DirName)
}

// New returns the journal of a session data directory. An empty directory
// (a session without a journal) gives in-process rollback only.
func New(sessionDataDir string) *Journal {
	return &Journal{dir: Dir(sessionDataDir)}
}

// fileState is one path's existence, content hash and permission bits.
type fileState struct {
	Exists bool        `json:"exists"`
	SHA256 string      `json:"sha256,omitempty"`
	Size   int64       `json:"size,omitempty"`
	Perm   fs.FileMode `json:"perm,omitempty"`
}

func (s fileState) sameContent(other fileState) bool {
	return s.Exists == other.Exists && s.SHA256 == other.SHA256
}

// entry records one path of a batch: what it was and what the batch makes it.
type entry struct {
	Path  string    `json:"path"`
	Prior fileState `json:"prior"`
	Post  fileState `json:"post"`
	// CopyOffset is where the prior bytes (Prior.Size of them) start in
	// the batch's prior.bin.
	CopyOffset int64 `json:"copy_offset,omitempty"`
	// Staged is the temporary name a write stages through; a crash between
	// creating and renaming it leaves this file for recovery to remove.
	Staged string `json:"staged,omitempty"`
}

// record is the durable journal of one batch.
type record struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	// Root is the absolute directory every entry path is relative to.
	Root    string    `json:"root"`
	Origin  string    `json:"origin,omitempty"`
	Created time.Time `json:"created"`
	Entries []entry   `json:"entries"`
	// CreatedDirs are directories the batch's writes create; rollback
	// removes them again when they are empty.
	CreatedDirs []string `json:"created_dirs,omitempty"`
}

// batch is a record in flight with its prior bytes held in memory.
type batch struct {
	record record
	prior  [][]byte
	// dir is the batch's journal directory, empty without a journal.
	dir string
	// lock holds the flock that tells Recover the owner is alive.
	lock *os.File
}

// Apply writes changes in order. A single change is published atomically on
// its own; more than one is journaled first, and a failed write rolls back
// every path already written before Apply returns the error. origin names
// the operation in recovery diagnostics (for example the tool call).
func (j *Journal) Apply(root *os.Root, origin string, changes []Change) error {
	return j.apply(root, origin, changes, -1)
}

// ApplyInterrupted writes the first completed changes of a journaled batch
// and then stops as a process killed at that point would: no rollback, the
// record left hot, ErrInterrupted returned. It is the fault-injection entry
// point for crash tests in the packages that drive Recover.
func (j *Journal) ApplyInterrupted(root *os.Root, origin string, changes []Change, completed int) error {
	if len(changes) < 2 || completed < 0 || completed > len(changes) {
		return fmt.Errorf("interrupt needs a multi-path batch and 0 <= completed <= %d", len(changes))
	}
	return j.apply(root, origin, changes, completed)
}

func (j *Journal) apply(root *os.Root, origin string, changes []Change, interruptAt int) error {
	if len(changes) == 0 {
		return nil
	}
	if len(changes) == 1 {
		return applyChange(root, changes[0], "")
	}
	current, err := j.begin(root, origin, changes)
	if err != nil {
		return err
	}
	for index, change := range changes {
		if index == interruptAt {
			return current.abandon()
		}
		if err := applyChange(root, change, current.record.Entries[index].Staged); err != nil {
			return current.rollback(root, fmt.Errorf("writing %s failed: %w", change.Path, err))
		}
	}
	if interruptAt == len(changes) {
		return current.abandon()
	}
	return current.commit(root)
}

// applyChange publishes one change, staging a write through staged when
// the batch recorded a name for it.
func applyChange(root *os.Root, change Change, staged string) error {
	if change.Delete {
		return Remove(root, change.Path)
	}
	return writeFile(root, change.Path, staged, change.Data, change.Perm)
}

// begin captures every path's prior state and, with a journal, makes the
// record durable before any workspace byte changes.
func (j *Journal) begin(root *os.Root, origin string, changes []Change) (*batch, error) {
	id, err := newBatchID()
	if err != nil {
		return nil, err
	}
	rootDir, err := filepath.Abs(root.Name())
	if err != nil {
		return nil, err
	}
	current := &batch{record: record{Version: recordVersion, ID: id, Root: rootDir, Origin: origin, Created: time.Now().UTC()}}
	seen := map[string]bool{}
	createdDirs := map[string]bool{}
	for index, change := range changes {
		name := filepath.Clean(change.Path)
		if name == "." || filepath.IsAbs(name) || seen[name] {
			return nil, fmt.Errorf("edit batch: invalid or repeated path %q", change.Path)
		}
		seen[name] = true
		prior, data, err := readState(root, name)
		if err != nil {
			return nil, err
		}
		post := fileState{}
		if !change.Delete {
			post = fileState{Exists: true, SHA256: digest(change.Data), Perm: change.Perm}
			if err := missingParents(root, name, createdDirs); err != nil {
				return nil, err
			}
		}
		item := entry{Path: name, Prior: prior, Post: post}
		if !change.Delete {
			item.Staged = stagedName(name, id, index)
		}
		current.record.Entries = append(current.record.Entries, item)
		current.prior = append(current.prior, data)
	}
	for dir := range createdDirs {
		current.record.CreatedDirs = append(current.record.CreatedDirs, dir)
	}
	slices.Sort(current.record.CreatedDirs)
	if j.dir == "" {
		return current, nil
	}
	if err := current.persist(j.dir); err != nil {
		return nil, errors.Join(fmt.Errorf("edit batch journal: %w", err), current.discard())
	}
	return current, nil
}

// persist writes the prior copies, then the record under an flock, syncing
// each so the record never names a copy that is not on disk.
func (b *batch) persist(journalDir string) error {
	if err := ensureDir(journalDir); err != nil {
		return err
	}
	dir := filepath.Join(journalDir, b.record.ID)
	if err := os.Mkdir(dir, journalDirPerm); err != nil {
		return err
	}
	b.dir = dir
	if err := b.writePriors(filepath.Join(dir, priorsName)); err != nil {
		return err
	}
	encoded, err := json.Marshal(b.record)
	if err != nil {
		return err
	}
	staged := filepath.Join(dir, recordName+".tmp")
	file, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, journalFilePerm)
	if err != nil {
		return err
	}
	// The lock is taken before the record becomes visible, so Recover never
	// sees a live batch's record unlocked. It follows the inode through the
	// rename and is released by close or by the process dying.
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return errors.Join(err, file.Close())
	}
	b.lock = file
	if _, err := file.Write(encoded); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := os.Rename(staged, filepath.Join(dir, recordName)); err != nil {
		return err
	}
	if err := syncHostDir(dir); err != nil {
		return err
	}
	return syncHostDir(journalDir)
}

// commit deletes the record, the batch's durable commit point, then the
// copies. A record that cannot be deleted means the batch did not commit,
// so it is rolled back.
func (b *batch) commit(root *os.Root) error {
	if b.dir == "" {
		return nil
	}
	if err := os.Remove(filepath.Join(b.dir, recordName)); err != nil {
		return b.rollback(root, fmt.Errorf("committing the edit batch failed: %w", err))
	}
	if err := syncHostDir(b.dir); err != nil {
		return b.rollback(root, fmt.Errorf("committing the edit batch failed: %w", err))
	}
	// Committed. A leftover directory without a record is inert: Recover
	// deletes it, so cleanup failures here only cost disk until then.
	return b.discard()
}

// rollback restores the written paths after a failed write. A clean restore
// discards the journal; anything else leaves it for Recover or preserves it
// as a conflict, and the error says which.
func (b *batch) rollback(root *os.Root, cause error) error {
	outcome := settle(root, b.record, func(index int) ([]byte, error) { return b.prior[index], nil })
	outcome.Origin = b.record.Origin
	if outcome.Err != nil {
		// Keep the record hot: the next session open retries the restore.
		return errors.Join(cause, fmt.Errorf("rolling the batch back failed, the journal stays for recovery: %w", outcome.Err), b.release())
	}
	if len(outcome.Conflicts) > 0 {
		preserved, err := b.preserve(outcome.Conflicts)
		outcome.Preserved = preserved
		return errors.Join(cause, errors.New(outcome.String()), err)
	}
	if err := b.discard(); err != nil {
		return errors.Join(cause, err)
	}
	return fmt.Errorf("%w; the batch was rolled back and no file changed", cause)
}

// abandon stops without rollback or commit, as a dead process would.
func (b *batch) abandon() error {
	return errors.Join(ErrInterrupted, b.release())
}

// release drops the liveness lock, leaving the record in place.
func (b *batch) release() error {
	if b.lock == nil {
		return nil
	}
	lock := b.lock
	b.lock = nil
	return lock.Close()
}

// discard removes the batch's journal directory.
func (b *batch) discard() error {
	err := b.release()
	if b.dir == "" {
		return err
	}
	if removeErr := os.RemoveAll(b.dir); removeErr != nil {
		return errors.Join(err, removeErr)
	}
	return errors.Join(err, syncHostDir(filepath.Dir(b.dir)))
}

// preserve moves the batch directory under conflicts/, where it is no longer
// a hot journal but keeps the pre-batch copies for the user.
func (b *batch) preserve(conflicts []Conflict) (string, error) {
	err := b.release()
	if b.dir == "" {
		return "", err
	}
	return preserveDir(b.dir, b.record, conflicts, func(index int) ([]byte, error) { return b.prior[index], nil }, err)
}

// preserveDir retires a batch directory under conflicts/: each conflicted
// path's pre-batch bytes are written out as prior/<path> for the user, and
// the record is renamed so the batch can never be replayed.
func preserveDir(dir string, rec record, conflicts []Conflict, prior func(int) ([]byte, error), earlier error) (string, error) {
	if err := exportConflictCopies(dir, rec, conflicts, prior); err != nil {
		return "", errors.Join(earlier, err)
	}
	retired := filepath.Join(filepath.Dir(dir), ConflictsDirName)
	if err := ensureDir(retired); err != nil {
		return "", errors.Join(earlier, err)
	}
	target := filepath.Join(retired, filepath.Base(dir))
	if err := os.Rename(filepath.Join(dir, recordName), filepath.Join(dir, conflictRecordName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", errors.Join(earlier, err)
	}
	if err := os.Rename(dir, target); err != nil {
		return "", errors.Join(earlier, err)
	}
	return target, errors.Join(earlier, syncHostDir(retired), syncHostDir(filepath.Dir(dir)))
}

// exportConflictCopies writes the pre-batch bytes of every conflicted path
// that existed before the batch to <dir>/prior/<path>.
func exportConflictCopies(dir string, rec record, conflicts []Conflict, prior func(int) ([]byte, error)) error {
	indexOf := make(map[string]int, len(rec.Entries))
	for index, item := range rec.Entries {
		indexOf[item.Path] = index
	}
	for _, conflict := range conflicts {
		index, ok := indexOf[conflict.Path]
		if !ok || !rec.Entries[index].Prior.Exists {
			continue
		}
		data, err := prior(index)
		if err != nil {
			return err
		}
		target := filepath.Join(dir, conflictCopiesDirName, conflict.Path)
		if err := os.MkdirAll(filepath.Dir(target), journalDirPerm); err != nil {
			return err
		}
		if err := writeSynced(target, data); err != nil {
			return err
		}
	}
	return nil
}

// readState stats and, for an existing regular file, reads name.
func readState(root *os.Root, name string) (fileState, []byte, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return fileState{}, nil, nil
	}
	if err != nil {
		return fileState{}, nil, err
	}
	if !info.Mode().IsRegular() {
		return fileState{}, nil, fmt.Errorf("%s is not a regular file", name)
	}
	data, err := root.ReadFile(name)
	if err != nil {
		return fileState{}, nil, err
	}
	return fileState{Exists: true, SHA256: digest(data), Size: int64(len(data)), Perm: info.Mode().Perm()}, data, nil
}

// missingParents adds name's ancestor directories that do not exist yet.
func missingParents(root *os.Root, name string, created map[string]bool) error {
	for dir := filepath.Dir(name); dir != "."; dir = filepath.Dir(dir) {
		_, err := root.Lstat(dir)
		if err == nil {
			return nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		created[dir] = true
	}
	return nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func newBatchID() (string, error) {
	random := make([]byte, batchIDBytes)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(random), nil
}

// ensureDir creates a private journal directory and syncs its parent so the
// new entry survives a crash.
func ensureDir(dir string) error {
	info, err := os.Lstat(dir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := ensureDir(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.Mkdir(dir, journalDirPerm); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return syncHostDir(filepath.Dir(dir))
}

func writeSynced(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, journalFilePerm)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	return errors.Join(err, file.Close())
}

// writePriors writes every existing path's prior bytes into one file with
// one sync, recording each entry's offset; the record is written after it.
func (b *batch) writePriors(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, journalFilePerm)
	if err != nil {
		return err
	}
	var offset int64
	for index := range b.record.Entries {
		item := &b.record.Entries[index]
		if !item.Prior.Exists {
			continue
		}
		item.CopyOffset = offset
		written, err := file.Write(b.prior[index])
		offset += int64(written)
		if err != nil {
			return errors.Join(err, file.Close())
		}
	}
	return errors.Join(file.Sync(), file.Close())
}

func syncHostDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(handle.Sync(), handle.Close())
}
