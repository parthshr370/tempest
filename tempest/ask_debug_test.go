package tempest

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
)

func TestAskSDKAnswerJournaledBeforeResume(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.AppendResponses(faux.Respond(faux.ToolCall("ask", json.RawMessage(`{"questions":[{"id":"q","question":"Choose","options":[{"label":"one","description":"First"}],"allow_text":true}]}`), "question")), faux.Respond(faux.Text("done")))
	var session *Session
	calls := 0
	options := Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}, Questioner: func(_ context.Context, request QuestionRequest) ([]QuestionAnswer, error) {
		calls++
		raw, err := os.ReadFile(session.core.store.Path())
		if err != nil {
			return nil, err
		}
		if !bytes.Contains(raw, []byte("tool_started")) {
			t.Error("question presented before durable tool intent")
		}
		if request.SessionID != session.ID() || request.ToolCallID != "question" {
			t.Errorf("request=%+v", request)
		}
		return []QuestionAnswer{{ID: "q", Selected: []string{"one"}, Text: "user context"}}, nil
	}}
	var err error
	session, err = Open(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Prompt(t.Context(), Prompt{Text: "ask"}); err != nil {
		t.Fatal(err)
	}
	path := session.core.store.Path()
	if err := session.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	loaded, err := journal.Load(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range loaded.Messages {
		if result, ok := message.(types.ToolResultMessage); ok && result.ToolName == "ask" {
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			found = !result.IsError && bytes.Contains(encoded, []byte("user context"))
		}
	}
	if !found || calls != 1 {
		t.Fatalf("answer persisted=%t callbacks=%d", found, calls)
	}
	options.New = nil
	options.Resume = &ResumeSession{Path: path}
	resumed, err := Open(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("resume re-asked completed question")
	}
}

func TestAskDebugChildToolCeilingAndKind(t *testing.T) {
	if !slices.Contains(childToolNames("general", []string{"debug", "ask"}), "debug") {
		t.Fatal("general child lost inherited debug")
	}
	for _, kind := range []string{"general", "explore", "scout"} {
		if slices.Contains(childToolNames(kind, []string{"ask"}), "ask") {
			t.Fatalf("%s child can ask a user without a relay", kind)
		}
	}
	if slices.Contains(childToolNames("general", []string{"read"}), "debug") ||
		slices.Contains(childToolNames("explore", []string{"debug"}), "debug") {
		t.Fatal("debug escaped the child tool ceiling")
	}
	if _, kind := toolDescription("debug", json.RawMessage(`{"action":"evaluate"}`)); kind != "execute" {
		t.Fatalf("debug kind = %s", kind)
	}
}
