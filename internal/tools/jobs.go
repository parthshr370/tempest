package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		parameters = schema.ObjectRequired(schema.JSON{"job_ids": schema.JSON{"type": "array", "items": schema.String(), "minItems": 1, "maxItems": 8, "uniqueItems": true}, "names": schema.JSON{"type": "array", "items": schema.String(), "minItems": 1, "maxItems": 8, "uniqueItems": true, "description": "background bash job names, in place of or besides job_ids"}, "timeout": schema.Number()}, []string{})
		description = "Wait for any requested job to settle. Pass job_ids, names (the name given to a background bash job) or both. Timeout stops waiting, not the jobs. Automatic completion follow-ups remain pending."
	case CancelTool:
		parameters = schema.ObjectRequired(schema.JSON{"job_id": schema.String(), "name": schema.JSON{"type": "string", "description": "a background bash job's name, in place of job_id"}}, []string{})
		description = "Durably request cancellation of one session-owned job, by job_id or by background bash job name. Use wait for its joined terminal state."
	case LogsTool:
		parameters = schema.ObjectRequired(schema.JSON{"job_id": schema.String(), "name": schema.JSON{"type": "string", "description": "a background bash job's name, in place of job_id"}, "offset": schema.Number(), "limit": schema.Number()}, []string{})
		description = "Read bounded background bash output using raw byte offsets, by job_id or by background bash job name. Task output is its result artifact, not logs."
	}
	return agent.AgentTool{Tool: types.Tool{Name: string(name), Description: description, Parameters: parameters}, Label: string(name), Execute: func(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		var input struct {
			Cursor  string   `json:"cursor"`
			Limit   int      `json:"limit"`
			Action  string   `json:"action"`
			JobID   string   `json:"job_id"`
			Name    string   `json:"name"`
			JobIDs  []string `json:"job_ids"`
			Names   []string `json:"names"`
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
			ids := append([]string(nil), input.JobIDs...)
			for _, service := range input.Names {
				job, callErr := controller.JobByName(ctx, service)
				if callErr != nil {
					return agent.AgentToolResult{}, callErr
				}
				ids = append(ids, job.ID)
			}
			if len(ids) == 0 {
				return agent.AgentToolResult{}, missingJobTarget(ctx, controller, "wait needs job_ids or names")
			}
			jobs, callErr := controller.Wait(ctx, ids, timeout)
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
			jobID, callErr := jobTarget(ctx, controller, input.JobID, input.Name)
			if callErr != nil {
				return agent.AgentToolResult{}, callErr
			}
			job, callErr := controller.Cancel(ctx, jobID)
			if callErr != nil {
				return agent.AgentToolResult{}, callErr
			}
			encoded, err = json.Marshal(struct {
				Job work.ToolJob `json:"job"`
			}{job.ToolView()})
		case LogsTool:
			jobID, callErr := jobTarget(ctx, controller, input.JobID, input.Name)
			if callErr != nil {
				return agent.AgentToolResult{}, callErr
			}
			page, callErr := controller.Logs(ctx, jobID, input.Offset, input.Limit)
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

// jobTarget resolves exactly one of a job id and a service name to a job id.
func jobTarget(ctx context.Context, controller work.Controller, jobID, name string) (string, error) {
	switch {
	case jobID != "" && name != "":
		return "", errors.New("pass job_id or name, not both")
	case jobID != "":
		return jobID, nil
	case name != "":
		job, err := controller.JobByName(ctx, name)
		if err != nil {
			return "", err
		}
		return job.ID, nil
	}
	return "", missingJobTarget(ctx, controller, "pass job_id or name")
}

// missingJobTarget is the error for a job call that names no job: need says
// what to pass, and the session's jobs (or how to start one) say what to pass
// it.
func missingJobTarget(ctx context.Context, controller work.Controller, need string) error {
	var jobs []work.Job
	cursor := ""
	for {
		page, err := controller.Jobs(ctx, cursor, work.MaxJobPage)
		if err != nil {
			return fmt.Errorf("%s; listing this session's jobs failed: %w", need, err)
		}
		jobs = append(jobs, page.Jobs...)
		if page.NextCursor == "" {
			return fmt.Errorf("%s. %s", need, work.DescribeJobs(jobs))
		}
		cursor = page.NextCursor
	}
}
