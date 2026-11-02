package types

import (
	"encoding/json"
	"fmt"
)

// UserContent is the union of a plain string or a list of content blocks. It
// marshals back to whichever shape it holds.
type UserContent struct {
	Text   string         // used when Blocks == nil
	Blocks []ContentBlock // used when non-nil (takes precedence)
}

// StringContent builds text-form user content.
func StringContent(s string) UserContent { return UserContent{Text: s} }

// BlockContent builds block-form user content.
func BlockContent(blocks ...ContentBlock) UserContent { return UserContent{Blocks: blocks} }

// IsBlocks reports whether the content is in block form.
func (u UserContent) IsBlocks() bool { return u.Blocks != nil }

func (u UserContent) MarshalJSON() ([]byte, error) {
	if u.Blocks != nil {
		return json.Marshal(u.Blocks)
	}
	return json.Marshal(u.Text)
}

func (u *UserContent) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		u.Text, u.Blocks = s, nil
		return nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return fmt.Errorf("user content: not a string or block array: %w", err)
	}
	u.Text, u.Blocks = "", blocks
	return nil
}

// Message is the top-level transcript union (user | assistant | toolResult).
// An interface + role switch is idiomatic because the loop filters by role
// (02-map-ai-agent.md A11).
type Message interface {
	Role() string
}

// UserMessage is a user turn. Reminder names the conversation-scope reminder
// whose firing this message is (see agent.Reminder); it is empty for every
// other message and never sent to a provider.
type UserMessage struct {
	Content   UserContent `json:"content"`
	Timestamp int64       `json:"timestamp"`
	Reminder  string      `json:"reminder,omitempty"`
}

func (UserMessage) Role() string { return "user" }

func (m UserMessage) MarshalJSON() ([]byte, error) {
	type alias UserMessage
	return json.Marshal(struct {
		Role string `json:"role"`
		alias
	}{"user", alias(m)})
}

// AssistantMessage is the central accumulator a turn folds into.
type AssistantMessage struct {
	Content       []ContentBlock `json:"content"`
	API           string         `json:"api"`
	Provider      string         `json:"provider"`
	Model         string         `json:"model"`
	ResponseModel string         `json:"responseModel,omitempty"`
	ResponseID    string         `json:"responseId,omitempty"`
	// Diagnostics carries redacted provider/runtime diagnostics for failures and
	// recoveries.
	Diagnostics  []AssistantMessageDiagnostic `json:"diagnostics,omitempty"`
	Usage        Usage                        `json:"usage"`
	StopReason   StopReason                   `json:"stopReason"`
	ErrorMessage string                       `json:"errorMessage,omitempty"`
	ErrorCode    string                       `json:"errorCode,omitempty"`
	ErrorDetails map[string]any               `json:"errorDetails,omitempty"`
	Timestamp    int64                        `json:"timestamp"`
}

// StructuredError is an error carrying a machine-readable code and safe
// structured details for the terminal result frame. A provider converter's
// typed failure implements it so error_code/error_details survive to the
// Claude-compatible output without the provider layer importing the domain package.
type StructuredError interface {
	error
	ErrorCode() string
	ErrorDetails() map[string]any
}

func (AssistantMessage) Role() string { return "assistant" }

func (m AssistantMessage) MarshalJSON() ([]byte, error) {
	type alias AssistantMessage
	return json.Marshal(struct {
		Role string `json:"role"`
		alias
	}{"assistant", alias(m)})
}

// ToolResultMessage is a tool result fed back to the model.
type ToolResultMessage struct {
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Content    []ContentBlock  `json:"content"`
	Details    json.RawMessage `json:"details,omitempty"`
	IsError    bool            `json:"isError"`
	Timestamp  int64           `json:"timestamp"`
}

func (ToolResultMessage) Role() string { return "toolResult" }

func (m ToolResultMessage) MarshalJSON() ([]byte, error) {
	type alias ToolResultMessage
	return json.Marshal(struct {
		Role string `json:"role"`
		alias
	}{"toolResult", alias(m)})
}

// UnmarshalMessage decodes a transcript message, dispatching on its "role".
func UnmarshalMessage(raw []byte) (Message, error) {
	var head struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("message: %w", err)
	}
	switch head.Role {
	case "user":
		var m UserMessage
		return m, json.Unmarshal(raw, &m)
	case "assistant":
		var m AssistantMessage
		return m, json.Unmarshal(raw, &m)
	case "toolResult":
		var m ToolResultMessage
		return m, json.Unmarshal(raw, &m)
	default:
		return nil, fmt.Errorf("message: unknown role %q", head.Role)
	}
}
