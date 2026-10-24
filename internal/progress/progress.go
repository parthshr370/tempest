// Package progress manages the anti-stateless checkpoint: on each todo_write,
// it atomically rewrites the session's progress.md (tmp+rename, mutex-guarded)
// so a crash mid-build doesn't lose the working state. The file lives in the
// session's private data directory, never in the user's workspace. Validate
// rejects control characters that would break the single-line checklist format.
//
// Todos may carry a phase. Besides replacing the whole list, callers apply
// ops (init, start, done, block, append, drop) that address todos by content;
// the list behind progress.md is kept in progress.json beside it, so ops keep
// working after the session resumes in a new process.
package progress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
)

// FileName is the checkpoint's name inside a session data directory.
const FileName = "progress.md"

// stateFileName holds the todo list progress.md renders.
const stateFileName = "progress.json"

// Path returns the checkpoint path inside the session's local:// directory,
// or "" when the session has no data directory and todos stay in memory.
func Path(sessionDataDir string) string {
	if sessionDataDir == "" {
		return ""
	}
	return filepath.Join(sessionDataDir, "local", FileName)
}

// Migrate moves the former top-level progress files into local:// once.
func Migrate(sessionDataDir string) error {
	if sessionDataDir == "" {
		return nil
	}
	for _, name := range []string{FileName, stateFileName} {
		oldPath := filepath.Join(sessionDataDir, name)
		newPath := filepath.Join(sessionDataDir, "local", name)
		if _, err := os.Stat(oldPath); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if _, err := os.Stat(newPath); err == nil {
			return fmt.Errorf("progress migration: both %s and %s exist", oldPath, newPath)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		localDir := filepath.Dir(newPath)
		if err := os.Mkdir(localDir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("progress migration: %w", err)
		}
		info, err := os.Lstat(localDir)
		if err != nil {
			return fmt.Errorf("progress migration: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("progress migration: local directory %s is not a real directory", localDir)
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			return fmt.Errorf("progress migration: %w", err)
		}
	}
	return nil
}

// The todo statuses.
const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusBlocked    = "blocked"
	StatusDropped    = "dropped"
)

// Statuses lists every valid status, in schema order.
var Statuses = []string{StatusPending, StatusInProgress, StatusCompleted, StatusBlocked, StatusDropped}

// TodoItem is a single todo with id, content, status, and optional
// priority, phase and blocker note.
type TodoItem struct {
	ID       string `json:"id,omitempty"`
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority,omitempty"`
	Phase    string `json:"phase,omitempty"`
	Blocker  string `json:"blocker,omitempty"`
}

// The op names [Op] accepts.
const (
	OpInit   = "init"
	OpStart  = "start"
	OpDone   = "done"
	OpBlock  = "block"
	OpAppend = "append"
	OpDrop   = "drop"
)

// OpNames lists every op, in schema order.
var OpNames = []string{OpInit, OpStart, OpDone, OpBlock, OpAppend, OpDrop}

// Op is one edit of the todo list. Items name todos by exact content.
//
//	init    replace the list with Items as pending todos in Phase
//	append  add Items as pending todos in Phase
//	start   mark the one item in progress; any other in-progress todo returns to pending
//	done    mark Items, or every todo of Phase, completed
//	block   mark Items, or the open todos of Phase, blocked with Reason
//	drop    mark Items, or every todo of Phase, dropped
type Op struct {
	Op     string
	Phase  string
	Items  []string
	Reason string
}

// Store is a thread-safe progress store. When it has a path, Update
// atomically rewrites that file (tmp+rename, mutex-guarded) so a crash
// mid-build doesn't lose working state.
type Store struct {
	mu     sync.Mutex
	path   string
	todos  []TodoItem
	loaded bool
}

// NewStore keeps todos in memory and checkpoints them to path when it is set.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// Path returns the checkpoint file, or "" for an in-memory store.
func (s *Store) Path() string {
	return s.path
}

// Update validates todos, renders them to markdown, and atomically writes
// the checkpoint. Returns validation errors without touching the file.
func (s *Store) Update(ctx context.Context, todos []TodoItem) error {
	if err := Validate(todos); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(todos) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(cloneTodos(todos))
}

// Apply applies op to the current list and checkpoints the result, which
// it returns. A list from an earlier process is loaded first.
func (s *Store) Apply(ctx context.Context, op Op) ([]TodoItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	next, err := ApplyOp(s.todos, op)
	if err != nil {
		return nil, err
	}
	if err := Validate(next); err != nil {
		return nil, err
	}
	if err := s.saveLocked(next); err != nil {
		return nil, err
	}
	return cloneTodos(next), nil
}

// loadLocked reads progress.json once when the store has a path and no
// list yet.
func (s *Store) loadLocked() error {
	if s.loaded || s.path == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(s.path), stateFileName))
	if errors.Is(err, os.ErrNotExist) {
		s.loaded = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("read todo state: %w", err)
	}
	var todos []TodoItem
	if err := json.Unmarshal(data, &todos); err != nil {
		return fmt.Errorf("decode todo state: %w", err)
	}
	if err := Validate(todos); err != nil {
		return fmt.Errorf("todo state: %w", err)
	}
	s.todos, s.loaded = todos, true
	return nil
}

func (s *Store) saveLocked(todos []TodoItem) error {
	if s.path != "" {
		state, err := json.Marshal(todos)
		if err != nil {
			return err
		}
		if err := writeAtomic(filepath.Join(filepath.Dir(s.path), stateFileName), string(state)); err != nil {
			return err
		}
		if err := writeAtomic(s.path, RenderMarkdown(todos)); err != nil {
			return err
		}
	}
	s.todos, s.loaded = todos, true
	return nil
}

// writeAtomic replaces path through a sibling temp file; the session data
// directory is private, so it is created 0700 and the file 0600.
func writeAtomic(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(content), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return errors.Join(err, os.Remove(tmpPath))
	}
	return nil
}

// Todos returns a copy of the current todos.
func (s *Store) Todos() []TodoItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneTodos(s.todos)
}

// ApplyOp returns todos with op applied; todos is not modified.
func ApplyOp(todos []TodoItem, op Op) ([]TodoItem, error) {
	next := cloneTodos(todos)
	switch op.Op {
	case OpInit, OpAppend:
		if len(op.Items) == 0 {
			return nil, fmt.Errorf("%s needs items", op.Op)
		}
		if op.Op == OpInit {
			next = nil
		}
		for _, content := range op.Items {
			if indexes := findContent(next, content); len(indexes) > 0 {
				return nil, fmt.Errorf("todo %q already exists; contents must be unique to be addressable", content)
			}
			next = append(next, TodoItem{Content: content, Status: StatusPending, Phase: op.Phase})
		}
		return next, nil
	case OpStart:
		if len(op.Items) != 1 || op.Phase != "" {
			return nil, errors.New("start takes exactly one item and no phase")
		}
		targets, err := resolveTargets(next, op)
		if err != nil {
			return nil, err
		}
		for i := range next {
			if next[i].Status == StatusInProgress {
				next[i].Status = StatusPending
			}
		}
		next[targets[0]].Status, next[targets[0]].Blocker = StatusInProgress, ""
		return next, nil
	case OpDone, OpBlock, OpDrop:
		targets, err := resolveTargets(next, op)
		if err != nil {
			return nil, err
		}
		for _, i := range targets {
			switch op.Op {
			case OpDone:
				next[i].Status, next[i].Blocker = StatusCompleted, ""
			case OpDrop:
				next[i].Status, next[i].Blocker = StatusDropped, ""
			case OpBlock:
				closed := next[i].Status == StatusCompleted || next[i].Status == StatusDropped
				if closed && len(op.Items) > 0 {
					return nil, fmt.Errorf("todo %q is %s and cannot be blocked", next[i].Content, next[i].Status)
				}
				if !closed {
					next[i].Status, next[i].Blocker = StatusBlocked, strings.Join(strings.Fields(op.Reason), " ")
				}
			}
		}
		return next, nil
	case "":
		return nil, errors.New("op is required")
	}
	return nil, fmt.Errorf("unknown op %q; use one of %s", op.Op, strings.Join(OpNames, ", "))
}

// resolveTargets returns the indexes op addresses: its items by content,
// or every todo in its phase.
func resolveTargets(todos []TodoItem, op Op) ([]int, error) {
	if len(op.Items) > 0 && op.Phase != "" {
		return nil, fmt.Errorf("%s takes items or a phase, not both", op.Op)
	}
	if len(op.Items) == 0 {
		if op.Phase == "" {
			return nil, fmt.Errorf("%s needs items or a phase", op.Op)
		}
		var targets []int
		for i, todo := range todos {
			if todo.Phase == op.Phase {
				targets = append(targets, i)
			}
		}
		if len(targets) == 0 {
			return nil, fmt.Errorf("no todos in phase %q", op.Phase)
		}
		return targets, nil
	}
	targets := make([]int, 0, len(op.Items))
	for _, content := range op.Items {
		indexes := findContent(todos, content)
		switch len(indexes) {
		case 0:
			return nil, fmt.Errorf("no todo with content %q; address todos by their exact content", content)
		case 1:
			targets = append(targets, indexes[0])
		default:
			return nil, fmt.Errorf("%d todos have content %q; replace the list with unique contents", len(indexes), content)
		}
	}
	return targets, nil
}

func findContent(todos []TodoItem, content string) []int {
	var indexes []int
	for i, todo := range todos {
		if todo.Content == content {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

// Validate checks that every todo has non-empty content, printable
// single-line text, and valid status/priority values.
func Validate(todos []TodoItem) error {
	for i, todo := range todos {
		if strings.TrimSpace(todo.Content) == "" {
			return fmt.Errorf("todo %d content is required", i+1)
		}
		for field, value := range map[string]string{"content": todo.Content, "phase": todo.Phase, "blocker": todo.Blocker} {
			for _, r := range value {
				if unicode.IsControl(r) {
					return fmt.Errorf("todo %d %s must be a single printable line", i+1, field)
				}
			}
		}
		switch todo.Status {
		case StatusPending, StatusInProgress, StatusCompleted, StatusBlocked, StatusDropped:
		default:
			return fmt.Errorf("todo %d has invalid status %q", i+1, todo.Status)
		}
		if todo.Priority != "" {
			switch todo.Priority {
			case "high", "medium", "low":
			default:
				return fmt.Errorf("todo %d has invalid priority %q", i+1, todo.Priority)
			}
		}
	}
	return nil
}

// RenderMarkdown renders todos as a markdown checklist with status and
// priority annotations. Phased todos are grouped under a heading per phase,
// in order of first appearance, after any todos without a phase.
func RenderMarkdown(todos []TodoItem) string {
	var b strings.Builder
	b.WriteString("# Progress\n\n")
	if len(todos) == 0 {
		b.WriteString("No todos.\n")
		return b.String()
	}
	var phases []string
	byPhase := map[string][]TodoItem{}
	for _, todo := range todos {
		if _, seen := byPhase[todo.Phase]; !seen && todo.Phase != "" {
			phases = append(phases, todo.Phase)
		}
		byPhase[todo.Phase] = append(byPhase[todo.Phase], todo)
	}
	for _, todo := range byPhase[""] {
		renderTodo(&b, todo)
	}
	for i, phase := range phases {
		if i > 0 || len(byPhase[""]) > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "## %s\n", phase)
		for _, todo := range byPhase[phase] {
			renderTodo(&b, todo)
		}
	}
	return b.String()
}

func renderTodo(b *strings.Builder, todo TodoItem) {
	box := "[ ]"
	switch todo.Status {
	case StatusCompleted:
		box = "[x]"
	case StatusDropped:
		box = "[-]"
	}
	b.WriteString("- ")
	b.WriteString(box)
	b.WriteByte(' ')
	if todo.ID != "" {
		b.WriteString("`")
		b.WriteString(escapeBackticks(todo.ID))
		b.WriteString("` ")
	}
	b.WriteString(todo.Content)
	metadata := []string{}
	switch todo.Status {
	case StatusInProgress:
		metadata = append(metadata, "in progress")
	case StatusBlocked:
		if todo.Blocker != "" {
			metadata = append(metadata, "blocked: "+todo.Blocker)
		} else {
			metadata = append(metadata, "blocked")
		}
	case StatusDropped:
		metadata = append(metadata, "dropped")
	}
	if todo.Priority != "" {
		metadata = append(metadata, todo.Priority+" priority")
	}
	if len(metadata) > 0 {
		b.WriteString(" _(")
		b.WriteString(strings.Join(metadata, ", "))
		b.WriteString(")_")
	}
	b.WriteByte('\n')
}

func cloneTodos(todos []TodoItem) []TodoItem {
	if todos == nil {
		return nil
	}
	out := make([]TodoItem, len(todos))
	copy(out, todos)
	return out
}

func escapeBackticks(s string) string {
	return strings.ReplaceAll(s, "`", "\\`")
}
