package google

import (
	"encoding/json"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

func TestOutputSchemaSetsResponseJSONSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`)
	ctx := types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}
	params := buildParams(baseModel(), ctx, &Options{StreamOptions: types.StreamOptions{OutputSchema: schema}})
	config := params["generationConfig"].(map[string]any)
	sent, _ := json.Marshal(config["responseJsonSchema"])
	if config["responseMimeType"] != "application/json" || string(sent) != string(schema) {
		t.Fatalf("generationConfig = %#v", config)
	}
	if _, present := config["responseSchema"]; present {
		t.Fatalf("both schema fields sent: %#v", config)
	}
	config = buildParams(baseModel(), ctx, &Options{})["generationConfig"].(map[string]any)
	if _, present := config["responseMimeType"]; present {
		t.Fatalf("JSON mode without a schema: %#v", config)
	}
}

func imageToolContext(model types.Model) types.Context {
	return types.Context{Messages: []types.Message{
		types.UserMessage{Content: types.StringContent("look")},
		types.AssistantMessage{API: model.API, Provider: model.Provider, Model: model.ID, Content: []types.ContentBlock{
			types.NewToolCall("tc1", "view_image", json.RawMessage(`{"path":"a.png"}`)),
			types.NewToolCall("tc2", "read", json.RawMessage(`{"path":"b"}`)),
		}},
		types.ToolResultMessage{ToolCallID: "tc1", ToolName: "view_image", Content: []types.ContentBlock{types.NewText("Image a.png"), types.NewImage("aGk=", "image/png")}},
		types.ToolResultMessage{ToolCallID: "tc2", ToolName: "read", Content: []types.ContentBlock{types.NewText("text")}},
	}}
}

// Gemini 3 takes a tool result's image inside the functionResponse.
func TestGemini3ToolImageInsideFunctionResponse(t *testing.T) {
	model := baseModel()
	model.ID = "gemini-3.5-flash"
	model.Input = []string{"text", "image"}
	contents := convertMessages(imageToolContext(model).Messages, model)
	if len(contents) != 3 {
		t.Fatalf("contents = %#v", contents)
	}
	parts := contents[2]["parts"].([]map[string]any)
	response := parts[0]["functionResponse"].(map[string]any)
	images, _ := response["parts"].([]map[string]any)
	if len(parts) != 2 || len(images) != 1 || images[0]["inlineData"].(map[string]any)["data"] != "aGk=" {
		t.Fatalf("function responses = %#v", parts)
	}
}

// Older Gemini models need the image in a user turn after all the batch's
// function responses.
func TestOlderGeminiToolImageFollowsResponses(t *testing.T) {
	model := baseModel()
	model.Input = []string{"text", "image"}
	contents := convertMessages(imageToolContext(model).Messages, model)
	if len(contents) != 4 {
		t.Fatalf("contents = %#v", contents)
	}
	responses := contents[2]["parts"].([]map[string]any)
	if len(responses) != 2 || responses[0]["functionResponse"].(map[string]any)["parts"] != nil {
		t.Fatalf("function responses = %#v", responses)
	}
	imageTurn := contents[3]["parts"].([]map[string]any)
	if contents[3]["role"] != "user" || len(imageTurn) != 2 || imageTurn[1]["inlineData"].(map[string]any)["mimeType"] != "image/png" {
		t.Fatalf("image turn = %#v", contents[3])
	}
}

func TestMultimodalFunctionResponseVersions(t *testing.T) {
	for id, want := range map[string]bool{"gemini-3.5-flash": true, "gemini-10-pro": true, "gemini-2.5-flash": false, "gemini-flash-latest": true, "claude-x": true} {
		if got := multimodalFunctionResponse(id); got != want {
			t.Errorf("%s: %v", id, got)
		}
	}
}
