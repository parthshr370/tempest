package editbatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"go.harness.dev/harness/internal/toolio"
)

// maxRecordBytes bounds a record read during recovery; a batch names at
// most a few thousand paths.
const maxRecordBytes = 16 << 20

// Conflict is a path recovery left as found because its content matches
// neither the batch's pre-batch nor its intended state: someone changed it
// after the crash, and restoring would clobber that edit.
type Conflict struct {
	Path   string
	Reason string
}

// Outcome reports what recovery did with one interrupted batch.
type Outcome struct {
	Batch  string
	Origin string
	Root   string
	// Restored paths were returned to their pre-batch content.
	Restored []string
	// Untouched paths were still in their pre-batch state.
	Untouched []string
	Conflicts []Conflict
	// Preserved is the directory keeping the pre-batch copies of a batch
	// with conflicts.
	Preserved string
	// Err means recovery could not finish; the record stays hot and the
	// next session open retries it.
	Err error
}

// String is the outcome as one diagnostic line.
func (o Outcome) String() string {
	var text strings.Builder
	fmt.Fprintf(&text, "edit batch %s", o.Batch)
	if o.Origin != "" {
		fmt.Fprintf(&text, " (%s)", o.Origin)
	}
	if o.Root != "" {
		fmt.Fprintf(&text, " in %s", o.Root)
	}
	text.WriteString(" was interrupted before it committed")
	if len(o.Restored) > 0 {
		text.WriteString("; restored to their pre-batch content: " + strings.Join(o.Restored, ", "))
	}
	if len(o.Untouched) > 0 {
		text.WriteString("; still at their pre-batch content: " + strings.Join(o.Untouched, ", "))
	}
	for _, conflict := range o.Conflicts {
		if conflict.Path == "" {
			text.WriteString("; conflict: " + conflict.Reason)
			continue
		}
		fmt.Fprintf(&text, "; conflict, left as found: %s (%s)", conflict.Path, conflict.Reason)
	}
	if o.Preserved != "" {
		text.WriteString("; pre-batch copies kept in " + o.Preserved)
	}
	if o.Err != nil {
		text.WriteString("; recovery failed and is retried at the next session open: " + o.Err.Error())
	}
	return text.String()
}

// Recover restores every batch a dead process left in sessionDataDir's
// rollback directory. A batch whose owner is still alive (its record is
// flocked) is skipped. Each path is restored only when its current content
// is the batch's pre or post state; anything else is a conflict, left as
// found, and the batch moves under conflicts/ with its pre-batch copies.
// queue serializes the restores with in-process file tools; nil skips that.
func Recover(sessionDataDir string, queue *toolio.FileMutationQueue) ([]Outcome, error) {
	dir := Dir(sessionDataDir)
	if dir == "" {
		return nil, nil
	}
	items, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read edit rollback journal: %w", err)
	}
	var outcomes []Outcome
	for _, item := range items {
		if !item.IsDir() || item.Name() == ConflictsDirName {
			continue
		}
		outcome, interrupted := recoverBatch(filepath.Join(dir, item.Name()), queue)
		if interrupted {
			outcomes = append(outcomes, outcome)
		}
	}
	return outcomes, nil
}

// recoverBatch settles one batch directory. It reports false for a batch
// that needs nothing: still running, or committed and only awaiting cleanup.
func recoverBatch(batchDir string, queue *toolio.FileMutationQueue) (Outcome, bool) {
	name := filepath.Base(batchDir)
	recordPath := filepath.Join(batchDir, recordName)
	file, err := os.Open(recordPath)
	if errors.Is(err, fs.ErrNotExist) {
		// No published record: the batch never wrote a workspace byte, or it
		// committed. Either way the directory is leftover journal material.
		if removeErr := os.RemoveAll(batchDir); removeErr != nil {
			return Outcome{Batch: name, Err: removeErr}, true
		}
		return Outcome{}, false
	}
	if err != nil {
		return Outcome{Batch: name, Err: err}, true
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return Outcome{}, false
		}
		return Outcome{Batch: name, Err: err}, true
	}
	if !stillPublished(file, recordPath) {
		return Outcome{}, false
	}
	var rec record
	encoded, err := io.ReadAll(io.LimitReader(file, maxRecordBytes))
	if err == nil {
		err = json.Unmarshal(encoded, &rec)
	}
	if err == nil && rec.Version != recordVersion {
		err = fmt.Errorf("unsupported record version %d", rec.Version)
	}
	if err != nil {
		return preserveUnusable(batchDir, Outcome{Batch: name}, "the journal record is unreadable: "+err.Error())
	}
	base := Outcome{Batch: rec.ID, Origin: rec.Origin, Root: rec.Root}
	root, err := os.OpenRoot(rec.Root)
	if err != nil {
		return preserveUnusable(batchDir, base, "the workspace is unavailable: "+err.Error())
	}
	defer root.Close()
	priors, prior, err := priorReader(batchDir, rec)
	if err != nil {
		return preserveUnusable(batchDir, base, "the pre-batch copies are unreadable: "+err.Error())
	}
	defer priors.Close()
	keys := make([]string, 0, len(rec.Entries))
	for _, item := range rec.Entries {
		keys = append(keys, filepath.Join(rec.Root, item.Path))
	}
	var outcome Outcome
	lockErr := lockPaths(queue, keys, func() error {
		outcome = settle(root, rec, prior)
		return nil
	})
	outcome.Origin = rec.Origin
	if lockErr != nil {
		outcome.Err = errors.Join(outcome.Err, lockErr)
	}
	if outcome.Err != nil {
		return outcome, true
	}
	if len(outcome.Conflicts) > 0 {
		outcome.Preserved, outcome.Err = preserveDir(batchDir, rec, outcome.Conflicts, prior, nil)
		return outcome, true
	}
	if err := os.RemoveAll(batchDir); err != nil {
		outcome.Err = err
		return outcome, true
	}
	if err := syncHostDir(filepath.Dir(batchDir)); err != nil {
		outcome.Err = err
	}
	return outcome, true
}

// stillPublished reports whether the locked file is still the batch's
// record: its owner may have committed (unlinking it) between our open and
// our lock.
func stillPublished(file *os.File, recordPath string) bool {
	held, err := file.Stat()
	if err != nil {
		return false
	}
	current, err := os.Stat(recordPath)
	if err != nil {
		return false
	}
	return os.SameFile(held, current)
}

// preserveUnusable moves a batch recovery cannot evaluate under conflicts/.
func preserveUnusable(batchDir string, outcome Outcome, reason string) (Outcome, bool) {
	outcome.Conflicts = append(outcome.Conflicts, Conflict{Reason: reason})
	outcome.Preserved, outcome.Err = preserveDir(batchDir, record{}, nil, nil, nil)
	return outcome, true
}

// priorReader returns the pre-batch bytes of a record's entries from the
// batch's prior.bin. The caller closes the file.
func priorReader(batchDir string, rec record) (*os.File, func(int) ([]byte, error), error) {
	file, err := os.Open(filepath.Join(batchDir, priorsName))
	if err != nil {
		return nil, nil, err
	}
	return file, func(index int) ([]byte, error) {
		item := rec.Entries[index]
		data := make([]byte, item.Prior.Size)
		if _, err := file.ReadAt(data, item.CopyOffset); err != nil {
			return nil, fmt.Errorf("read the pre-batch copy of %s: %w", item.Path, err)
		}
		return data, nil
	}, nil
}

// settle restores a batch's paths, last written first. prior returns an
// entry's pre-batch bytes.
func settle(root *os.Root, rec record, prior func(int) ([]byte, error)) Outcome {
	outcome := Outcome{Batch: rec.ID, Root: rec.Root}
	for index := len(rec.Entries) - 1; index >= 0; index-- {
		item := rec.Entries[index]
		if item.Staged != "" {
			// A write killed between staging and rename leaves its staged file.
			if err := removeStaged(root, item.Staged); err != nil {
				outcome.Err = errors.Join(outcome.Err, err)
			}
		}
		current, _, err := readState(root, item.Path)
		if err != nil {
			outcome.Conflicts = append(outcome.Conflicts, Conflict{Path: item.Path, Reason: err.Error()})
			continue
		}
		switch {
		case current.sameContent(item.Prior):
			outcome.Untouched = append(outcome.Untouched, item.Path)
		case current.sameContent(item.Post):
			reason, err := restore(root, item, func() ([]byte, error) { return prior(index) })
			switch {
			case err != nil:
				outcome.Err = errors.Join(outcome.Err, fmt.Errorf("restore %s: %w", item.Path, err))
			case reason != "":
				outcome.Conflicts = append(outcome.Conflicts, Conflict{Path: item.Path, Reason: reason})
			default:
				outcome.Restored = append(outcome.Restored, item.Path)
			}
		default:
			outcome.Conflicts = append(outcome.Conflicts, Conflict{Path: item.Path, Reason: "changed after the interruption; matches neither its pre-batch nor its batch content"})
		}
	}
	slices.Reverse(outcome.Restored)
	slices.Reverse(outcome.Untouched)
	slices.Reverse(outcome.Conflicts)
	if outcome.Err == nil {
		outcome.Err = removeCreatedDirs(root, rec.CreatedDirs)
	}
	return outcome
}

// restore returns one path in its post state to its prior state. A damaged
// pre-batch copy is reported as a conflict reason, never written.
func restore(root *os.Root, item entry, prior func() ([]byte, error)) (string, error) {
	if !item.Prior.Exists {
		return "", Remove(root, item.Path)
	}
	data, err := prior()
	if err != nil {
		return "", err
	}
	if digest(data) != item.Prior.SHA256 {
		return "the pre-batch copy does not match its recorded hash", nil
	}
	return "", WriteFile(root, item.Path, data, item.Prior.Perm)
}

// removeCreatedDirs removes the directories a batch created, deepest first.
// A directory that is gone already or holds files someone else added since
// is left alone: it is no longer the batch's to remove.
func removeCreatedDirs(root *os.Root, dirs []string) error {
	ordered := slices.Clone(dirs)
	slices.SortFunc(ordered, func(a, b string) int {
		return strings.Count(b, string(filepath.Separator)) - strings.Count(a, string(filepath.Separator))
	})
	for _, dir := range ordered {
		err := root.Remove(dir)
		if err == nil || errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			continue
		}
		return fmt.Errorf("remove %s: %w", dir, err)
	}
	return nil
}

// lockPaths runs fn holding the mutation lock of every key, taken in sorted
// order like the file tools take them, so recovery cannot interleave with an
// in-process edit of the same file.
func lockPaths(queue *toolio.FileMutationQueue, keys []string, fn func() error) error {
	if queue == nil {
		return fn()
	}
	ordered := slices.Clone(keys)
	slices.Sort(ordered)
	ordered = slices.Compact(ordered)
	var lockNext func(index int) error
	lockNext = func(index int) error {
		if index == len(ordered) {
			return fn()
		}
		return queue.Do(ordered[index], func() error { return lockNext(index + 1) })
	}
	return lockNext(0)
}
