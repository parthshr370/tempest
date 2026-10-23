package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
)

// recordingSession opens a session whose provider records each request's
// context and options before the faux provider answers it.
func recordingSession(t *testing.T, f *faux.Faux) (*Session, func() ([]types.Context, []types.SimpleStreamOptions)) {
	t.Helper()
	var mu sync.Mutex
	var contexts []types.Context
	var options []types.SimpleStreamOptions
	streamFn := func(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
		mu.Lock()
		contexts = append(contexts, c)
		options = append(options, *opts)
		mu.Unlock()
		return f.StreamSimple(ctx, model, c, opts)
	}
	s, err := Open(context.Background(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{}, Stack: &StackConfig{Model: f.Model(), StreamFn: streamFn}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s, func() ([]types.Context, []types.SimpleStreamOptions) {
		mu.Lock()
		defer mu.Unlock()
		return contexts, options
	}
}

func lastUserBlocks(c types.Context) []types.ContentBlock {
	for i := len(c.Messages) - 1; i >= 0; i-- {
		if user, ok := c.Messages[i].(types.UserMessage); ok {
			return user.Content.Blocks
		}
	}
	return nil
}

func TestPromptOutputSchemaReturnsValidatedOutput(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text(`{"answer": 42}`)), faux.Respond(faux.Text("free text")))
	s, requests := recordingSession(t, f)
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"]}`)
	result, err := s.Prompt(t.Context(), Prompt{Text: "answer", OutputSchema: schema})
	if err != nil || result.StopReason != agent.StopCompleted || string(result.Output) != `{"answer":42}` {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	contexts, options := requests()
	blocks := lastUserBlocks(contexts[0])
	if len(blocks) != 2 || blocks[0].Text != "answer" || !strings.Contains(blocks[1].Text, `"answer"`) {
		t.Fatalf("prompt blocks = %#v", blocks)
	}
	if string(options[0].OutputSchema) != `{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"]}` {
		t.Fatalf("provider schema = %s", options[0].OutputSchema)
	}

	// The next prompt has no schema: free text, nothing sent to the provider.
	result, err = s.Prompt(t.Context(), Prompt{Text: "chat"})
	if err != nil || result.Output != nil {
		t.Fatalf("free run=%+v err=%v", result, err)
	}
	if _, options = requests(); options[1].OutputSchema != nil {
		t.Fatalf("schema leaked into the next run: %s", options[1].OutputSchema)
	}
}

func TestPromptRejectsInvalidOutputSchema(t *testing.T) {
	s, _ := recordingSession(t, faux.New(faux.Options{}))
	if _, err := s.Begin(t.Context(), Prompt{Text: "x", OutputSchema: json.RawMessage(`{"$ref":"file:///etc/passwd"}`)}); !errors.Is(err, ErrInvalidOutputSchema) {
		t.Fatalf("err = %v", err)
	}
}

// Prompt images (the ACP image block path) reach a vision model's request as
// image blocks after the prompt text.
func TestPromptImagesReachVisionModel(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("a red dot")))
	s, requests := recordingSession(t, f)
	if _, err := s.Prompt(t.Context(), Prompt{Text: "what is this", Media: []Media{types.NewImage("aGk=", "image/png")}}); err != nil {
		t.Fatal(err)
	}
	contexts, _ := requests()
	blocks := lastUserBlocks(contexts[0])
	if len(blocks) != 2 || blocks[1].Type != types.BlockImage || blocks[1].Data != "aGk=" || blocks[1].MimeType != "image/png" {
		t.Fatalf("request blocks = %#v", blocks)
	}
}
