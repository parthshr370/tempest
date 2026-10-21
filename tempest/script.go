package tempest

import (
	"encoding/json"
	"fmt"
	"os"

	"go.harness.dev/harness/internal/agent"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
)

func loadScript(path string) (types.Model, agent.StreamFn, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return types.Model{}, nil, err
	}
	var script []struct {
		Text      string `json:"text"`
		ToolCalls []struct {
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"toolCalls"`
	}
	if err := json.Unmarshal(raw, &script); err != nil {
		return types.Model{}, nil, fmt.Errorf("parse faux script: %w", err)
	}
	if len(script) == 0 {
		return types.Model{}, nil, fmt.Errorf("faux script has no responses")
	}
	provider := faux.New(faux.Options{})
	for _, response := range script {
		var blocks []types.ContentBlock
		if response.Text != "" {
			blocks = append(blocks, faux.Text(response.Text))
		}
		for _, call := range response.ToolCalls {
			if call.Name == "" {
				return types.Model{}, nil, fmt.Errorf("faux script tool call missing name")
			}
			args := call.Arguments
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			blocks = append(blocks, faux.ToolCall(call.Name, args, call.ID))
		}
		provider.AppendResponses(faux.Respond(blocks...))
	}
	return provider.Model(), provider.StreamSimple, nil
}
