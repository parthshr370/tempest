package journal

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ValidateChildPath rejects anything outside the deterministic, non-symlink child chain.
func ValidateChildPath(parentPath, childID, path string) error {
	if !filepath.IsAbs(parentPath) || len(childID) != 16 || strings.ToLower(childID) != childID {
		return fmt.Errorf("invalid child journal identity")
	}
	if _, err := hex.DecodeString(childID); err != nil {
		return fmt.Errorf("invalid child journal identity: %w", err)
	}
	expected := filepath.Join(parentPath+".d", "children", childID, "session.jsonl")
	if path != expected {
		return fmt.Errorf("unexpected child journal path")
	}
	parts := []string{parentPath + ".d", filepath.Join(parentPath+".d", "children"), filepath.Dir(expected), expected}
	for i, part := range parts {
		info, err := os.Lstat(part)
		if err != nil {
			return fmt.Errorf("inspect child journal path: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || (i < len(parts)-1 && !info.IsDir()) || (i == len(parts)-1 && !info.Mode().IsRegular()) {
			return fmt.Errorf("unsafe child journal path %q", part)
		}
	}
	return nil
}

// CreateChild creates the one deterministic child path; it never chooses another id.
func CreateChild(ctx context.Context, parentPath, childID, cwd string, opts Options) (*Store, error) {
	if len(childID) != 16 || strings.ToLower(childID) != childID {
		return nil, fmt.Errorf("invalid child session id")
	}
	if _, err := hex.DecodeString(childID); err != nil {
		return nil, fmt.Errorf("invalid child session id: %w", err)
	}
	if !filepath.IsAbs(parentPath) || !filepath.IsAbs(cwd) {
		return nil, fmt.Errorf("child journal paths must be absolute")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := parentPath + ".d"
	for _, part := range []string{"", "children", childID} {
		if part != "" {
			dir = filepath.Join(dir, part)
		}
		if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
			return nil, err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("unsafe child directory %q", dir)
		}
	}
	path := filepath.Join(dir, "session.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockJournal(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	clock, newID := resolveOptions(opts)
	header, err := json.Marshal(Header{Type: "session", Version: JournalVersion, ID: childID, Timestamp: clock(), Cwd: cwd, Title: opts.Title})
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = writeLine(f, header)
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = syncDirectory(dir)
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("create child journal: %w", err)
	}
	return &Store{path: path, id: childID, file: f, clock: clock, newID: newID}, nil
}
func syncDirectory(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

func upgradeHeader(f *os.File, version int) error {
	if version == JournalVersion {
		return nil
	}
	contents, err := os.ReadFile(f.Name())
	if err != nil {
		return err
	}
	line, _, _ := bytes.Cut(contents, []byte{'\n'})
	d := json.NewDecoder(bytes.NewReader(line))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return fmt.Errorf("upgrade: invalid header object")
	}
	var offset int64 = -1
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return err
		}
		var raw json.RawMessage
		if err = d.Decode(&raw); err != nil {
			return err
		}
		if token == "version" {
			if offset != -1 || len(raw) != 1 || raw[0] != byte('0'+version) {
				return fmt.Errorf("upgrade: noncanonical version token")
			}
			offset = d.InputOffset() - int64(len(raw))
		}
	}
	if offset < 0 {
		return fmt.Errorf("upgrade: missing version token")
	}
	if _, err = f.WriteAt([]byte{'3'}, offset); err != nil {
		return fmt.Errorf("upgrade header: %w", err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("sync upgraded header: %w", err)
	}
	_, err = f.Seek(0, io.SeekEnd)
	return err
}
