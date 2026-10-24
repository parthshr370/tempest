package tools

import (
	"context"
	"encoding/json"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/schema"
)

func newHistoryTool(search func(context.Context, string, string, int) (json.RawMessage, error)) agent.AgentTool {
	return agent.AgentTool{
		Tool:  types.Tool{Name: string(SearchHistoryTool), Description: "Search the session's immutable audit history, including text hidden by reset, rewind or compaction. Search is literal and case-insensitive. Follow nextCursor even when a page has no matches. Results don't change active context.", Parameters: schema.Object(schema.JSON{"query": schema.String(), "cursor": schema.String(), "limit": schema.JSON{"type": "integer", "minimum": 1, "maximum": 100}}, "query")},
		Label: string(SearchHistoryTool),
		Execute: func(ctx context.Context, _ string, raw json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			var input struct {
				Query  string `json:"query"`
				Cursor string `json:"cursor"`
				Limit  int    `json:"limit"`
			}
			if err := decodeParams(raw, &input); err != nil {
				return agent.AgentToolResult{}, err
			}
			page, err := search(ctx, input.Query, input.Cursor, input.Limit)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			return textResult(string(page), json.RawMessage(page)), nil
		},
	}
}
