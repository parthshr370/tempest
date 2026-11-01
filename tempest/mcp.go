package tempest

import (
	"context"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/question"
	"go.harness.dev/harness/internal/session"
	"go.harness.dev/harness/internal/tools"
)

// elicitor scopes the host's elicitor to this session, so ACP routes the
// request to this session's clients. Nil stays nil: no user can be asked.
func (c *sessionCore) elicitor(host Elicitor) question.Elicitor {
	if host == nil {
		return nil
	}
	return func(ctx context.Context, request question.Elicit) (question.ElicitResult, error) {
		request.SessionID = c.id
		return host(ctx, request)
	}
}

func (c *sessionCore) selectedMCPTools() []Tool {
	registry := make(map[tools.ToolName]agent.AgentTool, len(c.baseTools))
	for _, tool := range c.baseTools {
		registry[tools.ToolName(tool.Name)] = tool
	}
	if !c.plan {
		mcpTools := c.mcp.Tools()
		if c.checkpoints != nil {
			mcpTools = c.checkpoints.Wrap(mcpTools, func(string) bool { return true })
		}
		for _, tool := range mcpTools {
			if _, exists := registry[tools.ToolName(tool.Name)]; !exists {
				registry[tools.ToolName(tool.Name)] = tool
			}
		}
	}
	return session.SelectActive(registry, nil, c.excludeTools, "")
}

func (c *sessionCore) installMCPDefaults() {
	defaults := make(map[string]permission.Decision, len(c.baseMCPDefaults))
	for name, decision := range c.baseMCPDefaults {
		defaults[name] = decision
	}
	for name, decision := range c.mcp.Defaults() {
		defaults[name] = decision
	}
	c.permissionPolicy.ReplaceMCPDefaults(defaults)
}
