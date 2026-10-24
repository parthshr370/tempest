package anthropic

import (
	"encoding/json"
	"fmt"
	"io"
)

// iterateMessage reuses the SSE fold so Vertex rawPredict keeps identical tool,
// signature, and usage handling without a second message converter.
func iterateMessage(reader io.Reader, onActivity func(), emit func(rawAnthropicEvent) error) error {
	var reply struct {
		ID      string `json:"id"`
		Content []struct {
			anthropicContentBlock
			Text      string `json:"text"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		} `json:"content"`
		Stop  string          `json:"stop_reason"`
		Usage anthropicUsage  `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(reader).Decode(&reply); err != nil {
		return err
	}
	onActivity()
	if len(reply.Error) > 0 && string(reply.Error) != "null" {
		return fmt.Errorf("Anthropic response error: %s", reply.Error)
	}
	if reply.Stop == "" {
		return fmt.Errorf("Anthropic response ended without stop_reason")
	}
	if err := emit(rawAnthropicEvent{Type: "message_start", Message: anthropicMessage{ID: reply.ID, Usage: reply.Usage}}); err != nil {
		return err
	}
	for index, block := range reply.Content {
		if err := emit(rawAnthropicEvent{Type: "content_block_start", Index: index, ContentBlock: block.anthropicContentBlock}); err != nil {
			return err
		}
		deltas := []anthropicDelta{}
		switch block.Type {
		case "text":
			deltas = append(deltas, anthropicDelta{Type: "text_delta", Text: block.Text})
		case "thinking":
			deltas = append(deltas, anthropicDelta{Type: "thinking_delta", Thinking: block.Thinking}, anthropicDelta{Type: "signature_delta", Signature: block.Signature})
		case "tool_use":
			deltas = append(deltas, anthropicDelta{Type: "input_json_delta", PartialJSON: string(block.Input)})
		}
		for _, delta := range deltas {
			if err := emit(rawAnthropicEvent{Type: "content_block_delta", Index: index, Delta: delta}); err != nil {
				return err
			}
		}
		if err := emit(rawAnthropicEvent{Type: "content_block_stop", Index: index}); err != nil {
			return err
		}
	}
	return emit(rawAnthropicEvent{Type: "message_delta", Delta: anthropicDelta{StopReason: reply.Stop}, Usage: reply.Usage})
}
