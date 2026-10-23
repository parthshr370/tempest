package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/outputschema"
	"go.harness.dev/harness/internal/provider/faux"
)

const answerSchema = `{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}`

// schemaAgent runs the faux provider's scripted replies and records the
// output schema each provider call carried.
func schemaAgent(t *testing.T, replies ...string) (*Agent, *[]json.RawMessage) {
	t.Helper()
	provider := faux.New(faux.Options{})
	steps := make([]faux.ResponseStep, 0, len(replies))
	for _, reply := range replies {
		steps = append(steps, faux.Respond(faux.Text(reply)))
	}
	provider.SetResponses(steps...)
	var mu sync.Mutex
	var sent []json.RawMessage
	agent := NewAgent(AgentOptions{
		InitialState: &AgentState{Model: provider.Model()},
		ConvertToLlm: func(messages []types.Message) []types.Message { return messages },
		StreamFn: func(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
			mu.Lock()
			sent = append(sent, opts.OutputSchema)
			mu.Unlock()
			return provider.StreamSimple(ctx, model, c, opts)
		},
	})
	schema, err := outputschema.Compile(json.RawMessage(answerSchema))
	if err != nil {
		t.Fatal(err)
	}
	agent.SetOutputSchema(schema)
	return agent, &sent
}

func userTexts(messages []types.Message) []string {
	var texts []string
	for _, message := range messages {
		if user, ok := message.(types.UserMessage); ok {
			for _, block := range user.Content.Blocks {
				texts = append(texts, block.Text)
			}
		}
	}
	return texts
}

func TestOutputSchemaValidAnswerLandsInResult(t *testing.T) {
	agent, sent := schemaAgent(t, `{ "answer": 42 }`)
	result, err := agent.PromptText(context.Background(), "what is it")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != StopCompleted || string(result.Output) != `{"answer":42}` || result.Turns != 1 {
		t.Fatalf("result = %+v output=%s", result, result.Output)
	}
	if len(*sent) != 1 || string((*sent)[0]) != answerSchema {
		t.Fatalf("provider schema = %q", *sent)
	}
}

// An invalid answer gets one correction turn naming the validation error;
// a valid correction completes the run.
func TestOutputSchemaCorrectionTurnRecovers(t *testing.T) {
	agent, _ := schemaAgent(t, "It is 42.", `{"answer":42}`)
	result, err := agent.PromptText(context.Background(), "what is it")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != StopCompleted || string(result.Output) != `{"answer":42}` || result.Turns != 2 {
		t.Fatalf("result = %+v", result)
	}
	texts := userTexts(agent.State().Messages)
	if len(texts) != 2 || !strings.Contains(texts[1], "does not satisfy the required output schema") || !strings.Contains(texts[1], answerSchema) {
		t.Fatalf("user messages = %q", texts)
	}
}

func TestOutputSchemaStillInvalidStopsTyped(t *testing.T) {
	agent, _ := schemaAgent(t, "It is 42.", `{"answer":"forty-two"}`, `{"answer":42}`)
	result, err := agent.PromptText(context.Background(), "what is it")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != StopOutputInvalid || result.Output != nil || result.Turns != 2 || result.Err == nil || !strings.Contains(result.Err.Error(), "output schema") {
		t.Fatalf("result = %+v", result)
	}

	// The schema belongs to the caller's runs; clearing it returns to text.
	agent.SetOutputSchema(nil)
	result, err = agent.PromptText(context.Background(), "again")
	if err != nil || result.StopReason != StopCompleted || result.Output != nil {
		t.Fatalf("free-text run = %+v %v", result, err)
	}
}

func TestOutputSchemaCompileRejectsExternalReferences(t *testing.T) {
	for _, document := range []string{`{"$ref":"https://example.com/s.json"}`, `[]`, `{"type":"object"`, strings.Repeat(" ", outputschema.MaxSchemaBytes+1)} {
		if _, err := outputschema.Compile(json.RawMessage(document)); err == nil {
			t.Errorf("accepted %.40q", document)
		}
	}
	schema, err := outputschema.Compile(json.RawMessage(answerSchema))
	if err != nil {
		t.Fatal(err)
	}
	for _, answer := range []string{`{"answer":1} trailing`, "```json\n{\"answer\":1}\n```", `{"answer":1,"extra":true}`, strings.Repeat("1", outputschema.MaxAnswerBytes+1)} {
		if _, err := schema.Validate(answer); err == nil {
			t.Errorf("validated %.40q", answer)
		}
	}
	if _, err := schema.Validate(" {\"answer\": 7}\n"); err != nil {
		t.Fatal(errors.Join(errors.New("surrounding whitespace rejected"), err))
	}
}
