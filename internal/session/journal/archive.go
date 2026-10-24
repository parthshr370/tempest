package journal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"go.harness.dev/harness/internal/conversation"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/tempest/durable"
)

const archiveNamespace = "_tempest/archive"
const archiveSession = "_tempest/archives"

// ArchiveRef pins immutable bytes outside any individual session directory.
type ArchiveRef struct {
	Digest   string `json:"digest"`
	ParentID string `json:"parentId"`
	EntryID  string `json:"entryId"`
}

type archiveSegment struct {
	Header   Header              `json:"header"`
	Records  []json.RawMessage   `json:"records"`
	Messages []json.RawMessage   `json:"messages"`
	Frames   []durable.Record    `json:"frames,omitempty"`
	Cuts     []uint64            `json:"cuts,omitempty"`
	Owners   []string            `json:"owners,omitempty"`
	State    []durable.Record    `json:"state,omitempty"`
	Objects  map[string][]string `json:"objects,omitempty"`
}

// ForkSeed contains only transcript, agent choices and policy-selected documents.
// Lifetime permissions, spend, queues, tasks and provider IDs never enter it.
type ForkSeed struct {
	Prefix      ArchiveRef
	Agent       json.RawMessage
	Documents   []durable.Change
	Definitions []durable.DocumentDefinition
	Messages    []types.Message
	Model       string
	Cwd         string
}

func archivePath(journalPath, digest string) (string, error) {
	if !validDigest(digest) {
		return "", fmt.Errorf("%w: invalid archive digest", durable.ErrCorrupt)
	}
	return filepath.Join(filepath.Dir(filepath.Dir(journalPath)), "archive", digest+".json"), nil
}

func decodeArchive(raw []byte, reference ArchiveRef) (archiveSegment, error) {
	var segment archiveSegment
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != reference.Digest {
		return segment, fmt.Errorf("%w: archive checksum", durable.ErrCorrupt)
	}
	if err := json.Unmarshal(raw, &segment); err != nil {
		return segment, fmt.Errorf("%w: archive: %v", durable.ErrCorrupt, err)
	}
	if segment.Header.ID != reference.ParentID {
		return segment, fmt.Errorf("%w: archive parent", durable.ErrCorrupt)
	}
	return segment, nil
}

func readFileArchive(journalPath string, reference ArchiveRef) (archiveSegment, error) {
	path, err := archivePath(journalPath, reference.Digest)
	if err != nil {
		return archiveSegment{}, err
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return archiveSegment{}, fmt.Errorf("%w: archive missing: %v", durable.ErrCorrupt, err)
	}
	defer root.Close()
	raw, err := root.ReadFile(filepath.Base(path))
	if err != nil {
		return archiveSegment{}, fmt.Errorf("%w: archive missing: %v", durable.ErrCorrupt, err)
	}
	segment, err := decodeArchive(raw, reference)
	if err != nil {
		return segment, err
	}
	return hydrateArchive(segment, func(digest string) ([]byte, error) {
		return readFileArchiveObject(journalPath, digest)
	})
}
func (s *Store) readArchive(ctx context.Context, reference ArchiveRef) (archiveSegment, error) {
	if !s.external {
		return readFileArchive(s.path, reference)
	}
	page, err := s.durable.Read(ctx, durable.Query{SessionID: archiveSession, Kind: durable.EntryKind, Namespace: archiveNamespace, ID: reference.Digest, Limit: 1})
	if err != nil {
		return archiveSegment{}, err
	}
	if len(page.Records) != 1 {
		return archiveSegment{}, fmt.Errorf("%w: archive missing", durable.ErrCorrupt)
	}
	segment, err := decodeArchive(page.Records[0].Value, reference)
	if err != nil {
		return segment, err
	}
	return hydrateArchive(segment, func(digest string) ([]byte, error) {
		return s.readArchiveObject(ctx, digest)
	})
}

func (s *Store) sealArchive(ctx context.Context, segment archiveSegment, entryID string) (ArchiveRef, error) {
	var err error
	segment, err = packArchive(segment, func(raw []byte) (string, error) {
		return s.writeArchiveObject(ctx, raw)
	})
	if err != nil {
		return ArchiveRef{}, err
	}
	raw, err := json.Marshal(segment)
	if err != nil {
		return ArchiveRef{}, err
	}
	digest, err := s.writeArchiveObject(ctx, raw)
	return ArchiveRef{Digest: digest, ParentID: s.id, EntryID: entryID}, err
}

func (s *Store) writeArchiveObject(ctx context.Context, raw []byte) (string, error) {
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	if s.external {
		_, err := s.durable.Commit(ctx, durable.Commit{ID: "archive:" + digest, Changes: []durable.Change{{SessionID: archiveSession, Kind: durable.EntryKind, Namespace: archiveNamespace, ID: digest, Version: 1, Value: raw}}})
		return digest, err
	}
	path, err := archivePath(s.path, digest)
	if err != nil {
		return digest, err
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return digest, err
	}
	if prior, err := os.ReadFile(path); err == nil {
		if !bytes.Equal(prior, raw) {
			return digest, fmt.Errorf("%w: archive collision", durable.ErrCorrupt)
		}
		return digest, nil
	} else if !os.IsNotExist(err) {
		return digest, err
	}
	file, err := os.CreateTemp(dir, ".archive-")
	if err != nil {
		return digest, err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(raw)
	err = errors.Join(err, file.Sync(), file.Close())
	if err == nil {
		err = os.Rename(file.Name(), path)
	}
	if err == nil {
		err = syncDirectory(dir)
	}
	if err == nil {
		err = syncDirectory(filepath.Dir(dir))
	}
	return digest, err
}

// CaptureFork snapshots one safe cut under the journal's commit lock.
// Archive writes and syncs happen after releasing the parent writer.
func (s *Store) CaptureFork(ctx context.Context, entryID string, preceding bool) (*ForkSeed, error) {
	s.mu.Lock()
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()
	if s.closed {
		return nil, durable.ErrClosed
	}
	records, err := s.readAll(ctx, durable.Query{SessionID: s.id, Kind: durable.EntryKind, Namespace: "_tempest/journal", History: true})
	if err != nil {
		return nil, err
	}
	var inherited *archiveSegment
	local := slices.ContainsFunc(records, func(record durable.Record) bool { return record.ID == entryID })
	for _, record := range records {
		if local {
			break
		}
		entry, err := decodeEntry(record.Value)
		if err != nil {
			return nil, err
		}
		if head, ok := entry.(ContextHeadEntry); ok && head.Prefix != nil {
			segment, err := s.archiveCut(ctx, *head.Prefix, entryID)
			if err != nil {
				return nil, err
			}
			if segment != nil {
				inherited = segment
				break
			}
		}
	}
	localRecords := records
	if inherited != nil {
		records = make([]durable.Record, 0, len(inherited.Records))
		for index, raw := range inherited.Records {
			seq := uint64(index + 1)
			if index < len(inherited.Cuts) {
				seq = inherited.Cuts[index]
			}
			owner := inherited.Header.ID
			if index < len(inherited.Owners) {
				owner = inherited.Owners[index]
			}
			records = append(records, durable.Record{Change: durable.Change{SessionID: owner, Value: raw}, Seq: seq})
		}
	}
	var cut uint64
	end, lastSafe := -1, -1
	var boundary conversation.Boundary
	for index, record := range records {
		entry, err := decodeEntry(record.Value)
		if err != nil {
			return nil, err
		}
		safe := true
		switch value := entry.(type) {
		case RunStartedRecord:
			boundary = conversation.Boundary{}
		case MessageEntry:
			if value.Message.Role() == "assistant" {
				boundary = conversation.Boundary{}
			}
			safe = boundary.Add(value.Message)
		case ContextHeadEntry, RewindEntry, CompactionEntry:
			boundary = conversation.Boundary{}
		default:
			continue
		}
		if safe {
			lastSafe = index
		}
		if entry.meta().ID == entryID {
			end = index
			if !safe {
				if !preceding || lastSafe < 0 {
					return nil, fmt.Errorf("fork entry interrupts a tool round")
				}
				end = lastSafe
			}
			cut = records[end].Seq
			break
		}
	}
	if end < 0 {
		return nil, fmt.Errorf("fork entry %q not found", entryID)
	}
	readAt := func(query durable.Query) ([]durable.Record, error) {
		if inherited == nil || query.AsOf == 0 {
			return s.readAll(ctx, query)
		}
		var selected []durable.Record
		latest := make(map[string]durable.Record)
		for _, record := range inherited.State {
			if record.Seq <= query.AsOf && record.Kind == query.Kind && record.Namespace == query.Namespace && (query.ID == "" || record.ID == query.ID) {
				latest[record.ID] = record
			}
		}
		for _, record := range latest {
			if !record.Delete {
				selected = append(selected, record)
			}
		}
		return selected, nil
	}
	// Snapshot document choices and transcript at the same immutable store cut.
	definitionCut := cut
	if inherited != nil {
		definitionCut = localRecords[0].Seq
	}
	definitions, err := s.documentDefinitions(ctx, definitionCut)
	if err != nil {
		return nil, err
	}
	seed := &ForkSeed{Definitions: definitions}
	for _, definition := range definitions {
		asOf := cut
		if definition.Fork == durable.ForkCurrent {
			asOf = 0
		}
		var selected []durable.Record
		if definition.Fork != durable.ForkInitial {
			selected, err = readAt(durable.Query{SessionID: s.id, Kind: durable.DocumentKind, Namespace: definition.Namespace, AsOf: asOf})
			if err != nil {
				return nil, err
			}
		} else {
			current, err := s.readAll(ctx, durable.Query{SessionID: s.id, Kind: durable.DocumentKind, Namespace: definition.Namespace})
			if err != nil {
				return nil, err
			}
			for _, record := range current {
				selected = append(selected, durable.Record{Change: durable.Change{Kind: durable.DocumentKind, Namespace: definition.Namespace, ID: record.ID, Version: definition.Version, Value: definition.Initial}})
			}
			if len(selected) == 0 {
				selected = append(selected, durable.Record{Change: durable.Change{Kind: durable.DocumentKind, Namespace: definition.Namespace, ID: "default", Version: definition.Version, Value: definition.Initial}})
			}
		}
		hasDefault := false
		for _, record := range selected {
			if record.ID == "default" {
				hasDefault = true
			}
		}
		if !hasDefault {
			seed.Documents = append(seed.Documents, durable.Change{Kind: durable.DocumentKind, Namespace: definition.Namespace, ID: "default", Version: definition.Version, Delete: true})
		}
		for _, record := range selected {
			change := record.Change
			change.SessionID = ""
			seed.Documents = append(seed.Documents, change)
		}
	}
	agent, err := readAt(durable.Query{SessionID: s.id, Kind: durable.DocumentKind, Namespace: "_tempest/agent", ID: "selection", AsOf: cut, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(agent) != 1 {
		return nil, fmt.Errorf("agent selection unavailable at fork cut")
	}
	seed.Agent = agent[0].Value
	header := Header{Type: "session", Version: 3, ID: s.id, Timestamp: s.clock(), Cwd: filepath.Dir(s.path)}
	// The real header is needed for cwd and stable segment content.
	if s.external {
		page, err := s.durable.Read(ctx, durable.Query{SessionID: s.id, Kind: durable.DocumentKind, Namespace: "_tempest/header", ID: "header", Limit: 1})
		if err != nil {
			return nil, err
		}
		if len(page.Records) != 1 {
			return nil, ErrSessionNotFound
		}
		if err := json.Unmarshal(page.Records[0].Value, &header); err != nil {
			return nil, err
		}
	} else {
		header, err = loadHeader(ctx, s.path)
		if err != nil {
			return nil, err
		}
	}
	if inherited != nil {
		header.Cwd = inherited.Header.Cwd
	}
	header.Version = 3
	raw, _ := json.Marshal(header)
	var transcript bytes.Buffer
	transcript.Write(raw)
	transcript.WriteByte('\n')
	segment := archiveSegment{Header: header}
	if inherited != nil {
		segment.State = inherited.State
	} else {
		for _, namespace := range append([]string{"_tempest/agent"}, definitionNamespaces(definitions)...) {
			state, err := s.readAll(ctx, durable.Query{SessionID: s.id, Kind: durable.DocumentKind, Namespace: namespace, AsOf: cut, History: true})
			if err != nil {
				return nil, err
			}
			segment.State = append(segment.State, state...)
		}
	}
	if inherited == nil {
		segment.Frames, err = s.readAll(ctx, durable.Query{SessionID: s.id, Kind: durable.EntryKind, Namespace: "_tempest/requests", AsOf: cut})
		if err != nil {
			return nil, err
		}
		prompts, readErr := s.readAll(ctx, durable.Query{SessionID: s.id, Kind: durable.EntryKind, Namespace: "_tempest/request-prompts", AsOf: cut})
		if readErr != nil {
			return nil, readErr
		}
		segment.Frames = append(segment.Frames, prompts...)
	} else {
		cuts := make(map[string]uint64)
		for _, record := range records[:end+1] {
			cuts[record.SessionID] = max(cuts[record.SessionID], record.Seq)
		}
		for _, frame := range inherited.Frames {
			if frame.Seq <= cuts[frame.SessionID] {
				segment.Frames = append(segment.Frames, frame)
			}
		}
	}
	for _, record := range records[:end+1] {
		entry, err := decodeEntry(record.Value)
		if err != nil {
			return nil, err
		}
		if head, ok := entry.(ContextHeadEntry); inherited == nil && ok && head.Prefix != nil {
			inherited, err := s.readArchive(ctx, *head.Prefix)
			if err != nil {
				return nil, err
			}
			segment.Records = append(segment.Records, inherited.Records...)
			segment.Frames = append(segment.Frames, inherited.Frames...)
			segment.Cuts = append(segment.Cuts, inherited.Cuts...)
			for index := range inherited.Records {
				owner := inherited.Header.ID
				if index < len(inherited.Owners) {
					owner = inherited.Owners[index]
				}
				segment.Owners = append(segment.Owners, owner)
			}
		}
		segment.Records = append(segment.Records, record.Value)
		segment.Cuts = append(segment.Cuts, record.Seq)
		segment.Owners = append(segment.Owners, record.SessionID)
		transcript.Write(record.Value)
		transcript.WriteByte('\n')
	}
	loaded, err := loadContentsResolved(ctx, s.path, transcript.Bytes(), true, nil, func(ref ArchiveRef) (archiveSegment, error) { return s.readArchive(ctx, ref) })
	if err != nil {
		return nil, err
	}
	if err := conversation.Safe(loaded.Messages); err != nil {
		return nil, err
	}
	segment.Messages, err = rawMessages(loaded.Messages)
	if err != nil {
		return nil, err
	}
	selected, err := decodeEntry(records[end].Value)
	if err != nil {
		return nil, err
	}
	s.mu.Unlock()
	locked = false
	seed.Prefix, err = s.sealArchive(ctx, segment, selected.meta().ID)
	if err != nil {
		return nil, err
	}
	seed.Messages, seed.Model, seed.Cwd = loaded.Messages, loaded.Model, header.Cwd
	return seed, nil
}

func definitionNamespaces(definitions []durable.DocumentDefinition) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Namespace)
	}
	return names
}

func (s *Store) archiveCut(ctx context.Context, ref ArchiveRef, id string) (*archiveSegment, error) {
	segment, err := s.readArchive(ctx, ref)
	if err != nil {
		return nil, err
	}
	for _, raw := range segment.Records {
		entry, err := decodeEntry(raw)
		if err != nil {
			return nil, err
		}
		if head, ok := entry.(ContextHeadEntry); ok && head.Prefix != nil {
			found, err := s.archiveCut(ctx, *head.Prefix, id)
			if found != nil || err != nil {
				return found, err
			}
		}
	}
	for _, raw := range segment.Records {
		entry, err := decodeEntry(raw)
		if err != nil {
			return nil, err
		}
		if entry.meta().ID == id {
			return &segment, nil
		}
	}
	return nil, nil
}

// InitializeFork commits the pin, configuration and initial documents together.
// The session isn't catalog-visible until normal initialization completes.
func (s *Store) InitializeFork(ctx context.Context, seed *ForkSeed) error {
	if err := s.RegisterDocuments(ctx, seed.Definitions); err != nil {
		return err
	}
	meta := Meta{ID: s.NewRunID(), Timestamp: s.clock(), Lane: LaneMain, Seq: s.Position().Seq + 1}
	entry := ContextHeadEntry{Meta: meta, Reason: "fork", Prefix: &seed.Prefix}
	raw, err := MarshalEntry(entry)
	if err != nil {
		return err
	}
	epoch, _ := json.Marshal(meta.ID)
	changes := []durable.Change{
		{SessionID: s.id, Kind: durable.EntryKind, Namespace: "_tempest/journal", ID: meta.ID, Version: 1, Value: raw},
		{SessionID: s.id, Kind: durable.DocumentKind, Namespace: "_tempest/context", ID: "head", Version: 1, Value: epoch},
		{SessionID: s.id, Kind: durable.DocumentKind, Namespace: "_tempest/agent", ID: "selection", Version: 1, Value: seed.Agent},
		{SessionID: s.id, Kind: durable.ChildKind, Namespace: "_tempest/forks", ID: "creation", Version: 1, Value: raw},
	}
	for _, change := range seed.Documents {
		change.SessionID = s.id
		changes = append(changes, change)
	}
	_, err = s.Commit(ctx, durable.Commit{ID: "fork:initialize", Changes: changes})
	return err
}

func (s *Store) pinnedRecord(ctx context.Context, namespace, id string, cut uint64) (*durable.Record, error) {
	page, err := s.durable.Read(ctx, durable.Query{SessionID: s.id, Kind: durable.ChildKind, Namespace: "_tempest/forks", ID: "creation", AsOf: cut, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(page.Records) == 0 {
		return nil, nil
	}
	entry, err := decodeEntry(page.Records[0].Value)
	if err != nil {
		return nil, err
	}
	head, ok := entry.(ContextHeadEntry)
	if !ok || head.Prefix == nil {
		return nil, fmt.Errorf("%w: invalid fork creation receipt", durable.ErrCorrupt)
	}
	segment, err := s.readArchive(ctx, *head.Prefix)
	if err != nil {
		return nil, err
	}
	record := durable.Record{Change: durable.Change{SessionID: s.id, Kind: durable.EntryKind, Namespace: namespace, ID: id, Version: 1}, Revision: 1, Seq: page.Records[0].Seq, CommitID: page.Records[0].CommitID}
	if namespace == "_tempest/requests" {
		for _, frame := range segment.Frames {
			if frame.ID == id {
				record.Value = frame.Value
				return &record, nil
			}
		}
		return nil, nil
	}
	for _, raw := range segment.Records {
		entry, err := decodeEntry(raw)
		if err != nil {
			return nil, err
		}
		if entry.meta().ID == id {
			record.Value = raw
			return &record, nil
		}
	}
	return nil, nil
}
