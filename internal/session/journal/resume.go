package journal

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	ptypes "go.harness.dev/harness/internal/engine/types"
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

// Loaded is the reconstructed runtime view of a journal.
type Loaded struct {
	Header      Header
	Messages    []ptypes.Message
	Model       string
	Role        string
	LastID      string
	Diagnostics []Diagnostic
}

// replayMessage keeps each message's journal ID around until compaction has
// found the retained-history cut point.
type replayMessage struct {
	id      string
	message ptypes.Message
}

// Load reads path fully and reconstructs its runtime context.
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
	messages := make([]replayMessage, 0)
	lastContent := len(lines) - 1
	if lastContent > 0 && len(lines[lastContent]) == 0 {
		lastContent--
	}
	for index := 1; index <= lastContent; index++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("load journal: %w", err)
		}
		line := lines[index]
		entry, err := decodeEntry(line)
		if err != nil {
			var unknown unknownEntryError
			if errors.As(err, &unknown) {
				loaded.Diagnostics = append(loaded.Diagnostics, Diagnostic{Message: unknown.Error()})
				loaded.LastID = unknown.id
				continue
			}
			if index == lastContent {
				break
			}
			return nil, fmt.Errorf("decode journal entry on line %d: %w", index+1, err)
		}
		loaded.LastID = entry.meta().ID
		switch value := entry.(type) {
		case MessageEntry:
			messages = append(messages, replayMessage{id: value.ID, message: value.Message})
		case ModelChangeEntry:
			loaded.Model = value.Model
			loaded.Role = value.Role
			if loaded.Role == "" {
				loaded.Role = "default"
			}
		case CompactionEntry:
			messages = applyCompaction(messages, value)
		}
	}
	loaded.Messages = make([]ptypes.Message, len(messages))
	for index := range messages {
		loaded.Messages[index] = messages[index].message
	}
	return loaded, nil
}

// OpenForResume loads path and opens it for appending after its last entry.
func OpenForResume(ctx context.Context, path string, opts Options) (*Store, *Loaded, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("open journal for resume: %w", err)
	}
	loaded, err := Load(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	if err := trimMalformedFinalLine(ctx, path); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("open journal for resume: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open journal for resume: %w", err)
	}
	clock, newID := resolveOptions(opts)
	store := &Store{
		path:   path,
		id:     loaded.Header.ID,
		file:   file,
		lastID: loaded.LastID,
		clock:  clock,
		newID:  newID,
	}
	return store, loaded, nil
}

// trimMalformedFinalLine drops only a torn final write before we append again;
// earlier corruption stays visible instead of being silently erased.
func trimMalformedFinalLine(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("trim malformed journal tail: %w", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read journal tail: %w", err)
	}
	if len(contents) == 0 {
		return nil
	}
	end := len(contents)
	if contents[end-1] == '\n' {
		end--
	}
	if end == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("trim malformed journal tail: %w", err)
	}
	start := bytes.LastIndexByte(contents[:end], '\n') + 1
	_, err = decodeEntry(contents[start:end])
	if err == nil {
		return nil
	}
	var unknown unknownEntryError
	if errors.As(err, &unknown) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("trim malformed journal tail: %w", err)
	}
	if err := os.Truncate(path, int64(start)); err != nil {
		return fmt.Errorf("truncate malformed journal tail: %w", err)
	}
	return nil
}

// normalizeSessionID accepts the forms a user is likely to paste: the bare id
// the journal header records, the "<timestamp>_<id>.jsonl" filename, or a path
// ending in that filename. Session ids are hex, so the last underscore is
// always the filename separator.
func normalizeSessionID(id string) string {
	id = filepath.Base(strings.TrimSpace(id))
	id = strings.TrimSuffix(id, ".jsonl")
	if underscore := strings.LastIndex(id, "_"); underscore >= 0 {
		if suffix := id[underscore+1:]; suffix != "" {
			id = suffix
		}
	}
	return id
}

// Resolve returns the journal path for id under dir and cwd.
func Resolve(ctx context.Context, dir, cwd, id string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("resolve journal: %w", err)
	}
	id = normalizeSessionID(id)
	journalDir := filepath.Join(dir, encodeCwd(cwd))
	entries, err := os.ReadDir(journalDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("resolve journal %q: %w", id, ErrSessionNotFound)
		}
		return "", fmt.Errorf("read journal directory: %w", err)
	}
	suffix := "_" + id + ".jsonl"
	var corruptErr error
	available := make([]string, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("resolve journal: %w", err)
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		matches := strings.HasSuffix(entry.Name(), suffix)
		path := filepath.Join(journalDir, entry.Name())
		header, err := loadHeader(ctx, path)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", fmt.Errorf("resolve journal: %w", ctxErr)
			}
			if matches {
				corruptErr = fmt.Errorf("inspect journal %q: %w", entry.Name(), err)
			}
			continue
		}
		if header.Cwd != cwd {
			continue
		}
		if matches {
			return path, nil
		}
		available = append(available, normalizeSessionID(entry.Name()))
	}
	if corruptErr != nil {
		return "", corruptErr
	}
	if len(available) > 0 {
		sort.Strings(available)
		return "", fmt.Errorf("resolve journal %q: %w (sessions for this directory: %s)", id, ErrSessionNotFound, strings.Join(available, ", "))
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
	if header.Version != JournalVersion {
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
