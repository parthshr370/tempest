package question

// Elicitation is the ACP/MCP form request. ACP requires explicit mode and session scope.
type Elicitation struct {
	SessionID       string     `json:"sessionId,omitempty"`
	ToolCallID      string     `json:"toolCallId,omitempty"`
	Mode            string     `json:"mode,omitempty"`
	Message         string     `json:"message"`
	RequestedSchema FormSchema `json:"requestedSchema"`
}

// Message preserves option descriptions even in clients with plain enum widgets.
func Message(q Question) string {
	message := q.Question
	for _, option := range q.Options {
		if option.Description != "" {
			message += "\n" + option.Label + ": " + option.Description
		}
	}
	return message
}
