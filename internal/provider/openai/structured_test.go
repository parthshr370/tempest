package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

// captureRequests serves a one-word stream and records each request body.
func captureRequests(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		bodies = append(bodies, body)
		writeTextOpenAIStream(w, "ok")
	}))
	t.Cleanup(server.Close)
	return server, &bodies
}

func hiContext() types.Context {
	return types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}
}

func TestOutputSchemaSendsStrictResponseFormat(t *testing.T) {
	server, bodies := captureRequests(t)
	model := baseModel()
	model.BaseURL = server.URL
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`)
	drainAssistantStream(t, StreamSimple(context.Background(), model, hiContext(), &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{APIKey: "sk-test", OutputSchema: schema}}))
	format, ok := (*bodies)[0]["response_format"].(map[string]any)
	if !ok || format["type"] != "json_schema" {
		t.Fatalf("response_format = %#v", (*bodies)[0]["response_format"])
	}
	spec := format["json_schema"].(map[string]any)
	var want any
	if err := json.Unmarshal(schema, &want); err != nil {
		t.Fatal(err)
	}
	if spec["name"] != "final_answer" || spec["strict"] != true || !reflect.DeepEqual(spec["schema"], want) {
		t.Fatalf("json_schema = %#v", spec)
	}

	drainAssistantStream(t, StreamSimple(context.Background(), model, hiContext(), &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{APIKey: "sk-test"}}))
	if _, present := (*bodies)[1]["response_format"]; present {
		t.Fatalf("response_format sent without a schema: %#v", (*bodies)[1])
	}
}

// OpenRouter takes reasoning as {reasoning:{effort}}; OpenAI's own endpoint
// takes the top-level reasoning_effort. A model the catalog lists without
// reasoning gets neither.
func TestReasoningEffortWireFormPerEndpoint(t *testing.T) {
	server, bodies := captureRequests(t)
	opts := &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{APIKey: "sk-test"}, Reasoning: "low"}

	direct := baseModel()
	direct.BaseURL = server.URL
	direct.Reasoning = true
	drainAssistantStream(t, StreamSimple(context.Background(), direct, hiContext(), opts))
	if got := (*bodies)[0]; got["reasoning_effort"] != "low" || got["reasoning"] != nil {
		t.Fatalf("openai body = %#v", got)
	}

	routed := direct
	routed.Provider = "openrouter"
	routed.ID = "openai/gpt-5.5"
	drainAssistantStream(t, StreamSimple(context.Background(), routed, hiContext(), opts))
	reasoning, _ := (*bodies)[1]["reasoning"].(map[string]any)
	if reasoning["effort"] != "low" || (*bodies)[1]["reasoning_effort"] != nil {
		t.Fatalf("openrouter body = %#v", (*bodies)[1])
	}

	plain := routed
	plain.Reasoning = false
	drainAssistantStream(t, StreamSimple(context.Background(), plain, hiContext(), opts))
	if got := (*bodies)[2]; got["reasoning"] != nil || got["reasoning_effort"] != nil {
		t.Fatalf("non-reasoning model got a reasoning field: %#v", got)
	}
}

// Chat-completions tool messages carry only text, so a tool result's image
// follows the tool messages as one user message with an image_url part.
func TestToolResultImagesFollowAsUserMessage(t *testing.T) {
	model := baseModel()
	model.Input = []string{"text", "image"}
	ctx := types.Context{Messages: []types.Message{
		types.UserMessage{Content: types.StringContent("look")},
		types.AssistantMessage{API: model.API, Provider: model.Provider, Model: model.ID, Content: []types.ContentBlock{
			types.NewToolCall("tc1", "view_image", json.RawMessage(`{"path":"a.png"}`)),
			types.NewToolCall("tc2", "read", json.RawMessage(`{"path":"b"}`)),
		}},
		types.ToolResultMessage{ToolCallID: "tc1", ToolName: "view_image", Content: []types.ContentBlock{types.NewText("Image a.png"), types.NewImage("aGk=", "image/png")}},
		types.ToolResultMessage{ToolCallID: "tc2", ToolName: "read", Content: []types.ContentBlock{types.NewText("text")}},
	}}
	messages := convertMessages(model, ctx)
	roles := []string{}
	for _, message := range messages {
		roles = append(roles, message["role"].(string))
	}
	if len(roles) != 5 || roles[2] != "tool" || roles[3] != "tool" || roles[4] != "user" {
		t.Fatalf("roles = %v", roles)
	}
	if messages[2]["content"] != "Image a.png" {
		t.Fatalf("tool message = %#v", messages[2])
	}
	parts := messages[4]["content"].([]map[string]any)
	if len(parts) != 2 || parts[1]["type"] != "image_url" || parts[1]["image_url"].(map[string]any)["url"] != "data:image/png;base64,aGk=" {
		t.Fatalf("image message = %#v", parts)
	}

	// A text-only model gets the transform's placeholder, never an image part.
	model.Input = []string{"text"}
	messages = convertMessages(model, ctx)
	if last := messages[len(messages)-1]; last["role"] != "tool" {
		t.Fatalf("text-only model got an image message: %#v", last)
	}
}
