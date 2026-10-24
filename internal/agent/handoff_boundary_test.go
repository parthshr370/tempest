package agent

import (
	"context"
	"encoding/json"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
)

func TestHandoffPlacementRunsBeforeStoppedHookExit(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.ToolCall("handoff", json.RawMessage(`{}`), "call")))
	tool := AgentTool{Tool: types.Tool{Name: "handoff", Parameters: json.RawMessage(`{"type":"object"}`)}, Execute: func(context.Context, string, json.RawMessage, AgentToolUpdateCallback) (AgentToolResult, error) {
		return AgentToolResult{Content: []types.ContentBlock{types.NewText("next owner")}, Control: "handoff"}, nil
	}}
	var agt *Agent
	placed := false
	agt = NewAgent(AgentOptions{InitialState: &AgentState{Model: provider.Model(), Tools: []AgentTool{tool}}, StreamFn: provider.StreamSimple, Hooks: testCommandHooks(t, t.TempDir(), map[string]string{"PostToolUse": `printf '%s' '{"continue":false,"stopReason":"operator stop"}'`}), PrepareNextTurnE: func(_ context.Context, turn ShouldStopAfterTurnContext) (*AgentLoopTurnUpdate, error) {
		if turn.Handoff == nil {
			return nil, nil
		}
		placed = true
		agt.SetMessages(nil)
		next := turn.Context
		next.Messages = nil
		return &AgentLoopTurnUpdate{Context: &next, Stop: true}, nil
	}})
	result, err := agt.PromptText(t.Context(), "handoff and stop")
	if err != nil || result.Err != nil || !placed || len(agt.State().Messages) != 0 || provider.CallCount() != 1 {
		t.Fatal("handoff bypassed placement", result, err, placed)
	}
}
