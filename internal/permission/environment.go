package permission

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"strings"

	"go.harness.dev/harness/internal/cloudcmd"
	"go.harness.dev/harness/tempest/environment"
)

func isHostOnlyTool(tool string) bool {
	switch tool {
	case "todo_write", "task", "message":
		return true
	default:
		return strings.HasPrefix(tool, "mcp__") || strings.HasPrefix(tool, "ext__")
	}
}

// WorkspaceInspection contains filesystem-dependent facts, not permission grants.
// The host still applies live policy, plan mode and the child's permission ceiling.
type WorkspaceInspection struct {
	Bash               bool              `json:"bash"`
	Command            string            `json:"command"`
	GuardReason        string            `json:"guardReason"`
	Analysis           cloudcmd.Analysis `json:"analysis"`
	HookSettingsReason string            `json:"hookSettingsReason"`
}

// InspectionRequest describes the effective arguments after preparation and hooks.
type InspectionRequest struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

// InspectWorkspace runs only on the host that owns the workspace files.
func InspectWorkspace(tool string, args json.RawMessage, cwd string) WorkspaceInspection {
	facts := inspectShell(tool, args, cwd)
	return WorkspaceInspection{Bash: facts.bash, Command: facts.command, GuardReason: facts.guardReason, Analysis: facts.analysis, HookSettingsReason: hookSettingsReason(tool, args, cwd)}
}

type workspaceInspectionKey struct{}

// InspectEnvironment asks the selected workspace host for facts before the gate.
func InspectEnvironment(ctx context.Context, lease environment.Lease, tool string, args json.RawMessage) (context.Context, error) {
	if environment.IsLocal(lease) || ClassifyTool(tool) == TierRead {
		return ctx, nil
	}
	if isHostOnlyTool(tool) {
		return context.WithValue(ctx, workspaceInspectionKey{}, WorkspaceInspection{}), nil
	}
	if err := environment.Require(lease, environment.Permissions); err != nil {
		return ctx, err
	}
	arguments, err := json.Marshal(InspectionRequest{Tool: tool, Arguments: args})
	if err != nil {
		return ctx, err
	}
	result, err := lease.Call(ctx, environment.Call{Environment: lease.Reference(), Key: "permission:" + rand.Text(), Operation: "inspect", Arguments: arguments})
	if err != nil {
		return ctx, err
	}
	var inspection WorkspaceInspection
	if err := json.Unmarshal(result, &inspection); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, workspaceInspectionKey{}, inspection), nil
}

func inspectWorkspaceForGate(ctx context.Context, tool string, args json.RawMessage, cwd string) (shellFacts, string) {
	if inspection, ok := ctx.Value(workspaceInspectionKey{}).(WorkspaceInspection); ok {
		if tool == "bash" {
			facts := inspectShell(tool, args, cwd)
			if facts.guardReason == "" {
				facts.guardReason = inspection.GuardReason
			}
			return facts, inspection.HookSettingsReason
		}
		return shellFacts{bash: inspection.Bash, command: inspection.Command, guardReason: inspection.GuardReason, analysis: inspection.Analysis}, inspection.HookSettingsReason
	}
	facts := inspectShell(tool, args, cwd)
	return facts, hookSettingsReason(tool, args, cwd)
}
