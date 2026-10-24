package agent

import (
	"context"
	"encoding/json"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/durable"
)

// ExtensionRuntime prepares one immutable tool/prompt generation for a provider turn.
// Its lease remains pinned through all tool calls from that response.
type ExtensionRuntime interface {
	Prepare(context.Context, AgentContext) (context.Context, AgentContext, func(), error)
	BeforeTool(context.Context, types.ContentBlock) (json.RawMessage, error)
	AfterTool(context.Context, types.ContentBlock, AgentToolResult) (AgentToolResult, error)
	AfterResponse(context.Context, types.AssistantMessage) error
	Yield(context.Context) (string, error)
	BeforeCompaction(context.Context) error
}

// ToolDefinition is the immutable replay evidence attached to execution intent.
type ToolDefinition struct {
	Identity      string               `json:"identity,omitempty"`
	Revision      string               `json:"revision,omitempty"`
	Environment   string               `json:"environment,omitempty"`
	Replay        durable.ReplayPolicy `json:"replay,omitempty"`
	SchemaHash    string               `json:"schemaHash,omitempty"`
	ArgumentsHash string               `json:"argumentsHash,omitempty"`
	Effect        string               `json:"effect,omitempty"`
}

func hookInvocationKey(ctx context.Context, event string) string {
	if invocation, ok := work.InvocationFromContext(ctx); ok && invocation.CallID != "" {
		return invocation.CallID + ":" + event
	}
	return ""
}
