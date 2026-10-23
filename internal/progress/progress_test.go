package progress

import (
	"context"
	"os"
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
