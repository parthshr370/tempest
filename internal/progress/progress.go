// Package progress manages the anti-stateless checkpoint: on each todo_write,
// it atomically rewrites the session's progress.md (tmp+rename, mutex-guarded)
// so a crash mid-build doesn't lose the working state. The file lives in the
// session's private data directory, never in the user's workspace. Validate
// rejects control characters that would break the single-line checklist format.
package progress

import (
	"context"
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

// Path returns the checkpoint path inside sessionDataDir, or "" when the
// session has no data directory and todos stay in memory.
func Path(sessionDataDir string) string {
	if sessionDataDir == "" {
		return ""
	}
	return filepath.Join(sessionDataDir, FileName)
}

// TodoItem is a single todo with id, content, status, and optional priority.
type TodoItem struct {
	ID       string `json:"id,omitempty"`
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority,omitempty"`
}

// Store is a thread-safe progress store. When it has a path, Update
// atomically rewrites that file (tmp+rename, mutex-guarded) so a crash
// mid-build doesn't lose working state.
type Store struct {
	mu    sync.Mutex
	path  string
	todos []TodoItem
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
	copyTodos := cloneTodos(todos)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path != "" {
		if err := writeAtomic(s.path, RenderMarkdown(copyTodos)); err != nil {
			return err
		}
	}
	s.todos = copyTodos
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

// Validate checks that every todo has non-empty content, printable
// characters, and valid status/priority values.
func Validate(todos []TodoItem) error {
	for i, todo := range todos {
		if strings.TrimSpace(todo.Content) == "" {
			return fmt.Errorf("todo %d content is required", i+1)
		}
		for _, r := range todo.Content {
			if unicode.IsControl(r) {
				return fmt.Errorf("todo %d content must be a single printable line", i+1)
			}
		}
		switch todo.Status {
		case "pending", "in_progress", "completed":
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
// priority annotations.
func RenderMarkdown(todos []TodoItem) string {
	var b strings.Builder
	b.WriteString("# Progress\n\n")
	if len(todos) == 0 {
		b.WriteString("No todos.\n")
		return b.String()
	}
	for _, todo := range todos {
		box := "[ ]"
		if todo.Status == "completed" {
			box = "[x]"
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
		if todo.Status == "in_progress" {
			metadata = append(metadata, "in progress")
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
	return b.String()
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
