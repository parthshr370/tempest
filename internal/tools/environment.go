package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/envlocal"
	"go.harness.dev/harness/internal/hashline"
	"go.harness.dev/harness/internal/refs"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/environment"
)

// NewTool keeps one built-in schema and one permission identity for both hosts.
func NewTool(name ToolName, cwd string, options ToolsOptions) (agent.AgentTool, error) {
	cap := environment.ToolCapability(string(name))
	lease := options.Environment
	if cap == "" || lease == nil {
		return newLocalTool(name, cwd, options)
	}
	if err := environment.Require(lease, cap); err != nil {
		return agent.AgentTool{}, fmt.Errorf("%s: %w", name, err)
	}
	if local, ok := lease.(interface{ LocalRoot() string }); ok {
		cwd = local.LocalRoot()
	}
	if !environment.IsLocal(lease) {
		// Constructors supply schemas only. Their callbacks never run for workspace paths.
		options.Work = nil
		options.AdmitProcess = nil
	}
	tool, err := newLocalTool(name, cwd, options)
	if err != nil {
		return agent.AgentTool{}, err
	}
	tool.Definition.Environment = lease.Reference().Key()
	execute := tool.Execute
	if local, ok := lease.(*envlocal.Local); ok {
		local.Bind(string(name), func(ctx context.Context, c environment.Call) (json.RawMessage, error) {
			result, err := execute(ctx, c.Key, c.Arguments, nil)
			if err != nil {
				return nil, err
			}
			return json.Marshal(result)
		})
		tool.Execute = func(ctx context.Context, id string, args json.RawMessage, update agent.AgentToolUpdateCallback) (result agent.AgentToolResult, err error) {
			call := environment.Call{Environment: lease.Reference(), Key: environmentCallKey(ctx, options.SessionID, id), Operation: string(name), Arguments: args}
			err = local.Run(ctx, call, func(callCtx context.Context) error { result, err = execute(callCtx, id, args, update); return err })
			return result, err
		}
		return tool, nil
	}
	if name == BashTool {
		tool.PrepareArguments = nil
	}
	tool.Execute = func(ctx context.Context, id string, args json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		if name == ViewImageTool && !options.ImageInput {
			return agent.AgentToolResult{}, errors.New("view_image requires a model with image input")
		}
		host, err := hostReferenceCall(name, args)
		if err != nil {
			return agent.AgentToolResult{}, err
		}
		if host {
			return execute(ctx, id, args, nil)
		}
		raw, err := lease.Call(ctx, environment.Call{Environment: lease.Reference(), Key: environmentCallKey(ctx, options.SessionID, id), Operation: string(name), Arguments: args})
		if err != nil {
			return agent.AgentToolResult{}, err
		}
		var result agent.AgentToolResult
		if err = json.Unmarshal(raw, &result); err != nil {
			return result, fmt.Errorf("invalid environment result: %w", err)
		}
		return result, nil
	}
	return tool, nil
}

// hostReferenceCall accepts only explicit host-owned handles, never remote paths.
func hostReferenceCall(name ToolName, args json.RawMessage) (bool, error) {
	if name == BashTool {
		return false, nil
	}
	if name == HashlineTool {
		var input struct {
			Input string `json:"input"`
		}
		if err := json.Unmarshal(args, &input); err != nil {
			return false, err
		}
		patch, err := hashline.Parse(input.Input)
		if err != nil {
			return false, err
		}
		for _, section := range patch.Sections {
			if refs.IsURL(section.Path) {
				return false, errors.New("host artifact URLs cannot be expanded into a remote edit batch")
			}
		}
		return false, nil
	}
	if name != ReadTool && name != WriteTool && name != EditTool && name != GrepTool {
		return false, nil
	}
	var in struct {
		Path  string   `json:"path"`
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return false, err
	}
	paths := in.Paths
	if in.Path != "" {
		paths = append(paths, in.Path)
	}
	host, workspace := false, false
	for _, p := range paths {
		if refs.GitHubURL(p) {
			return false, fmt.Errorf("%w: remote GitHub URL resolution", environment.ErrUnsupported)
		}
		if refs.IsURL(p) {
			host = true
		} else {
			workspace = true
		}
	}
	if host && workspace {
		return false, errors.New("host artifact handles and remote workspace paths require separate calls")
	}
	return host, nil
}

func environmentCallKey(ctx context.Context, sessionID, fallback string) string {
	if invocation, ok := work.InvocationFromContext(ctx); ok {
		fallback = invocation.CallID
	}
	if sessionID != "" {
		return sessionID + ":" + fallback
	}
	return fallback
}

// BindEnvironment installs the ordinary workspace tools on a Local daemon lease.
func BindEnvironment(local *envlocal.Local, options ToolsOptions) error {
	options.Environment = local
	options.ImageInput = true
	options = withHashlineStore(options)
	for _, name := range []ToolName{ReadTool, WriteTool, EditTool, HashlineTool, GrepTool, FindTool, LsTool, BashTool, ViewImageTool} {
		if _, err := NewTool(name, local.LocalRoot(), options); err != nil {
			return err
		}
	}
	return nil
}
