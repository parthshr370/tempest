package types

import "encoding/json"

// Tool is a tool definition presented to the model. Parameters is a JSON Schema
// document held as raw bytes (TypeBox has no Go analog; 02-map-ai-agent.md A12).
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Context is the provider request input (02-map-ai-agent.md A13).
// RequestScoped counts the trailing Messages sent with this request only
// (request-scope reminders): the next request no longer carries them, so a
// provider that places cache breakpoints places them before these (see
// SplitRequestScoped).
type Context struct {
	SystemPrompt  string    `json:"systemPrompt,omitempty"`
	Messages      []Message `json:"messages"`
	Tools         []Tool    `json:"tools,omitempty"`
	RequestScoped int       `json:"requestScoped,omitempty"`
}

// SplitRequestScoped splits Messages into the prefix later requests resend
// and the RequestScoped tail sent with this request only. A cache breakpoint
// belongs in the prefix: one on the tail caches bytes the next request does
// not send, so that request cannot read it.
func (c Context) SplitRequestScoped() (prefix, tail []Message) {
	split := len(c.Messages) - min(max(c.RequestScoped, 0), len(c.Messages))
	return c.Messages[:split], c.Messages[split:]
}
