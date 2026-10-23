// Package subagent exposes the nonblocking, session-owned task tool.
package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.harness.dev/harness/internal/agent"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/schema"
	"go.harness.dev/harness/internal/work"
)

// Definition describes a child role used by the session's journaled runner.
type Definition struct {
	Description  string
	SystemPrompt string
	Tools        []agent.AgentTool
	Model        ptypes.Model
	StreamFn     agent.StreamFn
}

// Registry names the available child roles in the root system prompt.
type Registry map[string]Definition

// RegistryListing keeps the prompt's type names in sync with the task adapter.
func RegistryListing(reg Registry) string {
	if len(reg) == 0 {
		return ""
	}
	names := make([]string, 0, len(reg))
	for name := range reg {
		names = append(names, name)
	}
	sort.Strings(names)
	var listing strings.Builder
	listing.WriteString("<available_subagents>\nDelegate with the task tool using ONLY these subagent_type values. There is no \"general-purpose\" type; never invent one.\n")
	for _, name := range names {
		if description := strings.TrimSpace(reg[name].Description); description != "" {
			fmt.Fprintf(&listing, "- %s: %s\n", name, description)
		} else {
			fmt.Fprintf(&listing, "- %s\n", name)
		}
	}
	listing.WriteString("</available_subagents>")
	return listing.String()
}

// NewTool delegates only through Controller, so no child can bypass journaling.
func NewTool(controller work.Controller) agent.AgentTool {
	return agent.AgentTool{
		Tool:  ptypes.Tool{Name: "task", Description: "Start a delegated explore or general task. Returns a job handle immediately; completion arrives as a follow-up. General tasks use an isolated git worktree when eligible, otherwise shared cwd. A worktree child's patch is never applied on its own: merge it into the parent workspace with integrate.", Parameters: schema.Object(schema.JSON{"description": schema.String(), "prompt": schema.String(), "subagent_type": schema.JSON{"type": "string", "enum": []string{"explore", "general"}}, "output_schema": schema.JSON{"type": "object"}}, "prompt", "subagent_type")},
		Label: "task",
		Execute: func(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			if controller == nil {
				return agent.AgentToolResult{}, errors.New("task requires a durable root session")
			}
			var spec work.TaskSpec
			if err := json.Unmarshal(params, &spec); err != nil {
				return agent.AgentToolResult{}, err
			}
			job, err := controller.StartTask(ctx, spec)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			encoded, err := json.Marshal(job.Handle())
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			return agent.AgentToolResult{Content: []ptypes.ContentBlock{ptypes.NewText(string(encoded))}, Details: encoded}, nil
		},
	}
}

// FormatTaskSummary keeps the caller's task description next to its result.
func FormatTaskSummary(input, output string) string {
	input = strings.TrimSpace(input)
	output = strings.TrimSpace(output)
	if input == "" {
		return output
	}
	return fmt.Sprintf("Task: %s\n\n%s", input, output)
}
