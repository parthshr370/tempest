package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/work"
)

const maxSnapshotBytes = 16 << 20

func safeComponent(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
}

// WriteSnapshot commits the exact replacement projection before its journal entry.
func (s *Store) WriteSnapshot(id string, messages []types.Message) (*work.Artifact, error) {
	if !safeComponent(id) {
		return nil, fmt.Errorf("invalid compaction id")
	}
	data, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	if len(data) > maxSnapshotBytes {
		return nil, fmt.Errorf("compaction snapshot exceeds 16 MiB")
	}
	dir := s.path + ".d"
	for _, component := range []string{"", "compaction"} {
		if component != "" {
			dir = filepath.Join(dir, component)
		}
		if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
			return nil, err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("unsafe snapshot directory")
		}
	}
	path := filepath.Join(dir, id+".json")
	hash := sha256.Sum256(data)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("unsafe existing snapshot")
		}
		prior, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if string(prior) != string(data) {
			return nil, fmt.Errorf("conflicting compaction snapshot")
		}
		return &work.Artifact{Path: path, SHA256: hex.EncodeToString(hash[:]), Bytes: int64(len(data))}, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".snapshot-")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	if err = os.Rename(tmp, path); err != nil {
		return nil, err
	}
	if err = syncDirectory(dir); err != nil {
		return nil, err
	}
	return &work.Artifact{Path: path, SHA256: hex.EncodeToString(hash[:]), Bytes: int64(len(data))}, nil
}
func readSnapshot(journalPath string, e CompactionEntry) ([]types.Message, error) {
	a := e.Snapshot
	if !safeComponent(e.CompactionID) || !validDigest(e.SourceHash) || a == nil || a.Bytes < 0 || a.Bytes > maxSnapshotBytes || !validDigest(a.SHA256) {
		return nil, fmt.Errorf("invalid compaction snapshot metadata")
	}
	expected := filepath.Join(journalPath+".d", "compaction", e.CompactionID+".json")
	if filepath.Clean(a.Path) != expected {
		return nil, fmt.Errorf("compaction snapshot path outside owned directory")
	}
	for _, path := range []string{journalPath + ".d", filepath.Dir(expected), expected} {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("compaction snapshot: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("compaction snapshot symlink")
		}
	}
	f, err := os.Open(expected)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSnapshotBytes+1))
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(data)
	if int64(len(data)) != a.Bytes || hex.EncodeToString(hash[:]) != a.SHA256 {
		return nil, fmt.Errorf("compaction snapshot hash/size mismatch")
	}
	var raw []json.RawMessage
	if err = json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	messages := make([]types.Message, 0, len(raw))
	for _, encoded := range raw {
		message, err := types.UnmarshalMessage(encoded)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, nil
}

// ReadSnapshot verifies captured task input or output before using its messages.
func (s *Store) ReadSnapshot(id, source string, artifact *work.Artifact) ([]types.Message, error) {
	return readSnapshot(s.path, CompactionEntry{CompactionID: id, SourceHash: source, Snapshot: artifact})
}
