package tempest

import (
	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/session"
	"go.harness.dev/harness/internal/tools"
)

func (c *sessionCore) selectedMCPTools() []Tool {
	registry := make(map[tools.ToolName]agent.AgentTool, len(c.baseTools))
	for _, tool := range c.baseTools {
		registry[tools.ToolName(tool.Name)] = tool
	}
	if !c.plan {
		for _, tool := range c.mcp.Tools() {
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
