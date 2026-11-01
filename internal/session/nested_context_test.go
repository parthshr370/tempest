package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/prompt"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
)

// contextRecorder captures every request the faux model receives.
type contextRecorder struct {
	mu       sync.Mutex
	requests []ptypes.Context
}

func (r *contextRecorder) step(next faux.ResponseStep) faux.ResponseStep {
	return func(c ptypes.Context, opts *ptypes.StreamOptions, state faux.State, model ptypes.Model) (ptypes.AssistantMessage, error) {
		r.mu.Lock()
		r.requests = append(r.requests, c)
		r.mu.Unlock()
		return next(c, opts, state, model)
	}
}

// toolResultTexts returns the text of each tool result in order.
func toolResultTexts(messages []ptypes.Message) []string {
	var texts []string
	for _, message := range messages {
		var result ptypes.ToolResultMessage
		switch typed := message.(type) {
		case ptypes.ToolResultMessage:
			result = typed
		case *ptypes.ToolResultMessage:
			result = *typed
		default:
			continue
		}
		var text strings.Builder
		for _, block := range result.Content {
			text.WriteString(block.Text)
		}
		texts = append(texts, text.String())
	}
	return texts
}

func runReads(t *testing.T, cfg StackConfig, reads int) *contextRecorder {
	t.Helper()
	provider := faux.New(faux.Options{})
	recorder := &contextRecorder{}
	var steps []faux.ResponseStep
	for index := range reads {
		steps = append(steps, recorder.step(faux.Respond(faux.ToolCall("read", json.RawMessage(`{"path":"a/b/x.go"}`), "read-"+strconv.Itoa(index)))))
	}
	provider.SetResponses(append(steps, recorder.step(faux.Respond(faux.Text("done"))))...)
	cfg.Model, cfg.StreamFn = provider.Model(), provider.StreamSimple
	stack, err := BuildAgentStack(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Journal != nil {
		defer AttachJournal(stack.Agent, cfg.Journal)()
	}
	result, err := stack.Agent.PromptText(context.Background(), "read a/b/x.go")
	if err != nil || result.Err != nil {
		t.Fatalf("run err=%v result=%v", err, result.Err)
	}
	return recorder
}

func TestStackSurfacesNestedContextAfterReadOnceAndAcrossResume(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(cwd, "a", "AGENTS.md"):      "outer rules",
		filepath.Join(cwd, "a", "b", "CLAUDE.md"): "inner rules",
		filepath.Join(cwd, "a", "b", "x.go"):      "package b\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := journal.Create(context.Background(), t.TempDir(), cwd, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	startup := []prompt.ContextFile{{Path: filepath.Join(cwd, "AGENTS.md"), Content: "root rules"}}
	recorder := runReads(t, StackConfig{Cwd: cwd, Journal: store, ContextFiles: startup}, 2)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	requests := recorder.requests
	if len(requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(requests))
	}
	for index, request := range requests {
		if request.SystemPrompt != requests[0].SystemPrompt {
			t.Fatalf("request %d changed the system prompt", index)
		}
	}
	if strings.Contains(requests[0].SystemPrompt, "outer rules") {
		t.Fatal("nested context reached the system prompt")
	}
	results := toolResultTexts(requests[2].Messages)
	if len(results) != 2 {
		t.Fatalf("tool results = %q", results)
	}
	outer, inner := strings.Index(results[0], `<repository_context path="a/AGENTS.md">`), strings.Index(results[0], `<repository_context path="a/b/CLAUDE.md">`)
	if outer < 0 || inner < outer || !strings.Contains(results[0], "package b") || strings.Index(results[0], "package b") > outer {
		t.Fatalf("first read result = %q", results[0])
	}
	if strings.Contains(results[1], "repository_context") {
		t.Fatalf("second read repeated nested context: %q", results[1])
	}

	loaded, err := journal.Load(context.Background(), store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.ContextFiles) != 2 || loaded.ContextFiles[0].Path != "a/AGENTS.md" || loaded.ContextFiles[1].Path != "a/b/CLAUDE.md" {
		t.Fatalf("journaled context files = %+v", loaded.ContextFiles)
	}
	resumed := runReads(t, StackConfig{Cwd: cwd, Loaded: loaded, ContextFiles: startup}, 1)
	if texts := toolResultTexts(resumed.requests[1].Messages); len(texts) != 1 || strings.Contains(texts[0], "repository_context") {
		t.Fatalf("resumed read repeated nested context: %q", texts)
	}
}
