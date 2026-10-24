package progress

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateTodos(t *testing.T) {
	valid := []TodoItem{{Content: "plan", Status: "pending", Priority: "high"}}
	if err := Validate(valid); err != nil {
		t.Fatalf("valid todos: %v", err)
	}
	for _, tc := range []struct {
		name  string
		todos []TodoItem
	}{
		{"missing content", []TodoItem{{Status: "pending"}}},
		{"bad status", []TodoItem{{Content: "x", Status: "cancelled"}}},
		{"bad priority", []TodoItem{{Content: "x", Status: "pending", Priority: "urgent"}}},
		{"newline content", []TodoItem{{Content: "x\ny", Status: "pending"}}},
		{"control content", []TodoItem{{Content: "x\x00y", Status: "pending"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(tc.todos); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestRenderMarkdown(t *testing.T) {
	got := RenderMarkdown([]TodoItem{
		{ID: "a", Content: "Plan work", Status: "completed", Priority: "high"},
		{Content: "Implement", Status: "in_progress"},
	})
	for _, want := range []string{"# Progress", "- [x] `a` Plan work _(high priority)_", "- [ ] Implement _(in progress)_"} {
		if !strings.Contains(got, want) {
			t.Fatalf("render missing %q:\n%s", want, got)
		}
	}
}

func TestStoreUpdateWritesProgress(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(Path(dir))
	if err := store.Update(context.Background(), []TodoItem{{Content: "ship", Status: "pending"}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "- [ ] ship") {
		t.Fatalf("progress file = %q", string(data))
	}
	if got := store.Todos(); len(got) != 1 || got[0].Content != "ship" {
		t.Fatalf("store todos = %+v", got)
	}
	if _, err := os.Stat(Path(dir) + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file should not remain after atomic rename; err=%v", err)
	}
}

func TestStoreUpdateEmptyListIsNoop(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(Path(dir))
	if err := store.Update(context.Background(), []TodoItem{{Content: "ship", Status: "pending"}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("empty update changed progress file:\nbefore=%q\nafter=%q", string(before), string(after))
	}
	if got := store.Todos(); len(got) != 1 || got[0].Content != "ship" {
		t.Fatalf("empty update changed store todos = %+v", got)
	}
}

func TestApplyOp(t *testing.T) {
	base := []TodoItem{
		{Content: "a", Status: StatusInProgress, Phase: "P1"},
		{Content: "b", Status: StatusPending, Phase: "P1"},
		{Content: "c", Status: StatusCompleted, Phase: "P2"},
		{Content: "d", Status: StatusPending, Phase: "P2"},
	}
	started, err := ApplyOp(base, Op{Op: OpStart, Items: []string{"b"}})
	if err != nil || started[0].Status != StatusPending || started[1].Status != StatusInProgress {
		t.Fatalf("start = %+v, %v", started, err)
	}
	if base[0].Status != StatusInProgress {
		t.Fatal("ApplyOp modified its input")
	}
	blocked, err := ApplyOp(base, Op{Op: OpBlock, Phase: "P2", Reason: "  needs\tkeys "})
	if err != nil || blocked[2].Status != StatusCompleted || blocked[3].Status != StatusBlocked || blocked[3].Blocker != "needs keys" {
		t.Fatalf("block by phase = %+v, %v", blocked, err)
	}
	unblocked, err := ApplyOp(blocked, Op{Op: OpStart, Items: []string{"d"}})
	if err != nil || unblocked[3].Status != StatusInProgress || unblocked[3].Blocker != "" {
		t.Fatalf("start clears the blocker = %+v, %v", unblocked, err)
	}
	initialized, err := ApplyOp(base, Op{Op: OpInit, Phase: "Next", Items: []string{"x", "y"}})
	if err != nil || len(initialized) != 2 || initialized[1] != (TodoItem{Content: "y", Status: StatusPending, Phase: "Next"}) {
		t.Fatalf("init = %+v, %v", initialized, err)
	}
	duplicated := append(cloneTodos(base), TodoItem{Content: "a", Status: StatusPending})
	for _, tc := range []struct {
		todos []TodoItem
		op    Op
		want  string
	}{
		{duplicated, Op{Op: OpDone, Items: []string{"a"}}, "2 todos have content"},
		{base, Op{Op: OpBlock, Items: []string{"c"}}, "cannot be blocked"},
		{base, Op{Op: OpDone, Items: []string{"a"}, Phase: "P1"}, "not both"},
		{base, Op{Op: OpInit, Items: []string{"x", "x"}}, "already exists"},
		{base, Op{Op: OpInit}, "needs items"},
		{base, Op{Op: "rename"}, "unknown op"},
	} {
		if _, err := ApplyOp(tc.todos, tc.op); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("ApplyOp(%+v) error = %v, want %q", tc.op, err, tc.want)
		}
	}
}
func TestMigrateMovesProgressIntoLocalDirectory(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{FileName: "# old checkpoint\n", stateFileName: "[]"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(dir); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{FileName: "# old checkpoint\n", stateFileName: "[]"} {
		oldPath := filepath.Join(dir, name)
		newPath := filepath.Join(dir, "local", name)
		got, err := os.ReadFile(newPath)
		if err != nil || string(got) != want {
			t.Fatalf("migrated %s = %q, %v", name, got, err)
		}
		if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old %s remains after migration: %v", name, err)
		}
	}
}

func TestStoreApplyResumesFromState(t *testing.T) {
	dir := t.TempDir()
	first := NewStore(Path(dir))
	if _, err := first.Apply(context.Background(), Op{Op: OpInit, Phase: "Build", Items: []string{"compile", "link"}}); err != nil {
		t.Fatal(err)
	}
	second := NewStore(Path(dir))
	todos, err := second.Apply(context.Background(), Op{Op: OpDone, Items: []string{"compile"}})
	if err != nil || len(todos) != 2 || todos[0].Status != StatusCompleted || todos[1].Phase != "Build" {
		t.Fatalf("resumed apply = %+v, %v", todos, err)
	}
	data, err := os.ReadFile(Path(dir))
	if err != nil || !strings.Contains(string(data), "## Build\n- [x] compile\n- [ ] link\n") {
		t.Fatalf("progress.md = %q, %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), []byte(`[{"content":"x","status":"bogus"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(Path(dir)).Apply(context.Background(), Op{Op: OpDone, Items: []string{"x"}}); err == nil {
		t.Fatal("corrupt todo state was accepted")
	}
}
