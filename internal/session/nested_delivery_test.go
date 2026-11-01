package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/agent"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/session/journal"
)

func nestedWorkspace(t *testing.T) string {
	t.Helper()
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"AGENTS.md": "FIRST_LINE\nSECOND_LINE_RULE\n", "x.go": "package a\n"} {
		if err := os.WriteFile(filepath.Join(cwd, "a", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cwd
}

func surfaceAfter(t *testing.T, hook func(context.Context, agent.AfterToolCallContext) *agent.AfterToolCallResult, event agent.PathEvent) string {
	t.Helper()
	result := hook(context.Background(), agent.AfterToolCallContext{
		ToolCall: ptypes.ContentBlock{Type: ptypes.BlockToolCall, ID: "read-1", Name: "read"},
		Result:   agent.AgentToolResult{Content: []ptypes.ContentBlock{ptypes.NewText("source")}, Paths: []agent.PathEvent{event}},
	})
	if result == nil {
		return ""
	}
	var text strings.Builder
	for _, block := range result.Content {
		text.WriteString(block.Text)
	}
	return text.String()
}

// The record that a directory's context file was delivered is durable only
// together with the tool result that carries it: a crash after discovery but
// before the result is committed leaves nothing recorded, so the resumed
// session surfaces the file again.
func TestNestedContextCrashBeforeResultCommitSurfacesAgain(t *testing.T) {
	cwd := nestedWorkspace(t)
	store, err := journal.Create(context.Background(), t.TempDir(), cwd, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	hook := nestedContextHook(StackConfig{Journal: store}, cwd, true)
	read := agent.PathEvent{Path: filepath.Join(cwd, "a", "x.go")}
	if got := surfaceAfter(t, hook, read); !strings.Contains(got, "SECOND_LINE_RULE") {
		t.Fatalf("first read = %q", got)
	}
	// The process dies here: the tool result never reaches the journal.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := journal.Load(context.Background(), store.Path())
	if err != nil {
		t.Fatal(err)
	}
	resumed := nestedContextHook(StackConfig{Loaded: loaded}, cwd, true)
	if got := surfaceAfter(t, resumed, read); !strings.Contains(got, "SECOND_LINE_RULE") {
		t.Fatalf("resumed read lost the undelivered context file: %q", got)
	}
}

// A read that showed only part of a context file does not count as showing
// it: the rest arrives framed after the result.
func TestNestedPartialReadOfContextFileStillSurfacesIt(t *testing.T) {
	cwd := nestedWorkspace(t)
	hook := nestedContextHook(StackConfig{}, cwd, true)
	got := surfaceAfter(t, hook, agent.PathEvent{Path: filepath.Join(cwd, "a", "AGENTS.md")})
	if !strings.Contains(got, `<repository_context path="a/AGENTS.md">`) || !strings.Contains(got, "SECOND_LINE_RULE") {
		t.Fatalf("partial read of a/AGENTS.md = %q", got)
	}
}
