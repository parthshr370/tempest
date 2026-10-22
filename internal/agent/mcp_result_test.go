package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

func TestMCPErrorContentPreserved(t *testing.T) {
	content := []types.ContentBlock{types.NewText("remote failure with useful content"), types.NewImage("image-data", "image/png")}
	details := json.RawMessage(`{"mcp":{"server":"fixture","tool":"error","isError":true,"truncated":false}}`)
	tool := executorTool("mcp__fixture__error", "", ExecSequential, func(context.Context, string, json.RawMessage, AgentToolUpdateCallback) (AgentToolResult, error) {
		return AgentToolResult{Content: content, Details: details, IsError: true}, nil
	})
	assistant := types.AssistantMessage{Content: []types.ContentBlock{executorToolCall("call", "mcp__fixture__error", `{}`)}}
	sink := &recordingSink{}
	batch, err := executeToolCalls(context.Background(), AgentContext{Tools: []AgentTool{tool}}, assistant, AgentLoopConfig{}, sink.sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Messages) != 1 || !batch.Messages[0].IsError || !reflect.DeepEqual(batch.Messages[0].Content, content) || string(batch.Messages[0].Details) != string(details) {
		t.Fatalf("lost protocol error result: %#v", batch.Messages)
	}
	if batch.Terminate {
		t.Fatal("protocol error terminated run")
	}
}
