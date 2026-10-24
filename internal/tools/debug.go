package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/dap"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/schema"
)

func newDebugTool(cwd string, debugger *dap.Manager) agent.AgentTool {
	var mu sync.Mutex
	breakpoints := map[string]map[int]bool{}
	actions := []string{"launch", "attach", "set_breakpoint", "remove_breakpoint", "set_breakpoints", "continue", "step_over", "step_in", "step_out", "pause", "threads", "stack_trace", "scopes", "variables", "evaluate", "output", "terminate"}
	parameters := schema.Object(schema.JSON{
		"action":       schema.JSON{"type": "string", "enum": actions},
		"adapter":      schema.String(),
		"program":      schema.String(),
		"args":         schema.JSON{"type": "array", "items": schema.String()},
		"cwd":          schema.String(),
		"file":         schema.String(),
		"line":         schema.JSON{"type": "integer"},
		"lines":        schema.JSON{"type": "array", "items": schema.JSON{"type": "integer"}},
		"thread_id":    schema.JSON{"type": "integer"},
		"frame_id":     schema.JSON{"type": "integer"},
		"variable_ref": schema.JSON{"type": "integer"},
		"expression":   schema.String(),
		"pid":          schema.JSON{"type": "integer"},
		"arguments":    schema.JSON{"type": "object", "additionalProperties": true},
		"timeout":      schema.JSON{"type": "number", "minimum": 1, "maximum": 300, "default": 30},
	}, "action")
	return agent.AgentTool{
		Tool: types.Tool{
			Name:        string(DebugTool),
			Description: "Control a session-owned Debug Adapter Protocol adapter. Launch/attach with dlv (Go), debugpy (Python), or a configured single-session adapter (js-debug is unsupported). Set source breakpoints, continue/step, inspect stack/scopes/variables, evaluate expressions, read output and terminate. All actions are exec-tier and denied in plan mode. Launched programs are killed on session close; attached programs are detached. After resume no debug session remains; explicitly launch/attach again. arguments supplies adapter-specific launch/attach fields. Output is capped and spills to a readable private file.",
			Parameters:  parameters,
		},
		Label:         string(DebugTool),
		ExecutionMode: agent.ExecSequential,
		Execute: func(ctx context.Context, _ string, raw json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			var input struct {
				Action      string                     `json:"action"`
				Adapter     string                     `json:"adapter"`
				Program     string                     `json:"program"`
				Args        []string                   `json:"args"`
				Cwd         string                     `json:"cwd"`
				File        string                     `json:"file"`
				Line        int                        `json:"line"`
				Lines       []int                      `json:"lines"`
				ThreadID    int                        `json:"thread_id"`
				FrameID     *int                       `json:"frame_id"`
				VariableRef int                        `json:"variable_ref"`
				Expression  string                     `json:"expression"`
				PID         int                        `json:"pid"`
				Arguments   map[string]json.RawMessage `json:"arguments"`
				Timeout     *float64                   `json:"timeout"`
			}
			if err := decodeParams(raw, &input); err != nil {
				return agent.AgentToolResult{}, err
			}
			timeout := dap.Timeout
			if input.Timeout != nil {
				if *input.Timeout < 1 || *input.Timeout > 300 {
					return agent.AgentToolResult{}, errors.New("debug timeout must be between 1 and 300 seconds")
				}
				timeout = time.Duration(*input.Timeout * float64(time.Second))
			}
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			mu.Lock()
			defer mu.Unlock()
			var response json.RawMessage
			var err error
			switch input.Action {
			case "launch", "attach":
				if input.Action == "launch" && input.Program == "" {
					return agent.AgentToolResult{}, errors.New("debug launch requires program")
				}
				launchCwd := cwd
				if input.Cwd != "" {
					launchCwd = resolvePath(cwd, input.Cwd)
				}
				if input.Program != "" {
					input.Program = resolvePath(cwd, input.Program)
				}
				args := input.Arguments
				if args == nil {
					args = map[string]json.RawMessage{}
				}
				encoded, encodeErr := json.Marshal(struct {
					Program   string   `json:"program,omitempty"`
					Cwd       string   `json:"cwd"`
					Args      []string `json:"args,omitempty"`
					ProcessID int      `json:"processId,omitempty"`
				}{input.Program, launchCwd, input.Args, input.PID})
				if encodeErr != nil {
					return agent.AgentToolResult{}, encodeErr
				}
				var common map[string]json.RawMessage
				if err = json.Unmarshal(encoded, &common); err != nil {
					return agent.AgentToolResult{}, err
				}
				for key, value := range common {
					args[key] = value
				}
				response, err = debugger.Start(ctx, input.Action, input.Adapter, input.Program, args)
				if err == nil {
					breakpoints = map[string]map[int]bool{}
					response = json.RawMessage(`{"status":"debug session started"}`)
				}
			case "terminate":
				err = debugger.Terminate(ctx)
				response = json.RawMessage(`{"status":"debug session terminated"}`)
			case "output":
				snapshot, outputErr := debugger.Output()
				if outputErr != nil {
					return agent.AgentToolResult{}, outputErr
				}
				if snapshot.Truncated {
					snapshot.Content += "\n[Adapter output exceeded its 64 MiB disk cap; the spill retains only the captured prefix.]"
				}
				return debugOutput(snapshot.Content, snapshot.FullOutputPath, snapshot.Truncation.Truncated), nil
			case "set_breakpoint", "remove_breakpoint", "set_breakpoints":
				if input.File == "" {
					return agent.AgentToolResult{}, errors.New("debug breakpoint requires file")
				}
				file := resolvePath(cwd, input.File)
				current := map[int]bool{}
				if input.Action != "set_breakpoints" {
					for line := range breakpoints[file] {
						current[line] = true
					}
				}
				if input.Action == "set_breakpoints" {
					for _, line := range input.Lines {
						if line < 1 {
							return agent.AgentToolResult{}, errors.New("breakpoint lines must be positive")
						}
						current[line] = true
					}
				} else {
					if input.Line < 1 {
						return agent.AgentToolResult{}, errors.New("breakpoint requires positive line")
					}
					if input.Action == "remove_breakpoint" {
						delete(current, input.Line)
					} else {
						current[input.Line] = true
					}
				}
				lines := make([]int, 0, len(current))
				for line := range current {
					lines = append(lines, line)
				}
				sort.Ints(lines)
				type breakpoint struct {
					Line int `json:"line"`
				}
				points := make([]breakpoint, 0, len(lines))
				for _, line := range lines {
					points = append(points, breakpoint{line})
				}
				args, encodeErr := json.Marshal(struct {
					Source struct {
						Path string `json:"path"`
					} `json:"source"`
					Breakpoints []breakpoint `json:"breakpoints"`
				}{struct {
					Path string `json:"path"`
				}{file}, points})
				if encodeErr != nil {
					return agent.AgentToolResult{}, encodeErr
				}
				response, err = debugger.Request(ctx, "setBreakpoints", args)
				if err == nil {
					breakpoints[file] = current
				}
			default:
				command := map[string]string{"continue": "continue", "step_over": "next", "step_in": "stepIn", "step_out": "stepOut", "pause": "pause", "threads": "threads", "stack_trace": "stackTrace", "scopes": "scopes", "variables": "variables", "evaluate": "evaluate"}[input.Action]
				if command == "" {
					return agent.AgentToolResult{}, fmt.Errorf("unknown debug action %q", input.Action)
				}
				if input.ThreadID == 0 {
					input.ThreadID = debugger.Thread()
				}
				var args json.RawMessage
				switch input.Action {
				case "threads":
					args = json.RawMessage(`{}`)
				case "scopes":
					if input.FrameID == nil || *input.FrameID < 0 {
						return agent.AgentToolResult{}, errors.New("scopes requires frame_id")
					}
					args, err = json.Marshal(struct {
						FrameID *int `json:"frameId"`
					}{input.FrameID})
				case "variables":
					if input.VariableRef <= 0 {
						return agent.AgentToolResult{}, errors.New("variables requires variable_ref")
					}
					args, err = json.Marshal(struct {
						Reference int `json:"variablesReference"`
					}{input.VariableRef})
				case "evaluate":
					if input.Expression == "" {
						return agent.AgentToolResult{}, errors.New("evaluate requires expression")
					}
					args, err = json.Marshal(struct {
						Expression string `json:"expression"`
						FrameID    *int   `json:"frameId,omitempty"`
						Context    string `json:"context"`
					}{input.Expression, input.FrameID, "repl"})
				default:
					if input.ThreadID <= 0 {
						return agent.AgentToolResult{}, errors.New("debug action requires thread_id; use threads first")
					}
					args, err = json.Marshal(struct {
						ThreadID int `json:"threadId"`
					}{input.ThreadID})
				}
				if err == nil {
					response, err = debugger.Request(ctx, command, args)
				}
			}
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			if len(response) == 0 {
				response = json.RawMessage(`{}`)
			}
			snapshot, err := debugger.BoundResponse(response)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			return debugOutput(snapshot.Content, snapshot.FullOutputPath, snapshot.Truncation.Truncated), nil
		}}
}

func debugOutput(content, path string, truncated bool) agent.AgentToolResult {
	if truncated {
		content += "\n[Output truncated. Full output: " + path + "]"
	}
	return textResult(content, struct {
		Truncated bool   `json:"truncated"`
		Path      string `json:"full_output_path,omitempty"`
	}{truncated, path})
}

func resolvePath(cwd, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(cwd, path)
}
