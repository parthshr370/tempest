package acp

import (
	"context"
	"encoding/json"

	"go.harness.dev/harness/internal/question"
	"go.harness.dev/harness/internal/rpc"
)

func (b *Broker) setForm(connection *rpc.Conn, raw json.RawMessage) {
	if b == nil {
		return
	}
	var modes map[string]json.RawMessage
	var form map[string]json.RawMessage
	supported := json.Unmarshal(raw, &modes) == nil && json.Unmarshal(modes["form"], &form) == nil && form != nil
	b.mu.Lock()
	defer b.mu.Unlock()
	if supported {
		b.forms[connection] = true
	} else {
		delete(b.forms, connection)
	}
}

// Question routes only to a capability-negotiated connection authorized for this session.
// It never transfers a partially answered question to a reconnecting watcher.
func (b *Broker) Question(ctx context.Context, request question.Request) ([]question.Answer, error) {
	b.mu.Lock()
	var connection *rpc.Conn
	for candidate, count := range b.approvers[request.SessionID] {
		if count > 0 && b.forms[candidate] {
			connection = candidate
			break
		}
	}
	b.mu.Unlock()
	if connection == nil {
		return nil, question.Unavailable
	}
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	answers := make([]question.Answer, 0, len(request.Questions))
	for _, prompt := range request.Questions {
		if !b.authorized(request.SessionID, connection) {
			return nil, question.Unavailable
		}
		var raw json.RawMessage
		err := connection.Call(ctx, "elicitation/create", question.Elicitation{SessionID: request.SessionID, ToolCallID: request.ToolCallID, Mode: "form", Message: question.Message(prompt), RequestedSchema: question.Form(prompt)}, &raw)
		if err != nil {
			return nil, err
		}
		if !b.authorized(request.SessionID, connection) {
			return nil, question.Unavailable
		}
		answer, err := question.DecodeForm(prompt, raw)
		if err != nil {
			return nil, err
		}
		answers = append(answers, answer)
	}
	return answers, nil
}
