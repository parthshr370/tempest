package journal

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	ptypes "go.harness.dev/harness/internal/engine/types"
)

// ErrSessionLocked reports that another writer already owns the journal.
var ErrSessionLocked = errors.New("session is locked by another writer")

// Options configures a new or resumed journal.
type Options struct {
	Clock func() time.Time
	NewID func() string
	Title string
}

// Store is an append-only session journal writer. It is safe for concurrent
// use; appends are serialized and each line is written with one write call.
type Store struct {
	path   string
	id     string
	file   *os.File
	mu     sync.Mutex
	lastID string
	seq    uint64
	// failed is the first write or sync error. A failed write may have left a
	// partial line, so the store refuses further appends rather than splice
	// valid lines after it.
	failed       error
	clock        func() time.Time
	newID        func() string
	usageSources map[string]ptypes.Usage
}

// Create opens a new journal for cwd under dir and writes its header.
func Create(ctx context.Context, dir, cwd string, opts Options) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("create journal: %w", err)
	}
	clock, newID := resolveOptions(opts)
	timestamp := clock()
	id := newID()
	journalDir := filepath.Join(dir, encodeCwd(cwd))
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("create journal: %w", err)
	}
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		return nil, fmt.Errorf("create journal directory: %w", err)
	}
	path := filepath.Join(journalDir, timestamp.UTC().Format("20060102T150405Z")+"_"+id+".jsonl")
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("create journal: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create journal: %w", err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(path)
	}
	if err := lockJournal(file); err != nil {
		// Don't unlink an inode another opener may already have locked.
		_ = file.Close()
		return nil, err
	}
	header, err := json.Marshal(Header{
		Type:      "session",
		Version:   JournalVersion,
		ID:        id,
		Timestamp: timestamp,
		Cwd:       cwd,
		Title:     opts.Title,
	})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("marshal journal header: %w", err)
	}
	if err := ctx.Err(); err != nil {
		cleanup()
		return nil, fmt.Errorf("create journal: %w", err)
	}
	if err := writeLine(file, header); err != nil {
		cleanup()
		return nil, fmt.Errorf("write journal header: %w", err)
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return nil, fmt.Errorf("sync journal header: %w", err)
	}
	if err := syncDirectory(journalDir); err != nil {
		cleanup()
		return nil, fmt.Errorf("sync journal directory: %w", err)
	}
	return &Store{path: path, id: id, file: file, clock: clock, newID: newID}, nil
}

// ID returns the session id.
func (s *Store) ID() string { return s.id }

// Path returns the journal file path.
func (s *Store) Path() string { return s.path }

// NewRunID returns a fresh run id from the store's id generator.
func (s *Store) NewRunID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newID()
}

// AppendMessage writes a message entry outside any run.
func (s *Store) AppendMessage(ctx context.Context, message ptypes.Message) error {
	return s.Append(ctx, MessageEntry{Message: message})
}

// AppendModelChange writes a model-change entry.
func (s *Store) AppendModelChange(ctx context.Context, model, role string) error {
	return s.Append(ctx, ModelChangeEntry{Model: model, Role: role})
}

// AppendCompaction writes a compaction entry.
func (s *Store) AppendCompaction(ctx context.Context, summary, firstKeptID string, tokensBefore int) error {
	return s.Append(ctx, CompactionEntry{
		Summary:      summary,
		FirstKeptID:  firstKeptID,
		TokensBefore: tokensBefore,
	})
}

// Append writes one entry or record. The caller sets Meta.RunID and
// Meta.EventSeq; the store assigns Seq, Timestamp, Lane and, for entries, ID
// and ParentID. Records named by the flush policy are fsynced before Append
// returns. After any write or sync error the store refuses further appends.
func (s *Store) Append(ctx context.Context, entry Entry) error {
	_, err := s.AppendAssigned(ctx, entry)
	return err
}

// AppendAssigned commits an entry and returns the exact durable envelope.
func (s *Store) AppendAssigned(ctx context.Context, entry Entry) (Meta, error) {
	if entry == nil {
		return Meta{}, fmt.Errorf("append journal entry: nil entry")
	}
	if err := ctx.Err(); err != nil {
		return Meta{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Meta{}, err
	}
	if s.file == nil {
		return Meta{}, fmt.Errorf("append journal entry: store closed")
	}
	if s.failed != nil {
		return Meta{}, fmt.Errorf("journal failed earlier: %w", s.failed)
	}
	kind, given := entry.kind(), entry.meta()
	meta := Meta{Timestamp: s.clock(), Seq: s.seq + 1, Lane: LaneMain, RunID: given.RunID, EventSeq: given.EventSeq}
	if !isRecord(kind) {
		meta.ID = s.newID()
		meta.ParentID = s.lastID
	}
	if e, ok := entry.(MessageEntry); ok && e.Metered {
		for i, b := range assistantBlocks(e.Message) {
			if b.Type == ptypes.BlockToolCall {
				e.CallOccurrences = append(e.CallOccurrences, fmt.Sprintf("%s:%d", meta.ID, i))
			}
		}
		entry = e
	}
	line, err := MarshalEntry(entry.withMeta(meta))
	if err != nil {
		return Meta{}, fmt.Errorf("marshal journal entry: %w", err)
	}
	if err := writeLine(s.file, line); err != nil {
		s.failed = err
		return Meta{}, fmt.Errorf("write journal entry: %w", err)
	}
	if syncsAfter(kind) {
		if err := s.file.Sync(); err != nil {
			s.failed = err
			return Meta{}, fmt.Errorf("sync journal after %s: %w", kind, err)
		}
	}
	s.rememberUsage(entry, meta)
	s.seq = meta.Seq
	if meta.ID != "" {
		s.lastID = meta.ID
	}
	return meta, nil
}

// Close flushes and closes the journal, releasing its exclusive writer lock.
// It is safe to call more than once.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	file := s.file
	s.file = nil
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return fmt.Errorf("sync journal: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close journal: %w", closeErr)
	}
	return nil
}

func lockJournal(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("%w: %s", ErrSessionLocked, file.Name())
		}
		return fmt.Errorf("lock journal %s: %w", file.Name(), err)
	}
	return nil
}

func resolveOptions(opts Options) (func() time.Time, func() string) {
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	newID := opts.NewID
	if newID == nil {
		newID = randomID
	}
	return clock, newID
}

// randomID falls back to the current nanosecond when entropy is unavailable, so
// a transient random-source failure doesn't prevent journaling.
func randomID() string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err == nil && bytes != [8]byte{} {
		return hex.EncodeToString(bytes[:])
	}
	value := uint64(time.Now().UnixNano())
	if value == 0 {
		value = 1
	}
	binary.BigEndian.PutUint64(bytes[:], value)
	return hex.EncodeToString(bytes[:])
}

// encodeCwd groups journals under a readable workspace slug. Header.Cwd remains
// authoritative because different paths can share a slug.
func encodeCwd(cwd string) string {
	encoded := strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(cwd)
	encoded = strings.TrimLeft(encoded, "-")
	if encoded == "" {
		return "root"
	}
	return encoded
}

// writeLine makes a short write an append failure, so callers don't advance the
// in-memory parent ID after an incomplete JSONL record.
func writeLine(file *os.File, line []byte) error {
	buffer := make([]byte, len(line)+1)
	copy(buffer, line)
	buffer[len(line)] = '\n'
	written, err := file.Write(buffer)
	if err != nil {
		return err
	}
	if written != len(buffer) {
		return io.ErrShortWrite
	}
	return nil
}
