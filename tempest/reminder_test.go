package tempest

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/engine/stream"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
)

// reminderRequests keeps the messages and request-scope count of every
// provider request.
type reminderRequests struct {
	mu       sync.Mutex
	requests []types.Context
}

func (r *reminderRequests) stream(f *faux.Faux) agent.StreamFn {
	return func(ctx context.Context, model types.Model, request types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
		r.mu.Lock()
		copied := request
		copied.Messages = slices.Clone(request.Messages)
		r.requests = append(r.requests, copied)
		r.mu.Unlock()
		return f.StreamSimple(ctx, model, request, options)
	}
}

func (r *reminderRequests) all() []types.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requests)
}

func encodedMessages(t *testing.T, messages []types.Message) []byte {
	t.Helper()
	raw, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func reminderStack(f *faux.Faux, recorder *reminderRequests) *StackConfig {
	return &StackConfig{Model: f.Model(), StreamFn: recorder.stream(f), ListingPolicy: config.WorkspaceListing{Disabled: true}}
}

// A request-scope reminder reaches its own request only: the journal file
// never holds its text, and after resume neither the transcript nor any
// request carries it.
func TestReminderRequestScopeNotPersistedAcrossResume(t *testing.T) {
	cwd, sessions := t.TempDir(), t.TempDir()
	f := faux.New(faux.Options{})
	recorder := &reminderRequests{}
	f.SetResponses(faux.Respond(faux.Text("first")))
	s, err := Open(context.Background(), Options{SessionDir: sessions, Cwd: cwd, New: &NewSession{}, Stack: reminderStack(f, recorder),
		Reminders: []Reminder{{Name: "nudge", Text: "SDK-NUDGE-TEXT"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(context.Background(), Prompt{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if requests := recorder.all(); len(requests) != 1 || requests[0].RequestScoped != 1 || !bytes.Contains(encodedMessages(t, requests[0].Messages), []byte("SDK-NUDGE-TEXT")) {
		t.Fatalf("first request did not carry the reminder: %+v", requests)
	}
	path := s.core.store.Path()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("SDK-NUDGE-TEXT")) {
		t.Fatalf("journal holds the request-scope reminder:\n%s", raw)
	}

	recorder = &reminderRequests{}
	f.SetResponses(faux.Respond(faux.Text("second")))
	resumed, err := Open(context.Background(), Options{Resume: &ResumeSession{Path: path}, SessionDir: sessions, Stack: reminderStack(f, recorder), Reminders: []Reminder{}})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close(context.Background())
	if _, err := resumed.Prompt(context.Background(), Prompt{Text: "again"}); err != nil {
		t.Fatal(err)
	}
	requests := recorder.all()
	if len(requests) != 1 || requests[0].RequestScoped != 0 || bytes.Contains(encodedMessages(t, requests[0].Messages), []byte("SDK-NUDGE-TEXT")) {
		t.Fatalf("resumed request carries the reminder: %s", encodedMessages(t, requests[0].Messages))
	}
}

// A conversation reminder is journaled with the transcript and replayed
// exactly once after resume, at the place it fired.
func TestReminderConversationScopeReplayedOnceAfterResume(t *testing.T) {
	cwd, sessions := t.TempDir(), t.TempDir()
	f := faux.New(faux.Options{})
	recorder := &reminderRequests{}
	f.SetResponses(faux.Respond(faux.Text("first")))
	rule := Reminder{Name: "rule", Text: "SDK-RULE-TEXT", Scope: ReminderConversation}
	s, err := Open(context.Background(), Options{SessionDir: sessions, Cwd: cwd, New: &NewSession{}, Stack: reminderStack(f, recorder), Reminders: []Reminder{rule}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(context.Background(), Prompt{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	fired := recorder.all()[0].Messages
	path := s.core.store.Path()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	recorder = &reminderRequests{}
	f.SetResponses(faux.Respond(faux.Text("second")))
	resumed, err := Open(context.Background(), Options{Resume: &ResumeSession{Path: path}, SessionDir: sessions, Stack: reminderStack(f, recorder), Reminders: []Reminder{}})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close(context.Background())
	if _, err := resumed.Prompt(context.Background(), Prompt{Text: "again"}); err != nil {
		t.Fatal(err)
	}
	sent := recorder.all()[0].Messages
	if copies := bytes.Count(encodedMessages(t, sent), []byte("SDK-RULE-TEXT")); copies != 1 {
		t.Fatalf("resumed request holds %d copies, want 1:\n%s", copies, encodedMessages(t, sent))
	}
	// The resumed request starts with the bytes the first request sent.
	if len(sent) < len(fired) || !bytes.Equal(encodedMessages(t, sent[:len(fired)]), encodedMessages(t, fired)) {
		t.Fatalf("resumed prefix differs:\n%s\n%s", encodedMessages(t, fired), encodedMessages(t, sent))
	}
}

func TestReminderInvalidOptionsRefuseOpen(t *testing.T) {
	f := faux.New(faux.Options{})
	_, err := Open(context.Background(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{}, Stack: reminderStack(f, &reminderRequests{}),
		Reminders: []Reminder{{Name: "a", Text: "x"}, {Name: "a", Text: "y"}}})
	if err == nil || !strings.Contains(err.Error(), `reminder "a": name is used twice`) {
		t.Fatalf("Open = %v, want the duplicate name named", err)
	}
}
