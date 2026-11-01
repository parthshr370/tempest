package telemetry

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
)

// Message content is captured only when the operator sets
// OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true. It is encoded at
// export time on the exporter goroutine, never on the turn loop, in the
// OpenTelemetry GenAI message format (role plus typed parts).

// maxCapturedContentBytes caps one captured attribute; a long transcript
// would otherwise make every chat span megabytes large.
const maxCapturedContentBytes = 64 << 10

const truncatedMarker = "…[truncated]"

type contentPart struct {
	Type      string          `json:"type"`
	Content   string          `json:"content,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Response  string          `json:"response,omitempty"`
	MimeType  string          `json:"mime_type,omitempty"`
}

type contentMessage struct {
	Role         string        `json:"role"`
	Parts        []contentPart `json:"parts"`
	FinishReason string        `json:"finish_reason,omitempty"`
}

// truncateContent cuts text to the capture limit on a rune boundary.
func truncateContent(text string) string {
	if len(text) <= maxCapturedContentBytes {
		return text
	}
	cut := maxCapturedContentBytes - len(truncatedMarker)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + truncatedMarker
}

func encodeContent(messages []contentMessage) string {
	encoded, err := json.Marshal(messages)
	if err != nil {
		return "[content not encodable: " + err.Error() + "]"
	}
	return truncateContent(string(encoded))
}

// blockParts converts content blocks. Binary data (images) is described by
// its media type only: binary capture is not offered.
func blockParts(blocks []types.ContentBlock) []contentPart {
	parts := make([]contentPart, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case types.BlockText:
			parts = append(parts, contentPart{Type: "text", Content: block.Text})
		case types.BlockThinking:
			if !block.Redacted {
				parts = append(parts, contentPart{Type: "reasoning", Content: block.Thinking})
			}
		case types.BlockToolCall:
			parts = append(parts, contentPart{Type: "tool_call", ID: block.ID, Name: block.Name, Arguments: block.Arguments})
		case types.BlockImage, types.BlockImageRef, types.BlockDocumentRef:
			mimeType := block.MimeType
			if mimeType == "" {
				mimeType = block.RefMediaType
			}
			parts = append(parts, contentPart{Type: "blob", MimeType: mimeType})
		}
	}
	return parts
}

func blocksText(blocks []types.ContentBlock) string {
	var text strings.Builder
	for _, block := range blocks {
		if block.Type != types.BlockText {
			continue
		}
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(block.Text)
	}
	return text.String()
}

func contentOf(message types.Message) (contentMessage, bool) {
	switch typed := message.(type) {
	case types.UserMessage:
		return userContent(typed), true
	case *types.UserMessage:
		return userContent(*typed), true
	case types.AssistantMessage:
		return assistantContent(typed), true
	case *types.AssistantMessage:
		return assistantContent(*typed), true
	case types.ToolResultMessage:
		return toolResultContent(typed), true
	case *types.ToolResultMessage:
		return toolResultContent(*typed), true
	}
	return contentMessage{}, false
}

func userContent(message types.UserMessage) contentMessage {
	if message.Content.Blocks == nil {
		return contentMessage{Role: "user", Parts: []contentPart{{Type: "text", Content: message.Content.Text}}}
	}
	return contentMessage{Role: "user", Parts: blockParts(message.Content.Blocks)}
}

func assistantContent(message types.AssistantMessage) contentMessage {
	return contentMessage{Role: "assistant", Parts: blockParts(message.Content), FinishReason: finishReason(message.StopReason)}
}

func toolResultContent(message types.ToolResultMessage) contentMessage {
	return contentMessage{Role: "tool", Parts: []contentPart{{Type: "tool_call_response", ID: message.ToolCallID, Response: blocksText(message.Content)}}}
}

// inputMessages renders a request's context as gen_ai.input.messages.
func inputMessages(messages []types.Message) func() string {
	return func() string {
		converted := make([]contentMessage, 0, len(messages))
		for _, message := range messages {
			if content, ok := contentOf(message); ok {
				converted = append(converted, content)
			}
		}
		return encodeContent(converted)
	}
}

// outputMessages renders a response as gen_ai.output.messages.
func outputMessages(message types.AssistantMessage) func() string {
	return func() string { return encodeContent([]contentMessage{assistantContent(message)}) }
}

// systemInstructions renders gen_ai.system_instructions.
func systemInstructions(prompt string) func() string {
	return func() string {
		return encodeContent([]contentMessage{{Role: "system", Parts: []contentPart{{Type: "text", Content: prompt}}}})
	}
}

// toolResultText renders gen_ai.tool.call.result from a tool's final result.
func toolResultText(result agent.AgentToolResult) func() string {
	return func() string { return truncateContent(blocksText(result.Content)) }
}

// finishReason maps a stop reason onto the GenAI convention's vocabulary.
func finishReason(reason types.StopReason) string {
	if reason == types.StopToolUse {
		return "tool_call"
	}
	return string(reason)
}
