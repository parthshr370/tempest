package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.harness.dev/harness/internal/question"
)

const questionCancelNoticeTimeout = 10 * time.Millisecond

// supportsURL reports URL-mode elicitation, which exists only from 2025-11-25.
func supportsURL(revision string, raw json.RawMessage) bool {
	var modes map[string]json.RawMessage
	if revision == "2025-06-18" || json.Unmarshal(raw, &modes) != nil {
		return false
	}
	var object map[string]json.RawMessage
	return json.Unmarshal(modes["url"], &object) == nil && object != nil
}

func (p *routingPrompter) Question(ctx context.Context, request question.Request) ([]question.Answer, error) {
	p.mu.Lock()
	active := p.active
	p.mu.Unlock()
	if active == nil || !active.form {
		return nil, question.Unavailable
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	stop := context.AfterFunc(active.ctx, cancel)
	defer stop()
	answers := make([]question.Answer, 0, len(request.Questions))
	for _, prompt := range request.Questions {
		mode := "form"
		form := question.Form(prompt)
		decode := question.DecodeForm
		if active.revision == "2025-06-18" {
			mode = ""
			form = question.LegacyForm(prompt)
			decode = question.DecodeLegacyForm
		}
		raw, err := p.call(ctx, question.Elicitation{Mode: mode, Message: question.Message(prompt), RequestedSchema: form})
		if err != nil {
			return nil, err
		}
		answer, err := decode(prompt, raw)
		if err != nil {
			return nil, err
		}
		answers = append(answers, answer)
	}
	return answers, nil
}

// Elicit passes an elicitation from a session's MCP server through to the
// client of the tempest_run call that is active, when it declared the mode.
func (p *routingPrompter) Elicit(ctx context.Context, request question.Elicit) (question.ElicitResult, error) {
	if err := request.Validate(); err != nil {
		return question.ElicitResult{}, err
	}
	p.mu.Lock()
	active := p.active
	p.mu.Unlock()
	if active == nil || (request.Mode == "form" && !active.form) || (request.Mode == "url" && !active.url) {
		return question.ElicitResult{}, question.Unavailable
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	stop := context.AfterFunc(active.ctx, cancel)
	defer stop()
	params := request.Params(active.revision == "2025-06-18")
	params.SessionID = ""
	raw, err := p.call(ctx, params)
	if err != nil {
		return question.ElicitResult{}, err
	}
	return question.DecodeResult(request, raw)
}

// call sends one elicitation/create and tells the client when the wait ends
// early, so it can retire its dialog.
func (p *routingPrompter) call(ctx context.Context, params any) (json.RawMessage, error) {
	id := fmt.Sprintf("ask_%d", approvalIDs.Add(1))
	raw, err := p.conn.CallWithID(ctx, id, "elicitation/create", params)
	if err != nil && ctx.Err() != nil {
		notifyCtx, stop := context.WithTimeout(context.Background(), questionCancelNoticeTimeout)
		_ = p.conn.Notify(notifyCtx, "notifications/cancelled", struct {
			RequestID string `json:"requestId"`
			Reason    string `json:"reason"`
		}{id, "question ended"})
		stop()
	}
	return raw, err
}
