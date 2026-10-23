package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/schema"
	"go.harness.dev/harness/internal/toolio"
	"go.harness.dev/harness/internal/work"
)

type bashOutputWriter struct {
	mu          sync.Mutex
	accumulator *toolio.OutputAccumulator
	cancel      context.CancelFunc
	err         error
}

func (writer *bashOutputWriter) Write(buffer []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.err != nil {
		return 0, writer.err
	}
	if err := writer.accumulator.Append(buffer); err != nil {
		writer.err = err
		writer.cancel()
		return 0, err
	}
	return len(buffer), nil
}
func jobHandleResult(job work.Job) (agent.AgentToolResult, error) {
	encoded, err := json.Marshal(job.Handle())
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	return encodedResult(encoded), nil
}
func encodedResult(encoded json.RawMessage) agent.AgentToolResult {
	return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText(string(encoded))}, Details: encoded}
}

// JobsAction validates the action before a permission gate classifies it.
func JobsAction(params json.RawMessage) (string, error) {
	var input struct {
		Action string `json:"action"`
		JobID  string `json:"job_id"`
		Cursor string `json:"cursor"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(params, &input); err != nil {
		return "", err
	}
	if input.Action == "" {
		input.Action = "list"
	}
	if input.Action != "list" && input.Action != "cleanup" {
		return "", errors.New("jobs action must be list or cleanup")
	}
	if input.Action == "cleanup" && input.JobID == "" {
		return "", errors.New("cleanup requires job_id")
	}
	return input.Action, nil
}
func newManagementTool(name ToolName, controller work.Controller) agent.AgentTool {
	var parameters json.RawMessage
	var description string
	switch name {
	case JobsTool:
		parameters = schema.ObjectRequired(schema.JSON{"cursor": schema.String(), "limit": schema.Number(), "action": schema.JSON{"type": "string", "enum": []string{"list", "cleanup"}}, "job_id": schema.String()}, []string{})
		description = "List durable session jobs, or explicitly clean up one terminal job's owned worktree. Cleanup never applies its patch."
	case WaitTool:
		parameters = schema.Object(schema.JSON{"job_ids": schema.JSON{"type": "array", "items": schema.String(), "minItems": 1, "maxItems": 8, "uniqueItems": true}, "timeout": schema.Number()}, "job_ids")
		description = "Wait for any requested job to settle. Timeout stops waiting, not the jobs. Automatic completion follow-ups remain pending."
	case CancelTool:
		parameters = schema.Object(schema.JSON{"job_id": schema.String()}, "job_id")
		description = "Durably request cancellation of one session-owned job. Use wait for its joined terminal state."
	case LogsTool:
		parameters = schema.Object(schema.JSON{"job_id": schema.String(), "offset": schema.Number(), "limit": schema.Number()}, "job_id")
		description = "Read bounded background bash output using raw byte offsets. Task output is its result artifact, not logs."
	}
	return agent.AgentTool{Tool: types.Tool{Name: string(name), Description: description, Parameters: parameters}, Label: string(name), Execute: func(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		var input struct {
			Cursor  string   `json:"cursor"`
			Limit   int      `json:"limit"`
			Action  string   `json:"action"`
			JobID   string   `json:"job_id"`
			JobIDs  []string `json:"job_ids"`
			Timeout *float64 `json:"timeout"`
			Offset  int64    `json:"offset"`
		}
		if err := json.Unmarshal(params, &input); err != nil {
			return agent.AgentToolResult{}, err
		}
		var encoded json.RawMessage
		var err error
		switch name {
		case JobsTool:
			action, parseErr := JobsAction(params)
			if parseErr != nil {
				return agent.AgentToolResult{}, parseErr
			}
			if action == "cleanup" {
				job, callErr := controller.Cleanup(ctx, input.JobID)
				if callErr != nil {
					return agent.AgentToolResult{}, callErr
				}
				encoded, err = json.Marshal(struct {
					Job work.ToolJob `json:"job"`
				}{job.ToolView()})
			} else {
				page, callErr := controller.Jobs(ctx, input.Cursor, input.Limit)
				if callErr != nil {
					return agent.AgentToolResult{}, callErr
				}
				encoded, err = json.Marshal(page.ToolView())
			}
		case WaitTool:
			timeout := 30 * time.Second
			if input.Timeout != nil {
				if *input.Timeout < 0 || *input.Timeout > 300 {
					return agent.AgentToolResult{}, errors.New("wait timeout must be between 0 and 300 seconds")
				}
				timeout = time.Duration(*input.Timeout * float64(time.Second))
			}
			jobs, callErr := controller.Wait(ctx, input.JobIDs, timeout)
			if callErr != nil {
				return agent.AgentToolResult{}, callErr
			}
			timedOut := true
			for _, job := range jobs {
				switch job.State {
				case "completed", "failed", "cancelled", "unknown":
					timedOut = false
				}
			}
			encoded, err = json.Marshal(struct {
				Jobs     []work.ToolJob `json:"jobs"`
				TimedOut bool           `json:"timed_out"`
			}{work.ToolViews(jobs), timedOut})
		case CancelTool:
			if input.JobID == "" {
				return agent.AgentToolResult{}, errors.New("job_id is required")
			}
			job, callErr := controller.Cancel(ctx, input.JobID)
			if callErr != nil {
				return agent.AgentToolResult{}, callErr
			}
			encoded, err = json.Marshal(struct {
				Job work.ToolJob `json:"job"`
			}{job.ToolView()})
		case LogsTool:
			page, callErr := controller.Logs(ctx, input.JobID, input.Offset, input.Limit)
			if callErr != nil {
				return agent.AgentToolResult{}, callErr
			}
			encoded, err = json.Marshal(page.ToolView())
		}
		if err != nil {
			return agent.AgentToolResult{}, err
		}
		return encodedResult(encoded), nil
	}}
}
