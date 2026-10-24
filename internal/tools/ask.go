package tools

import (
	"context"
	"encoding/json"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/question"
	"go.harness.dev/harness/internal/schema"
)

func newAskTool(sessionID string, handler question.Handler) agent.AgentTool {
	promptSlot := make(chan struct{}, 1)
	option := schema.Object(schema.JSON{"label": schema.String(), "description": schema.String()}, "label")
	item := schema.Object(schema.JSON{"id": schema.String(), "question": schema.String(), "options": schema.JSON{"type": "array", "items": option}, "multi": schema.Bool(), "allow_text": schema.Bool()}, "id", "question", "options")
	return agent.AgentTool{
		Tool: types.Tool{
			Name:        string(AskTool),
			Description: "Ask the user structured non-sensitive questions. Options have labels and descriptions; multi allows several choices; allow_text permits a written answer. Never ask for secrets. If no interactive client is available, proceed with your best judgment. Interrupted questions are not automatically asked again.",
			Parameters:  schema.Object(schema.JSON{"questions": schema.JSON{"type": "array", "items": item, "minItems": 1, "maxItems": 20}}, "questions"),
		},
		Label:         string(AskTool),
		ExecutionMode: agent.ExecSequential,
		Execute: func(ctx context.Context, id string, raw json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			var input struct {
				Questions []question.Question `json:"questions"`
			}
			if err := decodeParams(raw, &input); err != nil {
				return agent.AgentToolResult{}, err
			}
			if err := question.Validate(input.Questions); err != nil {
				return agent.AgentToolResult{}, err
			}
			if handler == nil {
				return agent.AgentToolResult{}, question.Unavailable
			}
			select {
			case promptSlot <- struct{}{}:
				defer func() { <-promptSlot }()
			case <-ctx.Done():
				return agent.AgentToolResult{}, ctx.Err()
			}
			if err := ctx.Err(); err != nil {
				return agent.AgentToolResult{}, err
			}
			answers, err := handler(ctx, question.Request{SessionID: sessionID, ToolCallID: id, Questions: input.Questions})
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			if err := question.ValidateAnswers(input.Questions, answers); err != nil {
				return agent.AgentToolResult{}, err
			}
			encoded, err := json.Marshal(answers)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			return textResult(string(encoded), answers), nil
		}}
}
