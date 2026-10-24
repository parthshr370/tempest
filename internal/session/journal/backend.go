package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"go.harness.dev/harness/tempest/durable"
)

// CreateBackend creates a session in the caller's transaction domain. Path only
// locates host-local tool artifacts; transcript state stays in the backend.
func CreateBackend(ctx context.Context, backend durable.Store, dir, cwd string, opts Options) (*Store, error) {
	clock, newID := resolveOptions(opts)
	id := newID()
	release, err := acquireBackendSession(ctx, backend, id)
	if err != nil {
		return nil, err
	}
	owned := false
	defer func() {
		if !owned {
			release()
		}
	}()
	h := Header{Type: "session", Version: 4, ID: id, Timestamp: clock(), Cwd: cwd, Title: opts.Title}
	raw, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	receipt, err := backend.Commit(ctx, durable.Commit{ID: "create:" + id, Expected: []durable.RevisionCheck{{SessionID: id, Kind: durable.DocumentKind, Namespace: "_tempest/header", ID: "header"}}, Changes: []durable.Change{{SessionID: id, Kind: durable.DocumentKind, Namespace: "_tempest/header", ID: "header", Version: 1, Value: raw}}})
	if err != nil {
		return nil, err
	}
	owned = true
	return &Store{path: filepath.Join(dir, encodeCwd(cwd), id+".jsonl"), id: id, clock: clock, newID: newID, durable: backend, external: true, seq: receipt.Seq, releaseOwner: release}, nil
}

// OpenBackend loads an existing session without dispatching accepted work.
func OpenBackend(ctx context.Context, backend durable.Store, dir, id string, opts Options) (*Store, *Loaded, error) {
	clock, newID := resolveOptions(opts)
	header, err := backend.Read(ctx, durable.Query{SessionID: id, Kind: durable.DocumentKind, Namespace: "_tempest/header", ID: "header", Limit: 1})
	if err != nil {
		return nil, nil, err
	}
	if len(header.Records) == 0 {
		return nil, nil, ErrSessionNotFound
	}
	var h Header
	if err = json.Unmarshal(header.Records[0].Value, &h); err != nil {
		return nil, nil, err
	}
	release, err := acquireBackendSession(ctx, backend, id)
	if err != nil {
		return nil, nil, err
	}
	s := &Store{id: id, path: filepath.Join(dir, encodeCwd(h.Cwd), id+".jsonl"), clock: clock, newID: newID, durable: backend, external: true, releaseOwner: release}
	loaded, err := s.Load(ctx)
	if err != nil {
		release()
		return nil, nil, err
	}
	s.seq = loaded.lastSeq
	s.lastID = loaded.LastID
	s.usageSources = loaded.UsageSources
	return s, loaded, nil
}

// Load reduces the store's committed archive. It never executes handlers.
func (s *Store) Load(ctx context.Context) (*Loaded, error) {
	if !s.external {
		return Load(ctx, s.path)
	}
	header, err := s.Read(ctx, durable.Query{Kind: durable.DocumentKind, Namespace: "_tempest/header", ID: "header", Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(header.Records) == 0 {
		return nil, ErrSessionNotFound
	}
	var h Header
	if err = json.Unmarshal(header.Records[0].Value, &h); err != nil {
		return nil, err
	}
	if err = validateHeader(h); err != nil {
		return nil, err
	}
	original := h
	h.Version = 3
	raw, _ := json.Marshal(h)
	var transcript bytes.Buffer
	transcript.Write(raw)
	transcript.WriteByte('\n')
	q := durable.Query{Kind: durable.EntryKind, Namespace: "_tempest/journal", History: true, Limit: 1000, AsOf: header.Seq}
	for {
		page, err := s.Read(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, r := range page.Records {
			if _, err = decodeEntry(r.Value); err != nil {
				return nil, fmt.Errorf("backend journal: %w", err)
			}
			transcript.Write(r.Value)
			transcript.WriteByte('\n')
		}
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	results, err := readExtensionResults(ctx, s.durable, s.id, header.Seq)
	if err != nil {
		return nil, err
	}
	loaded, err := loadContentsResolved(ctx, s.path, transcript.Bytes(), true, results, func(ref ArchiveRef) (archiveSegment, error) {
		return s.readArchive(ctx, ref)
	})
	if err != nil {
		return nil, err
	}
	loaded.Header = original
	loaded.lastSeq = header.Seq
	return loaded, nil
}

// Backend reports an injected shared store. File journals own their own log.
func (s *Store) Backend() (durable.Store, bool) { return s.durable, s.external }

// OpenChildBackend preserves deterministic identity in the parent's domain.
func OpenChildBackend(ctx context.Context, backend durable.Store, parentPath, childID, cwd string, resume bool) (*Store, *Loaded, error) {
	dir := filepath.Dir(parentPath)
	if resume {
		release, err := acquireBackendSession(ctx, backend, childID)
		if err != nil {
			return nil, nil, err
		}
		clock, newID := resolveOptions(Options{})
		s := &Store{id: childID, path: filepath.Join(parentPath+".d", "children", childID, "session.jsonl"), clock: clock, newID: newID, durable: backend, external: true, releaseOwner: release}
		loaded, err := s.Load(ctx)
		if err == nil {
			s.seq, s.lastID, s.usageSources = loaded.lastSeq, loaded.LastID, loaded.UsageSources
			return s, loaded, nil
		}
		release()
		if !errors.Is(err, ErrSessionNotFound) {
			return nil, nil, err
		}
	}
	first := true
	s, err := CreateBackend(ctx, backend, dir, cwd, Options{NewID: func() string {
		if first {
			first = false
			return childID
		}
		return randomID()
	}})
	if err != nil {
		return nil, nil, err
	}
	s.path = filepath.Join(parentPath+".d", "children", childID, "session.jsonl")
	return s, nil, nil
}

func acquireBackendSession(ctx context.Context, backend durable.Store, id string) (func(), error) {
	leaser, ok := backend.(durable.SessionLeaser)
	if !ok {
		return nil, fmt.Errorf("%w: shared backend must implement durable.SessionLeaser", durable.ErrUnsupported)
	}
	release, err := leaser.AcquireSession(ctx, id)
	if errors.Is(err, durable.ErrLocked) {
		return nil, fmt.Errorf("%w: %s", ErrSessionLocked, id)
	}
	return release, err
}
