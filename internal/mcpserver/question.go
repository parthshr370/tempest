package mcpserver

import (
	"context"
	"fmt"
	"time"

	"go.harness.dev/harness/internal/question"
)

const questionCancelNoticeTimeout = 10 * time.Millisecond

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
		id := fmt.Sprintf("ask_%d", approvalIDs.Add(1))
		raw, err := p.conn.CallWithID(ctx, id, "elicitation/create", question.Elicitation{Mode: mode, Message: question.Message(prompt), RequestedSchema: form})
		if err != nil {
			if ctx.Err() != nil {
				notifyCtx, stop := context.WithTimeout(context.Background(), questionCancelNoticeTimeout)
				_ = p.conn.Notify(notifyCtx, "notifications/cancelled", struct {
					RequestID string `json:"requestId"`
					Reason    string `json:"reason"`
				}{id, "question ended"})
				stop()
			}
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
