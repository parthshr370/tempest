package journal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"go.harness.dev/harness/tempest/durable"
)

func checkArchiveObject(raw []byte, digest string) ([]byte, error) {
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != digest {
		return nil, fmt.Errorf("%w: archive object checksum", durable.ErrCorrupt)
	}
	return raw, nil
}

func readFileArchiveObject(journalPath, digest string) ([]byte, error) {
	path, err := archivePath(journalPath, digest)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("%w: archive object missing: %v", durable.ErrCorrupt, err)
	}
	defer root.Close()
	raw, err := root.ReadFile(filepath.Base(path))
	if err != nil {
		return nil, fmt.Errorf("%w: archive object missing: %v", durable.ErrCorrupt, err)
	}
	return checkArchiveObject(raw, digest)
}

func (s *Store) readArchiveObject(ctx context.Context, digest string) ([]byte, error) {
	if !validDigest(digest) {
		return nil, fmt.Errorf("%w: invalid archive object digest", durable.ErrCorrupt)
	}
	page, err := s.durable.Read(ctx, durable.Query{SessionID: archiveSession, Kind: durable.EntryKind, Namespace: archiveNamespace, ID: digest, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(page.Records) != 1 {
		return nil, fmt.Errorf("%w: archive object missing", durable.ErrCorrupt)
	}
	return checkArchiveObject(page.Records[0].Value, digest)
}

// Each cut has a small manifest; overlapping cuts share their immutable payloads.
func packArchive(segment archiveSegment, put func([]byte) (string, error)) (archiveSegment, error) {
	segment.Objects = make(map[string][]string)
	payloads := map[string][]json.RawMessage{"records": segment.Records, "messages": segment.Messages}
	for name, records := range map[string][]durable.Record{"frames": segment.Frames, "state": segment.State} {
		for _, record := range records {
			raw, err := json.Marshal(record)
			if err != nil {
				return segment, err
			}
			payloads[name] = append(payloads[name], raw)
		}
	}
	for _, name := range []string{"records", "messages", "frames", "state"} {
		for _, raw := range payloads[name] {
			digest, err := put(raw)
			if err != nil {
				return segment, err
			}
			segment.Objects[name] = append(segment.Objects[name], digest)
		}
	}
	segment.Records, segment.Messages, segment.Frames, segment.State = nil, nil, nil, nil
	return segment, nil
}

func hydrateArchive(segment archiveSegment, get func(string) ([]byte, error)) (archiveSegment, error) {
	for name, digests := range segment.Objects {
		for _, digest := range digests {
			raw, err := get(digest)
			if err != nil {
				return segment, err
			}
			switch name {
			case "records":
				segment.Records = append(segment.Records, json.RawMessage(raw))
			case "messages":
				segment.Messages = append(segment.Messages, json.RawMessage(raw))
			case "frames", "state":
				var record durable.Record
				if err := json.Unmarshal(raw, &record); err != nil {
					return segment, fmt.Errorf("%w: archive record: %v", durable.ErrCorrupt, err)
				}
				if name == "frames" {
					segment.Frames = append(segment.Frames, record)
				} else {
					segment.State = append(segment.State, record)
				}
			default:
				return segment, fmt.Errorf("%w: unknown archive object collection", durable.ErrCorrupt)
			}
		}
	}
	segment.Objects = nil
	return segment, nil
}
