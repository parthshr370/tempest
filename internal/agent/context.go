package agent

import "strings"

func isContextOverflow(message string) bool {
	text := strings.ToLower(message)
	for _, marker := range []string{"context_length_exceeded", "maximum context length", "context window exceeded", "context window is full", "prompt is too long", "input is too long", "too many tokens", "exceeds the context window"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
