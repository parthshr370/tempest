package hashline

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
)

// FileSystem is what Prepare needs from the host: containment-checked path
// resolution and reads. Paths are as written in the patch.
type FileSystem interface {
	// Key returns the snapshot-store key of the file path names (its
	// canonical path), or an error for a path outside the workspace.
	Key(path string) (string, error)
	// Read returns the file's bytes, or an error wrapping fs.ErrNotExist.
	Read(path string) ([]byte, error)
}

// binaryProbeBytes is how much of a file is checked for NUL bytes, as read
// does, before hashline refuses it as binary.
const binaryProbeBytes = 8 * 1024

// Op is what committing a Change does to the disk.
type Op int

const (
	// OpUpdate writes Encoded to Path.
	OpUpdate Op = iota
	// OpRemove deletes Path.
	OpRemove
	// OpMove moves Path to Dest, then writes Encoded there.
	OpMove
)

// Change is one section's validated effect, ready to commit.
type Change struct {
	Op   Op
	Path string
	Key  string
	// Dest and DestKey are set for OpMove.
	Dest    string
	DestKey string
	// Before is the decoded file as read; After the new normalized text.
	Before File
	After  string
	// Encoded is After with the file's BOM and line endings restored.
	Encoded []byte
	// Notes report how block locators resolved.
	Notes []string

	origins []int
}

// Plan is a fully validated patch: every section parsed, bound to its tag,
// checked against displayed lines and applied in memory. Nothing has been
// written.
type Plan struct {
	Changes  []*Change
	Warnings []string

	registers map[string][]string
}

// Prepare validates and applies every section of patch in memory, in patch
// order, so registers cut in one section can be pasted in a later one. Any
// failure rejects the whole patch before a byte is written. The caller holds
// the file locks of every path the patch names until it has committed.
func Prepare(patch *Patch, fsys FileSystem, store *Store) (*Plan, error) {
	clip := &clipboard{named: store.registerSnapshot()}
	plan := &Plan{Warnings: patch.Warnings}
	claimed := map[string]string{} // key -> path that claimed it
	claim := func(path, key string) error {
		if prev, ok := claimed[key]; ok {
			return fmt.Errorf("%s and %s name the same file; merge their ops under one [PATH#TAG] header", prev, path)
		}
		claimed[key] = path
		return nil
	}
	for _, sec := range patch.Sections {
		header := fmt.Sprintf("[%s#%s]", sec.Path, sec.Tag)
		key, err := fsys.Key(sec.Path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", header, err)
		}
		if err := claim(sec.Path, key); err != nil {
			return nil, err
		}
		raw, err := fsys.Read(sec.Path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: %s does not exist. hashline edits existing files only; create new files with write", header, sec.Path)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", header, err)
		}
		if bytes.IndexByte(raw[:min(len(raw), binaryProbeBytes)], 0) >= 0 {
			return nil, fmt.Errorf("%s: %s is a binary file; hashline edits text files only", header, sec.Path)
		}
		file := Decode(raw)
		matched, err := store.matchTag(key, sec.Path, sec.Tag, file.Text, namedLines(sec))
		if err != nil {
			return nil, err
		}
		change := &Change{Op: OpUpdate, Path: sec.Path, Key: key, Before: file, After: file.Text}
		if sec.Remove {
			change.Op = OpRemove
			plan.Changes = append(plan.Changes, change)
			continue
		}
		if len(sec.Hunks) > 0 {
			result, err := applySection(sec, file.Text, clip)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", header, err)
			}
			if err := store.checkSeen(matched, sec.Path, sec.Tag, file.Text, result.touched); err != nil {
				return nil, err
			}
			change.After = joinLines(result.lines, file.Text)
			change.origins = result.origins
			change.Notes = result.notes
		} else {
			for i := range Lines(file.Text) {
				change.origins = append(change.origins, i+1)
			}
		}
		if sec.Dest != "" {
			destKey, err := fsys.Key(sec.Dest)
			if err != nil {
				return nil, fmt.Errorf("%s: MV %s: %w", header, sec.Dest, err)
			}
			if destKey == key {
				return nil, fmt.Errorf("%s: MV destination %s is the file itself", header, sec.Dest)
			}
			if _, err := fsys.Read(sec.Dest); err == nil {
				return nil, fmt.Errorf("%s: MV destination %s already exists; MV never overwrites. Pick another path, or REM the destination in a separate call first", header, sec.Dest)
			} else if !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("%s: MV %s: %w", header, sec.Dest, err)
			}
			if err := claim(sec.Dest, destKey); err != nil {
				return nil, err
			}
			change.Op, change.Dest, change.DestKey = OpMove, sec.Dest, destKey
		} else if change.After == file.Text {
			return nil, fmt.Errorf("%s: the edits leave %s unchanged (the bodies reproduce the current lines). Re-read the file if you expected a change", header, sec.Path)
		}
		change.Encoded = file.Encode(change.After)
		plan.Changes = append(plan.Changes, change)
	}
	plan.registers = clip.named
	return plan, nil
}

// Committed records a change that reached the disk and returns the file's
// new tag and a numbered preview of the changed lines (both empty for
// OpRemove).
func (s *Store) Committed(c *Change) (tag, preview string) {
	if c.Op == OpRemove {
		s.Forget(c.Key)
		return "", ""
	}
	tag, preview = s.derive(c.Key, c.Before.Text, c.After, Lines(c.After), c.origins, len(Lines(c.Before.Text)))
	if c.Op == OpMove {
		s.relocate(c.Key, c.DestKey)
	}
	return tag, preview
}

// CommitRegisters publishes the named registers of a fully committed plan
// so later calls can paste them.
func (s *Store) CommitRegisters(p *Plan) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registers = maps.Clone(p.registers)
}

// relocate moves the version history of from to to.
func (s *Store) relocate(from, to string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.paths[from]; ok {
		s.paths[to] = h
		delete(s.paths, from)
	}
}
