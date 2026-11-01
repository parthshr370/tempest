package telemetry

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
)

// Message content is captured only when the operator sets
// OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true, in the
// OpenTelemetry GenAI message format (role plus typed parts). The listener
// copies a bounded prefix of the content when the event arrives, so a span
// waiting for a slow collector holds at most that copy, never the history it
// came from. JSON encoding of the copy happens at export time on the exporter
// goroutine, and nothing cuts the encoded text, so it is always valid JSON.

// maxCapturedContentBytes caps the text one captured attribute keeps; a long
// transcript would otherwise make every chat span megabytes large.
const maxCapturedContentBytes = 64 << 10

// capturedStructureBytes is what each captured message and part is charged
// for its JSON structure (braces, role, type and member names), so a history
// of many tiny messages is bounded too.
const capturedStructureBytes = 32

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

// contentBudget is what one captured attribute may still keep. Text it
// returns is a private copy, so the capture never pins a larger string.
type contentBudget struct {
	remaining int
	// exhausted is set once something did not fit; marked once the
	// truncation marker has been written into a captured text.
	exhausted bool
	marked    bool
}

func newContentBudget() *contentBudget {
	return &contentBudget{remaining: maxCapturedContentBytes}
}

// text returns as much of value as fits, cut on a rune boundary and ending
// in truncatedMarker when cut, or "" once the budget is exhausted.
func (b *contentBudget) text(value string) string {
	if b.exhausted {
		return ""
	}
	if len(value) <= b.remaining {
		b.remaining -= len(value)
		return strings.Clone(value)
	}
	cut := max(b.remaining-len(truncatedMarker), 0)
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	b.remaining, b.exhausted, b.marked = 0, true, true
	return strings.Clone(value[:cut]) + truncatedMarker
}

// structure charges one message's or part's JSON structure and reports
// whether it fits.
func (b *contentBudget) structure() bool {
	if b.exhausted || b.remaining < capturedStructureBytes {
		b.remaining, b.exhausted = 0, true
		return false
	}
	b.remaining -= capturedStructureBytes
	return true
}

// arguments keeps tool call arguments whole when they fit; cut arguments
// become a JSON string, since a prefix of JSON is not JSON.
func (b *contentBudget) arguments(raw json.RawMessage) json.RawMessage {
	if b.exhausted || len(raw) == 0 {
		return nil
	}
	if len(raw) <= b.remaining {
		b.remaining -= len(raw)
		return append(json.RawMessage(nil), raw...)
	}
	// One byte past the budget makes text cut the arguments and mark them.
	encoded, err := json.Marshal(b.text(string(raw[:b.remaining+1])))
	if err != nil {
		// Encoding a string cannot fail; leaving the arguments out keeps the
		// capture valid JSON if it ever did.
		return nil
	}
	return encoded
}

// capture converts messages in order until the budget runs out. When it ran
// out without cutting a text, a final marker part says content was left out.
func (b *contentBudget) capture(messages []types.Message) []contentMessage {
	captured := make([]contentMessage, 0, min(len(messages), maxCapturedContentBytes/capturedStructureBytes))
	for _, message := range messages {
		if !b.structure() {
			break
		}
		content, ok := b.message(message)
		if ok {
			captured = append(captured, content)
		}
		if b.exhausted {
			break
		}
	}
	if b.exhausted && !b.marked && len(captured) > 0 {
		last := &captured[len(captured)-1]
		last.Parts = append(last.Parts, contentPart{Type: "text", Content: truncatedMarker})
		b.marked = true
	}
	return captured
}

func (b *contentBudget) message(message types.Message) (contentMessage, bool) {
	switch typed := message.(type) {
	case types.UserMessage:
		return b.user(typed), true
	case *types.UserMessage:
		return b.user(*typed), true
	case types.AssistantMessage:
		return b.assistant(typed), true
	case *types.AssistantMessage:
		return b.assistant(*typed), true
	case types.ToolResultMessage:
		return b.toolResult(typed), true
	case *types.ToolResultMessage:
		return b.toolResult(*typed), true
	}
	return contentMessage{}, false
}

func (b *contentBudget) user(message types.UserMessage) contentMessage {
	if message.Content.Blocks == nil {
		parts := []contentPart{}
		if b.structure() {
			parts = append(parts, contentPart{Type: "text", Content: b.text(message.Content.Text)})
		}
		return contentMessage{Role: "user", Parts: parts}
	}
	return contentMessage{Role: "user", Parts: b.blocks(message.Content.Blocks)}
}

func (b *contentBudget) assistant(message types.AssistantMessage) contentMessage {
	return contentMessage{Role: "assistant", Parts: b.blocks(message.Content), FinishReason: finishReason(message.StopReason)}
}

func (b *contentBudget) toolResult(message types.ToolResultMessage) contentMessage {
	parts := []contentPart{}
	if b.structure() {
		parts = append(parts, contentPart{Type: "tool_call_response", ID: b.text(message.ToolCallID), Response: b.blocksText(message.Content)})
	}
	return contentMessage{Role: "tool", Parts: parts}
}

// blocks converts content blocks. Binary data (images) is described by its
// media type only: binary capture is not offered.
func (b *contentBudget) blocks(blocks []types.ContentBlock) []contentPart {
	parts := []contentPart{}
	for _, block := range blocks {
		if b.exhausted {
			break
		}
		if block.Type == types.BlockThinking && block.Redacted {
			continue
		}
		if !b.structure() {
			break
		}
		switch block.Type {
		case types.BlockText:
			parts = append(parts, contentPart{Type: "text", Content: b.text(block.Text)})
		case types.BlockThinking:
			parts = append(parts, contentPart{Type: "reasoning", Content: b.text(block.Thinking)})
		case types.BlockToolCall:
			parts = append(parts, contentPart{Type: "tool_call", ID: b.text(block.ID), Name: b.text(block.Name), Arguments: b.arguments(block.Arguments)})
		case types.BlockImage, types.BlockImageRef, types.BlockDocumentRef:
			mimeType := block.MimeType
			if mimeType == "" {
				mimeType = block.RefMediaType
			}
			parts = append(parts, contentPart{Type: "blob", MimeType: b.text(mimeType)})
		}
	}
	return parts
}

// blocksText joins the text blocks with newlines within the budget.
func (b *contentBudget) blocksText(blocks []types.ContentBlock) string {
	var text strings.Builder
	for _, block := range blocks {
		if block.Type != types.BlockText || b.exhausted {
			continue
		}
		if text.Len() > 0 {
			text.WriteString(b.text("\n"))
		}
		text.WriteString(b.text(block.Text))
	}
	return text.String()
}

func encodeContent(messages []contentMessage) string {
	encoded, err := json.Marshal(messages)
	if err != nil {
		return "[content not encodable: " + err.Error() + "]"
	}
	return string(encoded)
}

// inputMessages captures a request's context for gen_ai.input.messages.
func inputMessages(messages []types.Message) func() string {
	captured := newContentBudget().capture(messages)
	return func() string { return encodeContent(captured) }
}

// outputMessages captures a response for gen_ai.output.messages.
func outputMessages(message types.AssistantMessage) func() string {
	captured := newContentBudget().capture([]types.Message{message})
	return func() string { return encodeContent(captured) }
}

// systemInstructions captures gen_ai.system_instructions, which the GenAI
// schema defines as a list of parts, not messages.
func systemInstructions(prompt string) func() string {
	text := newContentBudget().text(prompt)
	return func() string {
		encoded, err := json.Marshal([]contentPart{{Type: "text", Content: text}})
		if err != nil {
			return "[content not encodable: " + err.Error() + "]"
		}
		return string(encoded)
	}
}

// toolArguments captures gen_ai.tool.call.arguments: the arguments' JSON
// text, cut to the capture limit.
func toolArguments(raw json.RawMessage) string {
	return newContentBudget().text(string(raw[:min(len(raw), maxCapturedContentBytes+1)]))
}

// toolResultText captures gen_ai.tool.call.result from a tool's final result.
func toolResultText(result agent.AgentToolResult) string {
	return newContentBudget().blocksText(result.Content)
}

// finishReason maps a stop reason onto the GenAI convention's vocabulary.
func finishReason(reason types.StopReason) string {
	if reason == types.StopToolUse {
		return "tool_call"
	}
	return string(reason)
}
