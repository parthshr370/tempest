package session

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
)

// requestLog keeps the messages of every request a faux provider is sent.
type requestLog struct {
	mu       sync.Mutex
	requests []types.Context
}

func (l *requestLog) stream(f *faux.Faux) agent.StreamFn {
	return func(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
		l.mu.Lock()
		copied := c
		copied.Messages = slices.Clone(c.Messages)
		l.requests = append(l.requests, copied)
		l.mu.Unlock()
		return f.StreamSimple(ctx, model, c, opts)
	}
}

func (l *requestLog) all() []types.Context {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.requests)
}

func reminderCopies(messages []types.Message, name string) int {
	count := 0
	for _, message := range messages {
		if user, ok := message.(types.UserMessage); ok && user.Reminder == name {
			count++
		}
	}
	return count
}

func newReminderAgent(f *faux.Faux, log *requestLog, messages []types.Message, reminders ...agent.Reminder) *agent.Agent {
	work := agent.AgentTool{
		Tool: types.Tool{Name: "work", Description: "work", Parameters: json.RawMessage(`{"type":"object"}`)},
		Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("ok")}}, nil
		},
	}
	return agent.NewAgent(agent.AgentOptions{
		InitialState: &agent.AgentState{SystemPrompt: "test", Model: f.Model(), Tools: []agent.AgentTool{work}, Messages: messages},
		StreamFn:     log.stream(f),
		Reminders:    reminders,
	})
}

// crashOn runs prompt and simulates process death at the first event match
// accepts: the journal (attached first) has recorded it, then the store
// closes and the run fails.
func crashOn(t *testing.T, store *journal.Store, agt *agent.Agent, match func(agent.AgentEvent) bool) {
	t.Helper()
	crashed := false
	agt.Subscribe(func(_ context.Context, ev agent.AgentEvent) error {
		if crashed {
			return errCrashed
		}
		if !match(ev) {
			return nil
		}
		crashed = true
		if err := store.Close(); err != nil {
			t.Errorf("close at crash: %v", err)
		}
		return errCrashed
	})
	if _, err := agt.PromptText(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if !crashed {
		t.Fatal("crash point not reached")
	}
}

func resumeStore(t *testing.T, path string) (*journal.Store, *journal.Loaded) {
	t.Helper()
	store, loaded, err := journal.OpenForResume(context.Background(), path, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if loaded.LastRun == nil || loaded.LastRun.Finished {
		t.Fatalf("crashed run looks finished: %+v", loaded.LastRun)
	}
	return store, loaded
}

// A crash right after a conversation reminder is journaled, before its
// request is answered, resumes with exactly one copy: the resumed request
// does not append the reminder again, though its cadence and bound allow it.
func TestConversationReminderCrashResumesWithOneCopy(t *testing.T) {
	rule := agent.Reminder{Name: "rule", Text: "keep the tests green", Scope: agent.ReminderConversation, First: 2, MaxFires: 3}
	store, err := journal.Create(context.Background(), t.TempDir(), "/workspace/project", journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	path := store.Path()
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.ToolCall("work", json.RawMessage(`{}`), "call-1")), faux.Respond(faux.Text("not reached")))
	log := &requestLog{}
	agt := newReminderAgent(f, log, nil, rule)
	AttachJournal(agt, store)
	crashOn(t, store, agt, func(ev agent.AgentEvent) bool {
		user, ok := ev.Message.(types.UserMessage)
		return ev.Type == agent.EventMessageEnd && ok && user.Reminder == "rule"
	})
	if len(log.all()) != 1 {
		t.Fatalf("requests before the crash = %d, want 1", len(log.all()))
	}

	resumed, loaded := resumeStore(t, path)
	if reminderCopies(loaded.Messages, "rule") != 1 {
		t.Fatalf("journaled copies = %d, want 1", reminderCopies(loaded.Messages, "rule"))
	}
	f.SetResponses(faux.Respond(faux.Text("done")))
	log = &requestLog{}
	after := newReminderAgent(f, log, loaded.Messages, rule)
	AttachJournal(after, resumed)
	result, err := after.Continue(context.Background())
	if err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("resumed run = %+v err=%v", result, err)
	}
	requests := log.all()
	if len(requests) != 1 || reminderCopies(requests[0].Messages, "rule") != 1 {
		t.Fatalf("resumed request copies = %d (requests %d), want 1", reminderCopies(requests[0].Messages, "rule"), len(requests))
	}
	if got := reminderCopies(after.State().Messages, "rule"); got != 1 {
		t.Fatalf("transcript copies = %d, want 1", got)
	}
	final := loadJournal(t, path)
	if fires := final.LogicalReminders(final.LastRun.LogicalRunID); fires["rule"] != 1 {
		t.Fatalf("journaled fires = %v, want rule once", fires)
	}
}

// A request-scope reminder never reaches the journal, its committed firing
// keeps MaxFires across a crash, and a resumed session never sees it again.
func TestRequestReminderIsNotJournaledAndBoundHoldsAcrossCrash(t *testing.T) {
	nudge := agent.Reminder{Name: "nudge", Text: "NUDGE-SECRET-TEXT"}
	store, err := journal.Create(context.Background(), t.TempDir(), "/workspace/project", journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	path := store.Path()
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.ToolCall("work", json.RawMessage(`{}`), "call-1")), faux.Respond(faux.Text("not reached")))
	log := &requestLog{}
	agt := newReminderAgent(f, log, nil, nudge)
	AttachJournal(agt, store)
	crashOn(t, store, agt, func(ev agent.AgentEvent) bool { return ev.Type == agent.EventTurnEnd })
	if requests := log.all(); len(requests) != 1 || requests[0].RequestScoped != 1 {
		t.Fatalf("first request did not carry the nudge: %+v", requests)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("NUDGE-SECRET-TEXT")) || !bytes.Contains(raw, []byte(`"reminders":[{"name":"nudge","scope":"request"}]`)) {
		t.Fatalf("journal holds the reminder text or lacks its firing:\n%s", raw)
	}

	resumed, loaded := resumeStore(t, path)
	if reminderCopies(loaded.Messages, "nudge") != 0 {
		t.Fatal("resumed transcript holds a request-scope reminder")
	}
	f.SetResponses(faux.Respond(faux.Text("done")))
	log = &requestLog{}
	after := newReminderAgent(f, log, loaded.Messages, nudge)
	AttachJournal(after, resumed)
	if result, err := after.Continue(context.Background()); err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("resumed run = %+v err=%v", result, err)
	}
	for _, request := range log.all() {
		encoded, _ := json.Marshal(request.Messages)
		if request.RequestScoped != 0 || bytes.Contains(encoded, []byte("NUDGE-SECRET-TEXT")) {
			t.Fatalf("resumed request carries the spent reminder: %s", encoded)
		}
	}
}

// A firing whose request was never answered is not spent: after a crash
// before the response committed, the resumed request carries it again.
func TestRequestReminderUnansweredFiresAgainAfterCrash(t *testing.T) {
	nudge := agent.Reminder{Name: "nudge", Text: "nudge"}
	store, err := journal.Create(context.Background(), t.TempDir(), "/workspace/project", journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	path := store.Path()
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("never journaled")))
	agt := newReminderAgent(f, &requestLog{}, nil, nudge)
	AttachJournal(agt, store)
	crashOn(t, store, agt, func(ev agent.AgentEvent) bool { return ev.Type == agent.EventModelRequest })

	resumed, loaded := resumeStore(t, path)
	if fires := loaded.LogicalReminders(loaded.LastRun.LogicalRunID); len(fires) != 0 {
		t.Fatalf("unanswered firing was counted: %v", fires)
	}
	f.SetResponses(faux.Respond(faux.Text("done")))
	log := &requestLog{}
	after := newReminderAgent(f, log, loaded.Messages, nudge)
	AttachJournal(after, resumed)
	if result, err := after.Continue(context.Background()); err != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("resumed run = %+v err=%v", result, err)
	}
	if requests := log.all(); len(requests) != 1 || requests[0].RequestScoped != 1 {
		t.Fatalf("resumed request did not carry the unanswered nudge: %+v", requests)
	}
}
