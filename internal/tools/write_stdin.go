package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/schema"
	"go.harness.dev/harness/internal/work"
)

// writeStdinDescription teaches expect-style use: start the program as a
// background bash job, then alternate input and the output it caused.
var writeStdinDescription = fmt.Sprintf("Write text to a running background bash job's stdin (by job_id or name) and return the output it printed since the previous write_stdin on that job. "+
	"Only a job started with bash stdin:true or pty:true takes input; any other background job's stdin is /dev/null. "+
	"Start interactive programs (REPLs, prompts, shells, debuggers) with bash {\"command\":...,\"name\":...,\"pty\":true} so they see a terminal; a pty echoes your input and ends lines with \\r\\n. "+
	"text is written as given: end a line with \\n; \\u0003 is Ctrl-C (a pty delivers SIGINT to the foreground program; on a pipe job, text exactly \\u0003 sends SIGINT to the job's process group). "+
	"close_stdin ends input after text: it closes a pipe job's stdin, and sends Ctrl-D (end of file at the start of a line) on a pty. Empty text just polls. "+
	"The call returns once new output has been quiet for 0.3 s, after wait_ms (default %d, max %d) or when the job's output ends; with no new output it waits the full wait_ms. "+
	"It returns at most the newest 16 KiB; omitted_bytes before offset stay readable with logs.", work.DefaultStdinWaitMS, work.MaxStdinWaitMS)

// writeStdinResult is write_stdin's reply: the job's state and the output
// produced since the previous call, with raw log offsets logs accepts.
type writeStdinResult struct {
	JobID        string `json:"job_id"`
	State        string `json:"state"`
	ExitCode     *int   `json:"exit_code,omitempty"`
	Output       string `json:"output"`
	Offset       int64  `json:"offset"`
	NextOffset   int64  `json:"next_offset"`
	OmittedBytes int64  `json:"omitted_bytes,omitempty"`
	StdinClosed  bool   `json:"stdin_closed,omitempty"`
}

func newWriteStdinTool(controller work.Controller) agent.AgentTool {
	parameters := schema.ObjectRequired(schema.JSON{
		"job_id":      schema.String(),
		"name":        schema.JSON{"type": "string", "description": "a background bash job's name, in place of job_id"},
		"text":        schema.JSON{"type": "string", "description": "bytes to write; empty polls for output"},
		"close_stdin": schema.JSON{"type": "boolean", "description": "end the job's input after text"},
		"wait_ms":     schema.JSON{"type": "integer", "minimum": 0, "maximum": work.MaxStdinWaitMS, "description": fmt.Sprintf("longest wait for output, default %d", work.DefaultStdinWaitMS)},
	}, []string{})
	return agent.AgentTool{Tool: types.Tool{Name: string(WriteStdinTool), Description: writeStdinDescription, Parameters: parameters}, Label: string(WriteStdinTool), Execute: func(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		var input struct {
			JobID      string `json:"job_id"`
			Name       string `json:"name"`
			Text       string `json:"text"`
			CloseStdin bool   `json:"close_stdin"`
			WaitMS     *int   `json:"wait_ms"`
		}
		if err := decodeParams(params, &input); err != nil {
			return agent.AgentToolResult{}, err
		}
		wait := work.DefaultStdinWaitMS
		if input.WaitMS != nil {
			wait = *input.WaitMS
		}
		jobID, err := jobTarget(ctx, controller, input.JobID, input.Name)
		if err != nil {
			return agent.AgentToolResult{}, err
		}
		result, err := controller.WriteStdin(ctx, jobID, work.StdinRequest{Text: input.Text, CloseStdin: input.CloseStdin, Wait: time.Duration(wait) * time.Millisecond})
		if err != nil {
			return agent.AgentToolResult{}, err
		}
		encoded, err := json.Marshal(writeStdinResult{JobID: jobID, State: result.Job.State, ExitCode: result.Job.ExitCode, Output: result.Output, Offset: result.Offset, NextOffset: result.NextOffset, OmittedBytes: result.OmittedBytes, StdinClosed: result.StdinClosed})
		if err != nil {
			return agent.AgentToolResult{}, err
		}
		return encodedResult(encoded), nil
	}}
}
