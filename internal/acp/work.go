package acp

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest"
)

type workParams struct {
	SessionID      string   `json:"sessionId"`
	Text           string   `json:"text"`
	Key            string   `json:"key"`
	QueueID        string   `json:"queueId"`
	JobID          string   `json:"jobId"`
	JobIDs         []string `json:"jobIds"`
	Cursor         string   `json:"cursor"`
	Limit          int      `json:"limit"`
	Offset         int64    `json:"offset"`
	TimeoutSeconds *float64 `json:"timeoutSeconds"`
}

func (c *Client) handleWork(ctx context.Context, method string, raw json.RawMessage) (any, *rpc.Error) {
	var params workParams
	if failure := decode(raw, &params); failure != nil {
		return nil, failure
	}
	if params.SessionID == "" {
		return nil, rpc.NewError(-32602, "invalid_params", "sessionId is required")
	}
	owner, release, err := c.backend.Acquire(params.SessionID)
	if err != nil {
		return nil, failure(err)
	}
	defer release()
	switch method {
	case "tempest/steer", "tempest/follow_up":
		var receipt tempest.QueuedItem
		input := tempest.QueuedInput{Text: params.Text, Key: params.Key}
		if method == "tempest/steer" {
			receipt, err = owner.Steer(ctx, input)
		} else {
			receipt, err = owner.FollowUp(ctx, input)
		}
		if err != nil {
			return nil, failure(err)
		}
		return struct {
			Item tempest.QueuedItem `json:"item"`
		}{receipt}, nil
	case "tempest/cancel_queued":
		if params.QueueID == "" {
			return nil, rpc.NewError(-32602, "invalid_params", "queueId is required")
		}
		if err = owner.CancelQueued(ctx, params.QueueID); err != nil {
			return nil, failure(err)
		}
		return struct{}{}, nil
	case "tempest/jobs":
		page, err := owner.Jobs(ctx, params.Cursor, params.Limit)
		if err != nil {
			return nil, failure(err)
		}
		return page, nil
	case "tempest/wait":
		timeout := 30 * time.Second
		if params.TimeoutSeconds != nil {
			seconds := *params.TimeoutSeconds
			if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > 300 {
				return nil, rpc.NewError(-32602, "invalid_params", "timeoutSeconds must be between 0 and 300")
			}
			timeout = time.Duration(seconds * float64(time.Second))
		}
		jobs, err := owner.WaitJobs(ctx, params.JobIDs, timeout)
		if err != nil {
			return nil, failure(err)
		}
		return struct {
			Jobs []tempest.Job `json:"jobs"`
		}{jobs}, nil
	case "tempest/cancel_job", "tempest/cleanup_job":
		if params.JobID == "" {
			return nil, rpc.NewError(-32602, "invalid_params", "jobId is required")
		}
		var job tempest.Job
		if method == "tempest/cancel_job" {
			job, err = owner.CancelJob(ctx, params.JobID)
		} else {
			job, err = owner.CleanupJob(ctx, params.JobID)
		}
		if err != nil {
			return nil, failure(err)
		}
		return struct {
			Job tempest.Job `json:"job"`
		}{job}, nil
	case "tempest/logs":
		if params.JobID == "" {
			return nil, rpc.NewError(-32602, "invalid_params", "jobId is required")
		}
		page, err := owner.JobLogs(ctx, params.JobID, params.Offset, params.Limit)
		if err != nil {
			return nil, failure(err)
		}
		return page, nil
	}
	return nil, rpc.NewError(-32601, "method_not_found", "unknown method")
}
