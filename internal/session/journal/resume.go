package journal

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/work"
)

const (
	compactionPrefix = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n"
	compactionSuffix = "\n</summary>"
)

// ErrSessionNotFound reports that no journal matches a requested session.
var ErrSessionNotFound = fmt.Errorf("session not found: %w", fs.ErrNotExist)

// Diagnostic records a nonfatal condition encountered while loading a journal.
type Diagnostic struct {
	Message string
}

// Loaded is the reconstructed runtime view of a journal: the reduction of its
// entries and records.
type Loaded struct {
	Header Header
	// Messages is the reconciled transcript: every tool call the provider
	// will see has exactly one result, synthesized when the journal holds no
	// real one.
	Messages []ptypes.Message
	Model    string
	Role     string
	LastID   string
	// Interrupted lists started-but-unfinished calls from LastRun, in start
	// order. Older unknown outcomes remain in Messages, not in this warning.
	Interrupted []InterruptedTool
	// LastRun is the most recent run_started, or nil when the journal has no
	// run records (version 1 files).
	LastRun *RunSummary
	// UsageTotal folds every usage record in the file.
	UsageTotal      ptypes.Usage
	Diagnostics     []Diagnostic
	Queued          []work.QueueItem
	QueueReceipts   []work.QueueItem
	QueueCancelled  map[string]bool
	QueueConsumed   map[string]bool
	Jobs            []work.Job
	Recoveries      []work.Recovery
	Runs            []RunDetail
	UsageSources    map[string]ptypes.Usage
	CompactionParts map[string]CompactionPart
	StopRequests    map[string]string
	HookRuns        map[string]HookState
	// EvalLanguages survives transcript compaction; process state doesn't.
	EvalLanguages map[string]bool
	snapshots     map[string][]ptypes.Message

	// lastSeq is where a resumed store continues numbering lines.
	lastSeq uint64
}

// replayMessage keeps each message's journal and run IDs around until
// compaction and reconciliation are done with them.
type replayMessage struct {
	id      string
	runID   string
	message ptypes.Message
	calls   []replayTool
}

// Load reads path fully and reduces its entries and records into a Loaded. A
// torn final line is dropped with a diagnostic; an undecodable line anywhere
// else is an error naming the line.
func Load(ctx context.Context, path string) (*Loaded, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("load journal: %w", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read journal: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("load journal: %w", err)
	}
	return loadContents(ctx, path, contents, true)
}

func loadContents(ctx context.Context, path string, contents []byte, withSnapshots bool) (*Loaded, error) {
	lines := bytes.Split(contents, []byte{'\n'})
	if len(lines) == 0 || len(lines[0]) == 0 {
		return nil, fmt.Errorf("read journal header: missing header")
	}
	var header Header
	if err := json.Unmarshal(lines[0], &header); err != nil {
		return nil, fmt.Errorf("decode journal header: %w", err)
	}
	if err := validateHeader(header); err != nil {
		return nil, err
	}

	loaded := &Loaded{Header: header}
	entries := make([]Entry, 0, len(lines))
	lastContent := len(lines) - 1
	if lastContent > 0 && len(lines[lastContent]) == 0 {
		lastContent--
	}
	var maxSeq uint64
	decoded := 0
	for index := 1; index <= lastContent; index++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("load journal: %w", err)
		}
		lineNumber := index + 1
		entry, err := decodeEntry(lines[index])
		if err != nil {
			var unknown unknownEntryError
			if errors.As(err, &unknown) {
				loaded.diagnose("line %d: %v skipped", lineNumber, unknown)
				if unknown.id != "" {
					loaded.LastID = unknown.id
				}
				maxSeq = max(maxSeq, unknown.seq)
				decoded++
				continue
			}
			if index == lastContent {
				loaded.diagnose("line %d: dropped torn final line: %v", lineNumber, err)
				break
			}
			return nil, fmt.Errorf("decode journal entry on line %d: %w", lineNumber, err)
		}
		decoded++
		meta := entry.meta()
		if meta.Seq != 0 {
			if meta.Seq <= maxSeq {
				loaded.diagnose("line %d: seq %d does not increase past %d", lineNumber, meta.Seq, maxSeq)
			}
			maxSeq = max(maxSeq, meta.Seq)
		}
		if meta.ID != "" {
			loaded.LastID = meta.ID
		}
		entries = append(entries, entry)
	}
	// Version 1 lines carry no seq; counting them keeps a resumed file's
	// numbering past every existing line.
	loaded.lastSeq = max(maxSeq, uint64(decoded))
	if err := loaded.reduceWork(ctx, entries, path, withSnapshots); err != nil {
		return nil, err
	}
	if !withSnapshots {
		return loaded, nil
	}
	reduce(loaded, entries)
	return loaded, nil
}

// OpenForResume exclusively locks path until Store.Close, loads it, and repairs
// a torn tail before appending. Load remains available to read-only callers.
func OpenForResume(ctx context.Context, path string, opts Options) (*Store, *Loaded, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("open journal for resume: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open journal for resume: %w", err)
	}
	if err := lockJournal(file); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	loaded, err := Load(ctx, path)
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if err := repairTail(ctx, file); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if err := upgradeHeader(file, loaded.Header.Version); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	loaded.Header.Version = JournalVersion
	if _, err := file.Seek(0, 2); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("open journal for resume: %w", err)
	}
	clock, newID := resolveOptions(opts)
	store := &Store{
		path:         path,
		id:           loaded.Header.ID,
		file:         file,
		lastID:       loaded.LastID,
		seq:          loaded.lastSeq,
		clock:        clock,
		newID:        newID,
		usageSources: maps.Clone(loaded.UsageSources),
	}
	return store, loaded, nil
}

// repairTail makes the file safe to append to. A torn final line (one that
// does not decode) is truncated away; a complete final line that lost only
// its newline gets the newline back, so the next append is not glued onto it.
// Earlier corruption is left in place for Load to report.
func repairTail(ctx context.Context, file *os.File) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("repair journal tail: %w", err)
	}
	contents, err := os.ReadFile(file.Name())
	if err != nil {
		return fmt.Errorf("read journal tail: %w", err)
	}
	if len(contents) == 0 {
		return nil
	}
	end := len(contents)
	terminated := contents[end-1] == '\n'
	if terminated {
		end--
	}
	start := bytes.LastIndexByte(contents[:end], '\n') + 1
	if start == 0 {
		// Only the header, which Load already validated.
		if terminated {
			return nil
		}
		return appendNewline(file)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("repair journal tail: %w", err)
	}
	_, err = decodeEntry(contents[start:end])
	var unknown unknownEntryError
	if err == nil || errors.As(err, &unknown) {
		if terminated {
			return nil
		}
		return appendNewline(file)
	}
	if err := file.Truncate(int64(start)); err != nil {
		return fmt.Errorf("truncate torn journal tail: %w", err)
	}
	return nil
}

func appendNewline(file *os.File) error {
	if _, err := file.Seek(0, 2); err != nil {
		return fmt.Errorf("seek journal tail: %w", err)
	}
	if _, err := file.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("repair journal tail: %w", err)
	}
	return nil
}

func (l *Loaded) diagnose(format string, args ...any) {
	l.Diagnostics = append(l.Diagnostics, Diagnostic{Message: fmt.Sprintf(format, args...)})
}

// Resolve accepts a session id or journal filename under dir and cwd.
func Resolve(ctx context.Context, dir, cwd, id string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("resolve journal: %w", err)
	}
	journalDir := filepath.Join(dir, encodeCwd(cwd))
	entries, err := os.ReadDir(journalDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("resolve journal %q: %w", id, ErrSessionNotFound)
		}
		return "", fmt.Errorf("read journal directory: %w", err)
	}
	suffix := "_" + id + ".jsonl"
	filename := strings.TrimSuffix(id, ".jsonl") + ".jsonl"
	var found, foundName string
	var corruptErr error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("resolve journal: %w", err)
		}
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), suffix) && entry.Name() != filename) {
			continue
		}
		path := filepath.Join(journalDir, entry.Name())
		header, err := loadHeader(ctx, path)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", fmt.Errorf("resolve journal: %w", ctxErr)
			}
			corruptErr = fmt.Errorf("inspect journal %q: %w", entry.Name(), err)
			continue
		}
		if header.Cwd != cwd || (entry.Name() != filename && header.ID != id) {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("ambiguous session %q: pass -resume %s or -resume %s", id, foundName, entry.Name())
		}
		found, foundName = path, entry.Name()
	}
	if found != "" {
		return found, nil
	}
	if corruptErr != nil {
		return "", corruptErr
	}
	return "", fmt.Errorf("resolve journal %q: %w", id, ErrSessionNotFound)
}

// Recent returns the most-recently-modified journal path for cwd under dir.
func Recent(ctx context.Context, dir, cwd string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("recent journal: %w", err)
	}
	journalDir := filepath.Join(dir, encodeCwd(cwd))
	entries, err := os.ReadDir(journalDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("recent journal: %w", ErrSessionNotFound)
		}
		return "", fmt.Errorf("read journal directory: %w", err)
	}
	var recentPath string
	var recentTime time.Time
	var corruptErr error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("recent journal: %w", err)
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(journalDir, entry.Name())
		header, err := loadHeader(ctx, path)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", fmt.Errorf("recent journal: %w", ctxErr)
			}
			corruptErr = fmt.Errorf("inspect journal %q: %w", entry.Name(), err)
			continue
		}
		if header.Cwd != cwd {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			corruptErr = fmt.Errorf("inspect journal %q: %w", entry.Name(), err)
			continue
		}
		if recentPath == "" || info.ModTime().After(recentTime) || (info.ModTime().Equal(recentTime) && path > recentPath) {
			recentPath = path
			recentTime = info.ModTime()
		}
	}
	if recentPath == "" {
		if corruptErr != nil {
			return "", corruptErr
		}
		return "", fmt.Errorf("recent journal: %w", ErrSessionNotFound)
	}
	return recentPath, nil
}

// validateHeader keeps discovery from treating an unrelated or incompatible
// JSONL file as a resumable session.
func validateHeader(header Header) error {
	if header.Type != "session" {
		return fmt.Errorf("decode journal header: unexpected type %q", header.Type)
	}
	if header.Version < 1 || header.Version > JournalVersion {
		return fmt.Errorf("decode journal header: unsupported version %d", header.Version)
	}
	if len(header.ID) != 16 {
		return fmt.Errorf("decode journal header: invalid id %q", header.ID)
	}
	if _, err := hex.DecodeString(header.ID); err != nil {
		return fmt.Errorf("decode journal header: invalid id %q: %w", header.ID, err)
	}
	if header.Timestamp.IsZero() {
		return fmt.Errorf("decode journal header: missing timestamp")
	}
	if header.Cwd == "" || !filepath.IsAbs(header.Cwd) {
		return fmt.Errorf("decode journal header: cwd must be absolute")
	}
	return nil
}

// loadHeader lets [Resolve] and [Recent] inspect candidate journals without
// parsing their full transcripts.
func loadHeader(ctx context.Context, path string) (Header, error) {
	if err := ctx.Err(); err != nil {
		return Header{}, fmt.Errorf("load journal header: %w", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return Header{}, fmt.Errorf("read journal header: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Header{}, fmt.Errorf("load journal header: %w", err)
	}
	line, _, _ := bytes.Cut(contents, []byte{'\n'})
	var header Header
	if err := json.Unmarshal(line, &header); err != nil {
		return Header{}, fmt.Errorf("decode journal header: %w", err)
	}
	if err := validateHeader(header); err != nil {
		return Header{}, err
	}
	return header, nil
}

// applyCompaction rebuilds the replayable history from a summary and its first
// retained message, even when an old journal lacks that message ID.
func applyCompaction(messages []replayMessage, entry CompactionEntry) []replayMessage {
	start := -1
	for index := range messages {
		if messages[index].id == entry.FirstKeptID {
			start = index
			break
		}
	}
	kept := messages
	if start >= 0 {
		kept = messages[start:]
	}
	summary := compactionPrefix + entry.Summary + compactionSuffix
	if len(kept) > 0 {
		switch message := kept[0].message.(type) {
		case ptypes.UserMessage:
			kept[0].message = mergeSummaryIntoUser(summary, message, entry.Timestamp)
			return kept
		case *ptypes.UserMessage:
			if message != nil {
				kept[0].message = mergeSummaryIntoUser(summary, *message, entry.Timestamp)
				return kept
			}
		}
	}
	summaryMessage := ptypes.UserMessage{
		Content:   ptypes.StringContent(summary),
		Timestamp: entry.Timestamp.UnixMilli(),
	}
	return append([]replayMessage{{message: summaryMessage}}, kept...)
}

// mergeSummaryIntoUser keeps the summary at the front of the retained user turn
// so replay doesn't add a synthetic turn before its original context.
func mergeSummaryIntoUser(summary string, user ptypes.UserMessage, timestamp time.Time) ptypes.UserMessage {
	blocks := make([]ptypes.ContentBlock, 0, len(user.Content.Blocks)+2)
	blocks = append(blocks, ptypes.NewText(summary))
	if user.Content.IsBlocks() {
		blocks = append(blocks, user.Content.Blocks...)
	} else if strings.TrimSpace(user.Content.Text) != "" {
		blocks = append(blocks, ptypes.NewText(user.Content.Text))
	}
	if user.Timestamp == 0 {
		user.Timestamp = timestamp.UnixMilli()
	}
	user.Content = ptypes.BlockContent(blocks...)
	return user
}
