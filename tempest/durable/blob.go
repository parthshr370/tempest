package durable

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// EncodeEnvelope spills large atomic batches before publishing their reference.
// The checksum remains over the logical commit, independent of its disk layout.
func EncodeEnvelope(path string, e Envelope) ([]byte, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	spill := len(raw) > MaxBatchBytes
	for _, change := range e.Commit.Changes {
		spill = spill || len(change.Value) > MaxValueBytes
	}
	if !spill {
		return raw, nil
	}
	value, err := json.Marshal(e.Commit)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(value)
	digest := hex.EncodeToString(sum[:])
	dir := path + ".blobs"
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".pending-")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	defer os.Remove(name)
	n, err := f.Write(value)
	if err == nil && n != len(value) {
		err = io.ErrShortWrite
	}
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return nil, err
	}
	if err = os.Rename(name, filepath.Join(dir, digest)); err != nil {
		return nil, err
	}
	for _, directory := range []string{dir, filepath.Dir(dir)} {
		d, openErr := os.Open(directory)
		if openErr != nil {
			return nil, openErr
		}
		if err = errors.Join(d.Sync(), d.Close()); err != nil {
			return nil, err
		}
	}
	e.Commit = Commit{}
	e.Blob = digest
	return json.Marshal(e)
}

// DecodeEnvelope verifies immutable storage before exposing a logical commit.
func DecodeEnvelope(path string, raw []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return e, err
	}
	if e.Blob == "" {
		return e, nil
	}
	digest, err := hex.DecodeString(e.Blob)
	if err != nil || len(digest) != sha256.Size || len(e.Commit.Changes) != 0 || e.Commit.ID != "" {
		return e, fmt.Errorf("%w: invalid blob reference", ErrCorrupt)
	}
	value, err := os.ReadFile(filepath.Join(path+".blobs", e.Blob))
	if err != nil {
		return e, fmt.Errorf("%w: blob: %v", ErrCorrupt, err)
	}
	sum := sha256.Sum256(value)
	if hex.EncodeToString(sum[:]) != e.Blob {
		return e, fmt.Errorf("%w: blob checksum", ErrCorrupt)
	}
	if err = json.Unmarshal(value, &e.Commit); err != nil {
		return e, fmt.Errorf("%w: blob commit: %v", ErrCorrupt, err)
	}
	e.Blob = ""
	return e, nil
}
