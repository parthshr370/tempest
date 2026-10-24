package durable

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
)

// Submission is a lifetime admission receipt. Payload preserves the original
// input; rewrites never change RequestHash. Only explicit resume schedules it.
type Submission struct {
	ID              string          `json:"id"`
	RequestID       string          `json:"requestId"`
	ClientNamespace string          `json:"clientNamespace,omitempty"`
	State           string          `json:"state"`
	RunID           string          `json:"runId,omitempty"`
	LogicalRunID    string          `json:"logicalRunId,omitempty"`
	AnswerEntryID   string          `json:"answerEntryId,omitempty"`
	Reason          string          `json:"reason,omitempty"`
	Revision        uint64          `json:"revision"`
	RequestHash     string          `json:"requestHash"`
	Placement       string          `json:"placement"`
	Payload         json.RawMessage `json:"payload"`
	QueueID         string          `json:"queueId,omitempty"`
	Answer          string          `json:"answer,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	Error           string          `json:"error,omitempty"`
}

// SubmissionHash identifies original prompt content and placement, not transport IDs.
func SubmissionHash(placement string, prompt json.RawMessage) (string, error) {
	return Digest(struct {
		Placement string          `json:"placement"`
		Prompt    json.RawMessage `json:"prompt"`
	}{placement, prompt})
}

type clientNamespaceKey struct{}

// WithClientNamespace binds host-authenticated identity, never a wire payload.
func WithClientNamespace(ctx context.Context, namespace string) context.Context {
	return context.WithValue(ctx, clientNamespaceKey{}, namespace)
}

// ClientNamespace returns the host-established namespace, or the trusted default.
func ClientNamespace(ctx context.Context) string {
	value, _ := ctx.Value(clientNamespaceKey{}).(string)
	return value
}

// SubmissionKey separates client namespaces without changing the caller's ID.
func SubmissionKey(namespace, id string) string {
	if namespace == "" && !strings.HasPrefix(id, "client:") {
		return id
	}
	return "client:" + base64.RawURLEncoding.EncodeToString([]byte(namespace)) + ":" + base64.RawURLEncoding.EncodeToString([]byte(id))
}
