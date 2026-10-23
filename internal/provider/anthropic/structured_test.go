package anthropic

import (
	"context"
	"encoding/json"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

// output_config.format carries the final-answer schema next to the adaptive
// thinking effort; neither may overwrite the other.
func TestOutputSchemaJoinsOutputConfig(t *testing.T) {
	forceAdaptive := true
	model := baseModel()
	model.Reasoning = true
	model.Compat = &types.AnthropicCompat{ForceAdaptiveThinking: &forceAdaptive}
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`)
	ctx := types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}

	params, err := buildParams(context.Background(), model, ctx, false, &Options{StreamOptions: types.StreamOptions{OutputSchema: schema}, ThinkingEnabled: boolPtr(true), Effort: EffortLow})
	if err != nil {
		t.Fatal(err)
	}
	outputConfig := params["output_config"].(map[string]any)
	format := outputConfig["format"].(map[string]any)
	sent, _ := json.Marshal(format["schema"])
	if outputConfig["effort"] != "low" || format["type"] != "json_schema" || string(sent) != string(schema) {
		t.Fatalf("output_config = %#v", outputConfig)
	}

	params, err = buildParams(context.Background(), baseModel(), ctx, false, &Options{StreamOptions: types.StreamOptions{OutputSchema: schema}})
	if err != nil {
		t.Fatal(err)
	}
	if format := params["output_config"].(map[string]any)["format"]; format == nil {
		t.Fatalf("no format without thinking: %#v", params["output_config"])
	}

	params, err = buildParams(context.Background(), baseModel(), ctx, false, &Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := params["output_config"]; present {
		t.Fatalf("output_config without a schema or effort: %#v", params["output_config"])
	}
}

// A tool result's image travels inside the tool_result block, which the
// Messages API accepts natively.
func TestToolResultImageStaysInToolResult(t *testing.T) {
	model := baseModel()
	model.Input = []string{"text", "image"}
	ctx := types.Context{Messages: []types.Message{
		types.UserMessage{Content: types.StringContent("look")},
		types.AssistantMessage{API: model.API, Provider: model.Provider, Model: model.ID, Content: []types.ContentBlock{types.NewToolCall("tc1", "view_image", json.RawMessage(`{"path":"a.png"}`))}},
		types.ToolResultMessage{ToolCallID: "tc1", ToolName: "view_image", Content: []types.ContentBlock{types.NewText("Image a.png"), types.NewImage("aGk=", "image/png")}},
	}}
	params, err := buildParams(context.Background(), model, ctx, false, &Options{})
	if err != nil {
		t.Fatal(err)
	}
	messages := params["messages"].([]map[string]any)
	encoded, _ := json.Marshal(messages[len(messages)-1])
	var last struct {
		Role    string `json:"role"`
		Content []struct {
			Type    string `json:"type"`
			Content []struct {
				Type   string `json:"type"`
				Source struct {
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				} `json:"source"`
			} `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(encoded, &last); err != nil {
		t.Fatal(err)
	}
	if last.Role != "user" || len(last.Content) != 1 || last.Content[0].Type != "tool_result" {
		t.Fatalf("last message = %s", encoded)
	}
	blocks := last.Content[0].Content
	if len(blocks) != 2 || blocks[1].Type != "image" || blocks[1].Source.MediaType != "image/png" || blocks[1].Source.Data != "aGk=" {
		t.Fatalf("tool_result content = %s", encoded)
	}
}
