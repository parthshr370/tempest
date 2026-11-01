package tempest

import (
	"context"
	"strings"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/mcpclient"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/question"
	"go.harness.dev/harness/internal/session"
	"go.harness.dev/harness/internal/tools"
)

// Command is a slash command the session understands: an MCP server
// prompt, invoked as /<server>:<prompt> followed by its arguments.
type Command struct {
	// Name is the command without its leading slash.
	Name        string `json:"name"`
	Description string `json:"description"`
	// Hint names the arguments: <required> and [optional].
	Hint string `json:"hint,omitempty"`
}

// Commands lists the slash commands of the session's MCP prompts.
func (s *Session) Commands() []Command {
	if !s.core.mcpEnabled {
		return nil
	}
	return commands(s.core.mcp.Prompts())
}

func commands(prompts []mcpclient.Prompt) []Command {
	out := make([]Command, 0, len(prompts))
	for _, prompt := range prompts {
		out = append(out, Command{Name: prompt.Command(), Description: prompt.Summary(), Hint: prompt.Hint()})
	}
	return out
}

// AvailableCommand is one entry of an ACP available_commands_update.
type AvailableCommand struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Input       *CommandInput `json:"input,omitempty"`
}

// CommandInput tells the client the command takes free-form input.
type CommandInput struct {
	Hint string `json:"hint"`
}

// CommandsUpdate is the session/update payload announcing commands.
type CommandsUpdate struct {
	SessionUpdate     string             `json:"sessionUpdate"`
	AvailableCommands []AvailableCommand `json:"availableCommands"`
}

// NewCommandsUpdate builds the available_commands_update for commands.
func NewCommandsUpdate(commands []Command) CommandsUpdate {
	available := make([]AvailableCommand, 0, len(commands))
	for _, command := range commands {
		entry := AvailableCommand{Name: command.Name, Description: command.Description}
		if command.Hint != "" {
			entry.Input = &CommandInput{Hint: command.Hint}
		}
		available = append(available, entry)
	}
	return CommandsUpdate{SessionUpdate: "available_commands_update", AvailableCommands: available}
}

// announceCommands publishes a changed prompt list to every watcher.
func (c *sessionCore) announceCommands(prompts []mcpclient.Prompt) {
	payload := NewCommandsUpdate(commands(prompts))
	c.hub.Emit(update(payload.SessionUpdate, map[string]any{"availableCommands": payload.AvailableCommands}, time.Now()))
}

// expandCommand replaces a prompt that invokes an MCP prompt with that
// prompt's messages, as the user turn. Other prompts are returned unchanged.
func (c *sessionCore) expandCommand(ctx context.Context, p *Prompt) (*Prompt, error) {
	if !c.mcpEnabled || !strings.HasPrefix(p.Text, "/") {
		return p, nil
	}
	blocks, matched, err := c.mcp.ExpandPrompt(ctx, p.Text)
	if err != nil || !matched {
		return p, err
	}
	expanded := *p
	expanded.Media = nil
	var text []string
	for _, block := range blocks {
		if block.Type == types.BlockText {
			text = append(text, block.Text)
		} else {
			expanded.Media = append(expanded.Media, block)
		}
	}
	expanded.Text = strings.Join(text, "\n\n")
	expanded.Media = append(expanded.Media, p.Media...)
	return &expanded, nil
}

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
