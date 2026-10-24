package acp

import (
	"context"
	"encoding/json"

	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest"
	"go.harness.dev/harness/tempest/durable"
)

func (c *Client) handleDurable(ctx context.Context, method string, raw json.RawMessage) (any, *rpc.Error) {
	var p struct {
		SessionID    string            `json:"sessionId"`
		RequestID    string            `json:"requestId"`
		Prompt       []contentBlock    `json:"prompt"`
		OutputSchema json.RawMessage   `json:"outputSchema"`
		Placement    tempest.Placement `json:"placement"`
		Commit       durable.Commit    `json:"commit"`
		Query        durable.Query     `json:"query"`
	}
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	if p.SessionID == "" {
		return nil, rpc.NewError(-32602, "invalid_params", "sessionId is required")
	}
	if (method == "_tempest/durable/submit" || method == "_tempest/durable/get_submission" || method == "_tempest/durable/withdraw") && p.RequestID == "" {
		return nil, rpc.NewError(-32602, "invalid_params", "requestId is required")
	}
	s, release, err := c.backend.Acquire(p.SessionID)
	if err != nil {
		return nil, failure(err)
	}
	defer release()
	var value any
	switch method {
	case "_tempest/durable/submit":
		prompt, e := convertPrompt(p.Prompt)
		if e != nil {
			return nil, e
		}
		prompt.OutputSchema = p.OutputSchema
		value, err = s.Submit(ctx, tempest.Input{RequestID: p.RequestID, Prompt: prompt, Placement: p.Placement})
		if err == nil {
			submission := value.(tempest.Submission)
			if submission.State == "accepted" && (p.Placement == "" || p.Placement == tempest.PlacementInput) {
				err = s.ResumePending(ctx)
				if err == nil {
					value, err = s.Submission(ctx, p.RequestID)
				}
			}
		}
	case "_tempest/durable/get_submission":
		value, err = s.Submission(ctx, p.RequestID)
	case "_tempest/durable/withdraw":
		err = s.Withdraw(ctx, p.RequestID)
		value = struct{}{}
	case "_tempest/durable/commit":
		value, err = s.Commit(ctx, p.Commit)
	case "_tempest/durable/read":
		value, err = s.Read(ctx, p.Query)
	}
	if err != nil {
		return nil, failure(err)
	}
	return value, nil
}

func promptCompletion(result tempest.RunResult, err error) (any, *rpc.Error) {
	if result.StopReason == "error" || result.StopReason == "output_invalid" || result.StopReason == "repeated_failure" {
		code, name, message := -32009, "run_error", "run failed"
		switch result.StopReason {
		case "output_invalid":
			code, name = -32022, "output_invalid"
		case "repeated_failure":
			code, name = -32023, "repeated_failure"
		}
		if result.Err != nil {
			message = result.Err.Error()
		}
		return nil, rpc.NewError(code, name, message)
	}
	if err != nil {
		return nil, failure(err)
	}
	reason := "end_turn"
	switch result.StopReason {
	case "aborted":
		reason = "cancelled"
	case "max_turns", "max_cost", "max_wall_time":
		reason = "max_turn_requests"
	}
	var cost any
	if !result.Usage.Cost.Unknown {
		cost = result.Usage.Cost.Total
	}
	meta := map[string]any{"stopReason": result.StopReason, "runId": result.RunID, "turns": result.Turns, "usage": result.Usage, "costUsd": cost, "elapsedMs": result.ElapsedMS}
	if result.Output != nil {
		meta["output"] = result.Output
	}
	return map[string]any{"stopReason": reason, "_meta": map[string]any{"tempest": meta}}, nil
}
