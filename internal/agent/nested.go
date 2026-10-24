package agent

import (
	"context"
	"errors"

	"go.harness.dev/harness/internal/engine/types"
)

// ExecuteScopedTool uses ordinary validation, preparation, gate and result hooks.
// The caller owns its durable child key, intent and terminal result receipt.
func ExecuteScopedTool(ctx context.Context, tools []AgentTool, call types.ContentBlock, config AgentLoopConfig) (AgentToolResult, error) {
	current := AgentContext{Tools: tools}
	prepared, immediate := prepareToolCall(ctx, current, types.AssistantMessage{}, call, config)
	if immediate != nil {
		return immediate.result, errors.New(toolResultText(immediate.result))
	}
	emit := func(context.Context, AgentEvent) error { return nil }
	executed := executePreparedToolCall(ctx, prepared, emit)
	result := finalizeExecutedToolCall(ctx, current, types.AssistantMessage{}, prepared, executed, config)
	if result.isError {
		return result.result, errors.New(toolResultText(result.result))
	}
	return result.result, nil
}
func toolResultText(result AgentToolResult) string {
	for _, block := range result.Content {
		if block.Type == types.BlockText {
			return block.Text
		}
	}
	return "tool execution failed"
}
