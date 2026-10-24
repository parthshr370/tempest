package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/evalkernel"
	"go.harness.dev/harness/internal/schema"
)

type evalToolDetails struct {
	Language       string `json:"language"`
	OutputBytes    int    `json:"output_bytes"`
	Truncated      bool   `json:"truncated"`
	FullOutputPath string `json:"full_output_path,omitempty"`
	TimedOut       bool   `json:"timed_out,omitempty"`
}

func newEvalTool(kernels *evalkernel.Manager) agent.AgentTool {
	return agent.AgentTool{
		Tool: types.Tool{
			Name:        string(EvalTool),
			Description: "Execute a cell in a persistent session-owned Python (py) or JavaScript (js) kernel. Variables survive calls in that language. Python shows repr of the trailing expression; JS uses a synchronous VM under Node or Bun (require is available; no top-level await; timers and Promise callbacks run between cells). stdout/stderr and tracebacks are captured. timeout defaults to 30 seconds; 0 disables the execution deadline, not transport-failure detection. A timeout interrupts only the kernel, retaining state when the runtime cooperates; Bun without VM interruption requires a hard kill and loses state. reset:true replaces only this language's process before running code. After session resume or rewind, previously used languages require reset:true and setup again. Destructive code requires per-call approval; cloud deny rules still apply in bypass. Cells may have arbitrary side effects and are never replayed automatically. For figures, save a PNG inside the workspace and call view_image; eval does not implicitly render figures or image bytes.",
			Parameters: schema.Object(schema.JSON{
				"language": schema.JSON{"type": "string", "enum": []string{"py", "js"}},
				"code":     schema.String(),
				"timeout":  schema.JSON{"type": "number", "minimum": 0, "maximum": maxTimeoutSeconds, "default": 30},
				"reset":    schema.Bool(),
			}, "language", "code"),
		},
		Label: string(EvalTool),
		Execute: func(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			input := struct {
				Language string   `json:"language"`
				Code     string   `json:"code"`
				Timeout  *float64 `json:"timeout"`
				Reset    bool     `json:"reset"`
			}{}
			if err := decodeParams(params, &input); err != nil {
				return agent.AgentToolResult{}, err
			}
			timeout := 30.0
			if input.Timeout != nil {
				timeout = *input.Timeout
			}
			if math.IsNaN(timeout) || math.IsInf(timeout, 0) || timeout < 0 || timeout > maxTimeoutSeconds {
				return agent.AgentToolResult{}, errors.New("eval timeout must be between 0 and 2147483 seconds")
			}
			result, err := kernels.Eval(ctx, evalkernel.Request{Language: input.Language, Code: input.Code, Timeout: time.Duration(timeout * float64(time.Second)), Reset: input.Reset})
			text := result.Output.Content
			if text == "" {
				text = "(no output)"
			}
			if result.Output.Truncation.Truncated {
				text += "\n\n[Output truncated; showing tail. Full output: " + result.Output.FullOutputPath + "]"
			}
			if result.TimedOut {
				text += fmt.Sprintf("\n\nCell timed out after %g seconds; execution was interrupted. Effects and variables set before interruption remain unless the kernel state was lost.", timeout)
			}
			if err != nil {
				text += "\n\n" + err.Error()
			}
			response := textResult(text, evalToolDetails{
				Language:       input.Language,
				OutputBytes:    result.Output.Truncation.TotalBytes,
				Truncated:      result.Output.Truncation.Truncated,
				FullOutputPath: result.Output.FullOutputPath,
				TimedOut:       result.TimedOut,
			})
			response.IsError = result.Failed || err != nil
			return response, nil
		},
	}
}
