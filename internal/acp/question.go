package acp

import (
	"context"
	"encoding/json"

	"go.harness.dev/harness/internal/question"
	"go.harness.dev/harness/internal/rpc"
)

// setElicitation records which elicitation modes a client declared.
func (b *Broker) setElicitation(connection *rpc.Conn, raw json.RawMessage) {
	if b == nil {
		return
	}
	var modes map[string]json.RawMessage
	_ = json.Unmarshal(raw, &modes)
	declared := func(mode string) bool {
		var settings map[string]json.RawMessage
		return json.Unmarshal(modes[mode], &settings) == nil && settings != nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for mode, connections := range map[string]map[*rpc.Conn]bool{"form": b.forms, "url": b.urls} {
		if declared(mode) {
			connections[connection] = true
		} else {
			delete(connections, connection)
		}
	}
}

// connection picks a client authorized for the session that declared mode.
func (b *Broker) connection(sessionID, mode string) *rpc.Conn {
	supported := b.forms
	if mode == "url" {
		supported = b.urls
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for candidate, count := range b.approvers[sessionID] {
		if count > 0 && supported[candidate] {
			return candidate
		}
	}
	return nil
}

// Question routes only to a capability-negotiated connection authorized for this session.
// It never transfers a partially answered question to a reconnecting watcher.
func (b *Broker) Question(ctx context.Context, request question.Request) ([]question.Answer, error) {
	connection := b.connection(request.SessionID, "form")
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

// Elicit forwards an MCP server's elicitation, or an authorization URL, to a
// client of the session that declared the request's mode.
func (b *Broker) Elicit(ctx context.Context, request question.Elicit) (question.ElicitResult, error) {
	if err := request.Validate(); err != nil {
		return question.ElicitResult{}, err
	}
	connection := b.connection(request.SessionID, request.Mode)
	if connection == nil {
		return question.ElicitResult{}, question.Unavailable
	}
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	var raw json.RawMessage
	if err := connection.Call(ctx, "elicitation/create", request.Params(false), &raw); err != nil {
		return question.ElicitResult{}, err
	}
	if !b.authorized(request.SessionID, connection) {
		return question.ElicitResult{}, question.Unavailable
	}
	return question.DecodeResult(request, raw)
}
