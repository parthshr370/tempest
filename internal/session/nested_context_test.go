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
	"go.harness.dev/harness/tempest/durable"
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
	if len(loaded.ContextFiles) != 1 || len(loaded.ContextFiles[0].Files) != 2 || loaded.ContextFiles[0].ToolCallID != "read-0" ||
		loaded.ContextFiles[0].Files[0].Path != "a/AGENTS.md" || loaded.ContextFiles[0].Files[1].Path != "a/b/CLAUDE.md" {
		t.Fatalf("journaled context files = %+v", loaded.ContextFiles)
	}
	resumed := runReads(t, StackConfig{Cwd: cwd, Loaded: loaded, InitialMessages: loaded.Messages, ContextFiles: startup}, 1)
	if texts := toolResultTexts(resumed.requests[1].Messages); len(texts) != 3 || strings.Contains(texts[2], "repository_context") {
		t.Fatalf("resumed read repeated nested context: %q", texts)
	}
}

// nestedRepo writes cwd/a/AGENTS.md, cwd/a/b/CLAUDE.md and cwd/a/b/x.go.
func nestedRepo(t *testing.T) string {
	t.Helper()
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
	return cwd
}

// lastReadShowsContext reports whether the last tool result a request sent
// carries the nested context files.
func lastReadShowsContext(t *testing.T, recorder *contextRecorder) bool {
	t.Helper()
	texts := toolResultTexts(recorder.requests[len(recorder.requests)-1].Messages)
	if len(texts) == 0 {
		t.Fatal("no tool result reached the model")
	}
	last := texts[len(texts)-1]
	return strings.Contains(last, `<repository_context path="a/AGENTS.md">`) && strings.Contains(last, `<repository_context path="a/b/CLAUDE.md">`)
}

// A file counts as shown only while the conversation holds the result that
// delivered it: once a rewind or reset drops that result, the next read
// shows the file again, in the live session and after a resume.
func TestNestedContextFollowsTheTranscript(t *testing.T) {
	cwd := nestedRepo(t)
	store, err := journal.Create(context.Background(), t.TempDir(), cwd, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	provider := faux.New(faux.Options{})
	recorder := &contextRecorder{}
	read := func(id string) faux.ResponseStep {
		return recorder.step(faux.Respond(faux.ToolCall("read", json.RawMessage(`{"path":"a/b/x.go"}`), id)))
	}
	done := recorder.step(faux.Respond(faux.Text("done")))
	provider.SetResponses(read("first"), done, read("again"), done, read("after-reset"), done)
	stack, err := BuildAgentStack(StackConfig{Cwd: cwd, Journal: store, Model: provider.Model(), StreamFn: provider.StreamSimple})
	if err != nil {
		t.Fatal(err)
	}
	detach := AttachJournal(stack.Agent, store)
	prompt := func() {
		t.Helper()
		result, err := stack.Agent.PromptText(context.Background(), "read a/b/x.go")
		if err != nil || result.Err != nil {
			t.Fatalf("run err=%v result=%v", err, result.Err)
		}
	}
	prompt()
	if !lastReadShowsContext(t, recorder) {
		t.Fatal("first read did not show the nested context files")
	}
	before := stack.Agent.State().Messages[:1]

	// A rewind to before the read, as the checkpoint manager installs it.
	rewound, err := journal.NewRewindEntry("checkpoint", "", before)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), rewound); err != nil {
		t.Fatal(err)
	}
	stack.Agent.SetMessages(before)
	prompt()
	if !lastReadShowsContext(t, recorder) {
		t.Fatal("the read after a rewind past the delivering result did not show the files again")
	}

	// A reset clears the conversation.
	stack.Agent.SetMessages(nil)
	prompt()
	if !lastReadShowsContext(t, recorder) {
		t.Fatal("the read after a reset did not show the files again")
	}
	detach()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// No reset entry is journaled here, so the journal's transcript is the
	// rewound one plus both later prompts: resuming it holds their
	// deliveries, and a read does not repeat the files.
	loaded, err := journal.Load(context.Background(), store.Path())
	if err != nil {
		t.Fatal(err)
	}
	resumed := runReads(t, StackConfig{Cwd: cwd, Loaded: loaded, InitialMessages: loaded.Messages}, 1)
	if lastReadShowsContext(t, resumed) {
		t.Fatal("a resumed read repeated files its conversation holds")
	}
	// Resuming at the rewind drops every delivery.
	resumed = runReads(t, StackConfig{Cwd: cwd, Loaded: loaded, InitialMessages: before}, 1)
	if !lastReadShowsContext(t, resumed) {
		t.Fatal("a resumed conversation without the delivering result did not show the files")
	}
}

// A fork holds its parent's transcript up to the fork point, and the
// deliveries in it: forked after the read, a read does not repeat the files;
// forked before it, a read shows them.
func TestNestedContextAcrossForks(t *testing.T) {
	cwd := nestedRepo(t)
	sessions := t.TempDir()
	parent, err := journal.Create(context.Background(), sessions, cwd, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	selection := json.RawMessage(`{"model":"test-model","allowedTools":[],"environment":"local"}`)
	if _, err := parent.Commit(context.Background(), durable.Commit{ID: "agent-selection", Changes: []durable.Change{{SessionID: parent.ID(), Kind: durable.DocumentKind, Namespace: "_tempest/agent", ID: "selection", Version: 1, Value: selection}}}); err != nil {
		t.Fatal(err)
	}
	early, err := parent.AppendAssigned(context.Background(), journal.MessageEntry{Message: ptypes.UserMessage{Content: ptypes.StringContent("before the read"), Timestamp: 1}})
	if err != nil {
		t.Fatal(err)
	}
	runReads(t, StackConfig{Cwd: cwd, Journal: parent}, 1)
	late, err := parent.AppendAssigned(context.Background(), journal.MessageEntry{Message: ptypes.UserMessage{Content: ptypes.StringContent("after the read"), Timestamp: 2}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		entry string
		shows bool
	}{{"after the read", late.ID, false}, {"before the read", early.ID, true}} {
		seed, err := parent.CaptureFork(context.Background(), test.entry, false)
		if err != nil {
			t.Fatal(test.name, err)
		}
		child, err := journal.Create(context.Background(), sessions, cwd, journal.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := child.InitializeFork(context.Background(), seed); err != nil {
			t.Fatal(test.name, err)
		}
		loaded, err := child.Load(context.Background())
		if err != nil {
			t.Fatal(test.name, err)
		}
		if err := child.Close(); err != nil {
			t.Fatal(err)
		}
		forked := runReads(t, StackConfig{Cwd: cwd, Loaded: loaded, InitialMessages: loaded.Messages}, 1)
		if got := lastReadShowsContext(t, forked); got != test.shows {
			t.Fatalf("fork %s: read showed the files = %v, want %v", test.name, got, test.shows)
		}
	}
}

// Distinct assistant turns may share a timestamp and a provider call ID. A
// delivery belongs to the one result that carried it: a later read under
// the same timestamp and ID does not borrow a dropped result's files, and
// neither does a held result that carried none of them.
func TestNestedContextDeliveryIsTheResultNotItsTurnAndCallID(t *testing.T) {
	// Every read turn carries this one timestamp.
	const sharedTimestamp = 123
	cwd := nestedRepo(t)
	provider := faux.New(faux.Options{})
	recorder := &contextRecorder{}
	read := func(id string) faux.ResponseStep {
		return recorder.step(faux.RespondMessage(ptypes.AssistantMessage{
			Content:    []ptypes.ContentBlock{faux.ToolCall("read", json.RawMessage(`{"path":"a/b/x.go"}`), id)},
			StopReason: ptypes.StopToolUse,
			Timestamp:  sharedTimestamp,
		}))
	}
	done := recorder.step(faux.Respond(faux.Text("done")))
	provider.SetResponses(read("duplicate"), done, read("duplicate"), done, read("duplicate"), done, read("other"), done)
	stack, err := BuildAgentStack(StackConfig{Cwd: cwd, Model: provider.Model(), StreamFn: provider.StreamSimple})
	if err != nil {
		t.Fatal(err)
	}
	prompt := func() {
		t.Helper()
		result, err := stack.Agent.PromptText(context.Background(), "read a/b/x.go")
		if err != nil || result.Err != nil {
			t.Fatalf("run err=%v result=%v", err, result.Err)
		}
	}
	prompt()
	if !lastReadShowsContext(t, recorder) {
		t.Fatal("first read did not show the nested context files")
	}
	stack.Agent.SetMessages(nil)
	prompt()
	if !lastReadShowsContext(t, recorder) {
		t.Fatal("a read after a reset, under the dropped turn's timestamp and call ID, did not show the files")
	}
	// A second read in the same conversation carries no files: they are shown.
	prompt()
	if lastReadShowsContext(t, recorder) {
		t.Fatal("a read repeated files the conversation holds")
	}
	// Keep only the read that carried nothing: the files are not in the
	// conversation, though it holds a result with the same timestamp and ID.
	messages := stack.Agent.State().Messages
	stack.Agent.SetMessages(messages[len(messages)-4:])
	prompt()
	if !lastReadShowsContext(t, recorder) {
		t.Fatal("a held result that carried no files counted as their delivery")
	}
}
