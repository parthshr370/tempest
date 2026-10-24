package durable

// MemoScope binds a memo to one logical invocation and handler. Attempts share
// that invocation ID; unrelated calls must use a fresh one.
type MemoScope struct {
	SessionID    string `json:"sessionId"`
	InvocationID string `json:"invocationId"`
	ExtensionID  string `json:"extensionId"`
	HookID       string `json:"hookId"`
}
