package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/progress"
	"go.harness.dev/harness/internal/schema"
)

// todoToolInput carries either a full list (todos) or one op.
type todoToolInput struct {
	Todos  []progress.TodoItem `json:"todos"`
	Op     string              `json:"op"`
	Phase  string              `json:"phase"`
	Items  []string            `json:"items"`
	Reason string              `json:"reason"`
}

// newTodoTool builds the todo writer, using options.TodoStore when supplied.
// Otherwise it checkpoints into the session data directory, never the
// workspace, and keeps todos in memory when the session has no directory.
func newTodoTool(options ToolsOptions) agent.AgentTool {
	store := options.TodoStore
	if store == nil {
		store = progress.NewStore(progress.Path(options.SessionDataDir))
	}
	return agent.AgentTool{
		Tool: types.Tool{
			Name: string(TodoTool),
			Description: "Track the session todo list, saved as the session's progress checkpoint outside the workspace. Send either todos, the full list that replaces the current one, or one op that names todos by their exact content: " +
				"init (items, optional phase) starts a new list; append (items, optional phase) adds pending todos; start (one item) marks it in progress and returns any other in-progress todo to pending; " +
				"done and drop (items, or a phase for all its todos) close todos; block (items or a phase, with reason) marks open todos blocked. The result shows the updated list.",
			Parameters: schema.ObjectRequired(schema.JSON{
				"todos": schema.JSON{
					"type": "array",
					"items": schema.JSON{
						"type": "object",
						"properties": schema.JSON{
							"id":       schema.String(),
							"content":  schema.String(),
							"status":   schema.JSON{"type": "string", "enum": progress.Statuses},
							"priority": schema.JSON{"type": "string", "enum": []string{"high", "medium", "low"}},
							"phase":    schema.String(),
							"blocker":  schema.String(),
						},
						"required": []string{"content", "status"},
					},
				},
				"op":     schema.JSON{"type": "string", "enum": progress.OpNames},
				"phase":  schema.String(),
				"items":  schema.JSON{"type": "array", "items": schema.String()},
				"reason": schema.String(),
			}, []string{}),
		},
		Label: string(TodoTool),
		Execute: func(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			var input todoToolInput
			if err := decodeParams(params, &input); err != nil {
				return agent.AgentToolResult{}, err
			}
			var todos []progress.TodoItem
			switch {
			case input.Op != "" && input.Todos != nil:
				return agent.AgentToolResult{}, errors.New("send either todos (the full list) or an op, not both")
			case input.Op != "":
				applied, err := store.Apply(ctx, progress.Op{Op: input.Op, Phase: input.Phase, Items: input.Items, Reason: input.Reason})
				if err != nil {
					return agent.AgentToolResult{}, err
				}
				todos = applied
			case input.Todos != nil:
				if err := store.Update(ctx, input.Todos); err != nil {
					return agent.AgentToolResult{}, err
				}
				todos = store.Todos()
			default:
				return agent.AgentToolResult{}, errors.New("todo_write needs todos (the full list) or an op")
			}
			var text strings.Builder
			text.WriteString(progress.RenderMarkdown(todos))
			if path := store.Path(); path != "" {
				text.WriteString("\nProgress checkpoint: " + path)
			}
			return textResult(text.String(), map[string]any{"todos": todos}), nil
		},
	}
}
