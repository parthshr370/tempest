package question

import "go.harness.dev/harness/internal/termtext"

// terminalDisplay is Display for a terminal: the trusted attribution, then
// the server's message with its controls escaped.
func (e Elicit) terminalDisplay() string {
	if e.Server == "" {
		return termtext.Message(e.Message)
	}
	return "MCP server " + termtext.Line(e.Server) + ": " + termtext.Message(e.Message)
}
