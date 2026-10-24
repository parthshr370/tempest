package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
)

// fauxScriptResponse mirrors one scripted provider turn so the CLI can drive hermetic runs without code.
type fauxScriptResponse struct {
	Text        string               `json:"text"`
	ToolCalls   []fauxScriptToolCall `json:"toolCalls"`
	Error       string               `json:"error,omitempty"`
	WaitForFile string               `json:"waitForFile,omitempty"`
}

// fauxScriptToolCall keeps raw JSON arguments so the script passes them to the faux provider unchanged.
type fauxScriptToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// loadFauxScript turns the CLI's JSON response script into a faux provider, keeping one entry per model turn.
func loadFauxScript(path string) (*faux.Faux, types.Model, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, types.Model{}, err
	}
	var responses []fauxScriptResponse
	if err := json.Unmarshal(data, &responses); err != nil {
		return nil, types.Model{}, fmt.Errorf("parse faux script: %w", err)
	}
	if len(responses) == 0 {
		return nil, types.Model{}, fmt.Errorf("faux script has no responses")
	}

	provider := faux.New(faux.Options{})
	steps := make([]faux.ResponseStep, 0, len(responses))
	for _, response := range responses {
		blocks := make([]types.ContentBlock, 0, 1+len(response.ToolCalls))
		if response.Text != "" {
			blocks = append(blocks, faux.Text(response.Text))
		}
		for _, call := range response.ToolCalls {
			if call.Name == "" {
				return nil, types.Model{}, fmt.Errorf("faux script tool call missing name")
			}
			args := call.Arguments
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			blocks = append(blocks, faux.ToolCall(call.Name, args, call.ID))
		}
		respond := faux.Respond(blocks...)
		steps = append(steps, func(c types.Context, opts *types.StreamOptions, state faux.State, model types.Model) (types.AssistantMessage, error) {
			if response.WaitForFile != "" {
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					_, err := os.Stat(response.WaitForFile)
					if err == nil {
						break
					}
					if !os.IsNotExist(err) {
						return types.AssistantMessage{}, err
					}
					select {
					case <-state.Context.Done():
						return types.AssistantMessage{}, state.Context.Err()
					case <-ticker.C:
					}
				}
			}
			if response.Error != "" {
				return types.AssistantMessage{}, errors.New(response.Error)
			}
			return respond(c, opts, state, model)
		})
	}
	provider.SetResponses(steps...)
	return provider, provider.Model(), nil
}
