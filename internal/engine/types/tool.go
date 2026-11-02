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
// provider that places explicit cache breakpoints places them before these.
type Context struct {
	SystemPrompt  string    `json:"systemPrompt,omitempty"`
	Messages      []Message `json:"messages"`
	Tools         []Tool    `json:"tools,omitempty"`
	RequestScoped int       `json:"requestScoped,omitempty"`
}
