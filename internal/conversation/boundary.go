// Package conversation handles provider-valid transcript boundaries.
package conversation

import (
	"encoding/json"
	"fmt"

	"go.harness.dev/harness/internal/engine/types"
)

// Boundary tracks a complete tool round, not an individual tool result.
type Boundary struct{ pending map[string]int }

// Add reports whether the transcript ends without an unanswered tool call.
func (b *Boundary) Add(message types.Message) bool {
	if b.pending == nil {
		b.pending = make(map[string]int)
	}
	switch m := message.(type) {
	case types.AssistantMessage:
		if m.StopReason != types.StopError && m.StopReason != types.StopAborted {
			for _, block := range m.Content {
				if block.Type == types.BlockToolCall {
					b.pending[block.ID]++
				}
			}
		}
	case types.ToolResultMessage:
		if b.pending[m.ToolCallID] > 1 {
			b.pending[m.ToolCallID]--
		} else {
			delete(b.pending, m.ToolCallID)
		}
	}
	return len(b.pending) == 0
}

// Clone detaches a captured prefix from mutable provider-owned message buffers.
func Clone(messages []types.Message) ([]types.Message, error) {
	out := make([]types.Message, 0, len(messages))
	for _, message := range messages {
		raw, err := json.Marshal(message)
		if err != nil {
			return nil, err
		}
		value, err := types.UnmarshalMessage(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}

// Safe reports whether all calls in the prefix have results.
func Safe(messages []types.Message) error {
	var boundary Boundary
	safe := true
	for _, message := range messages {
		safe = boundary.Add(message)
	}
	if !safe {
		return fmt.Errorf("fork entry interrupts a tool round")
	}
	return nil
}
