// Package subagent exposes the nonblocking, session-owned task tool.
package subagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.harness.dev/harness/internal/agent"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/schema"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/extension"
)

// PresetResolver resolves data against the provider request's pinned generation.
type PresetResolver func(context.Context, string) (extension.AgentPreset, error)

// PresetDigest identifies a preset's whole definition. A task records it
// next to the preset's version, so recovery runs exactly the definition the
// task started with: a version is a 32-bit number an agent file's changed
// bytes can collide with, the digest is not.
func PresetDigest(preset extension.AgentPreset) string {
	hash := sha256.New()
	// Each field is length-prefixed, so no two definitions share an encoding.
	field := func(value string) { fmt.Fprintf(hash, "%d:%s;", len(value), value) }
	field(preset.ID)
	fmt.Fprintf(hash, "%d;", preset.Version)
	field(preset.Description)
	field(preset.Instructions)
	field(preset.Model)
	field(preset.Reasoning)
	field(string(preset.OutputSchema))
	// A nil allowlist (default tools) differs from an empty one (no tools).
	fmt.Fprintf(hash, "%t%d;", preset.AllowedTools == nil, len(preset.AllowedTools))
	for _, tool := range preset.AllowedTools {
		field(tool)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// builtinRoles describe the task tool's two built-in subagent types.
var builtinRoles = map[string]string{
	"explore": "read-only investigation on the smol model",
	"general": "full child with edit and bash tools, in an isolated git worktree when eligible",
}

// PresetListing describes the actual preset names accepted by the task
// adapter, beside the built-in roles the presets map does not redefine.
func PresetListing(presets map[string]extension.AgentPreset) string {
	if len(presets) == 0 {
		return ""
	}
	descriptions := make(map[string]string, len(presets)+len(builtinRoles))
	for name, description := range builtinRoles {
		descriptions[name] = description
	}
	for name, preset := range presets {
		descriptions[name] = preset.Description
	}
	names := make([]string, 0, len(descriptions))
	for name := range descriptions {
		names = append(names, name)
	}
	sort.Strings(names)
	var listing strings.Builder
	listing.WriteString("<available_subagents>\nDelegate with the task tool using ONLY these subagent_type values. There is no \"general-purpose\" type; never invent one.\n")
	for _, name := range names {
		fmt.Fprintf(&listing, "- %s: %s\n", name, descriptions[name])
	}
	listing.WriteString("</available_subagents>")
	return listing.String()
}

// PresetSchema keeps named presets and built-in roles on the same task tool.
func PresetSchema(names []string) json.RawMessage {
	all := append([]string{"explore", "general"}, names...)
	return schema.Object(schema.JSON{"description": schema.String(), "prompt": schema.String(), "subagent_type": schema.JSON{"type": "string", "enum": all}, "output_schema": schema.JSON{"type": "object"}}, "prompt", "subagent_type")
}

// NewTool delegates only through Controller, so no child can bypass journaling.
func NewTool(controller work.Controller) agent.AgentTool {
	return NewPresetTool(controller, nil, nil)
}

// NewPresetTool adapts named immutable presets to the existing child supervisor.
func NewPresetTool(controller work.Controller, names []string, resolve PresetResolver) agent.AgentTool {
	return agent.AgentTool{
		Tool:  ptypes.Tool{Name: "task", Description: "Start a delegated task using a listed agent preset. Returns a job handle immediately; completion arrives as a follow-up. General tasks use an isolated git worktree when eligible, otherwise shared cwd. A worktree child's patch is never applied on its own: merge it into the parent workspace with integrate.", Parameters: PresetSchema(names)},
		Label: "task",
		Execute: func(ctx context.Context, _ string, params json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			if controller == nil {
				return agent.AgentToolResult{}, errors.New("task requires a durable root session")
			}
			var spec work.TaskSpec
			if err := json.Unmarshal(params, &spec); err != nil {
				return agent.AgentToolResult{}, err
			}
			if spec.SubagentType != "" && spec.SubagentType != "explore" && spec.SubagentType != "general" {
				if resolve == nil {
					return agent.AgentToolResult{}, errors.New("named agent preset unavailable")
				}
				preset, err := resolve(ctx, spec.SubagentType)
				if err != nil {
					return agent.AgentToolResult{}, err
				}
				spec.PresetID, spec.PresetVersion, spec.PresetDigest = spec.SubagentType, preset.Version, PresetDigest(preset)
				spec.SubagentType = "general"
				if len(spec.OutputSchema) == 0 {
					spec.OutputSchema = preset.OutputSchema
				}
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
