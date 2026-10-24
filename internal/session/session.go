package session

import (
	"slices"
	"strings"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/prompt"
	"go.harness.dev/harness/internal/skills"
	"go.harness.dev/harness/internal/tools"
)

// BuildToolRegistry creates the full tool set for the given cwd and options.
func BuildToolRegistry(cwd string, opts tools.ToolsOptions) map[tools.ToolName]agent.AgentTool {
	return tools.AllTools(cwd, opts)
}

// SelectActive filters a tool map by allowed/excluded name lists. When allowed
// is empty, all tools are active (minus excluded) in name order; otherwise only
// those in allowed but not in excluded are returned, in allowed order. The
// order is deterministic because the tool list heads every provider request:
// a reshuffle between requests or sessions would miss the prompt cache.
func SelectActive(all map[tools.ToolName]agent.AgentTool, allowed, excluded []string, mode string) []agent.AgentTool {
	allowAll := len(allowed) == 0
	allowedSet := map[string]bool{}
	for _, n := range allowed {
		allowedSet[n] = true
	}
	excludedSet := map[string]bool{}
	for _, n := range excluded {
		excludedSet[n] = true
	}

	if allowAll {
		active := make([]agent.AgentTool, 0, len(all))
		for name, tool := range all {
			if excludedSet[string(name)] {
				continue
			}
			active = append(active, tool)
		}
		slices.SortFunc(active, func(left, right agent.AgentTool) int { return strings.Compare(left.Name, right.Name) })
		return active
	}

	active := make([]agent.AgentTool, 0, len(allowed))
	for _, name := range allowed {
		if excludedSet[name] {
			continue
		}
		if tool, ok := all[tools.ToolName(name)]; ok {
			active = append(active, tool)
		}
	}
	return active
}

// BuildToolSnippets returns a short description for each active tool's name,
// used in the system prompt's available-tools section.
func BuildToolSnippets(active []agent.AgentTool) map[string]string {
	snippets := map[string]string{}
	for _, tool := range active {
		label := tool.Label
		if label == "" {
			label = tool.Name
		}
		switch tool.Name {
		case "read":
			snippets[tool.Name] = "Read files (several in one call with paths), directories, archive members, office documents, JSON queries and internal URLs"
		case "bash":
			snippets[tool.Name] = "Execute bash commands (ls, grep, find, etc.)"
		case "eval":
			snippets[tool.Name] = "Run Python or JavaScript cells in persistent per-session kernels; reset after resume"
		case "edit":
			snippets[tool.Name] = "Make precise file edits with exact text replacement"
		case "hashline":
			snippets[tool.Name] = "Edit, move, rename or delete existing files with line-anchored [path#TAG] patches"
		case "integrate":
			snippets[tool.Name] = "Three-way merge a finished general task's changes into the live workspace"
		case "write":
			snippets[tool.Name] = "Create or overwrite files"
		case "grep":
			snippets[tool.Name] = "Search file contents for patterns, in one path or several"
		case "find":
			snippets[tool.Name] = "Find files by glob pattern"
		case "ls":
			snippets[tool.Name] = "List directory contents"
		case "todo_write":
			snippets[tool.Name] = "Update the session todo list (full list or phased ops) and progress checkpoint"
		case "task":
			snippets[tool.Name] = "Delegate a focused task to a child agent"
		case "message":
			snippets[tool.Name] = "Message your parent, a running child or all peers"
		case "write_stdin":
			snippets[tool.Name] = "Type into a background bash job (pty for REPLs and prompts) and read what it printed back"
		case "web_search":
			snippets[tool.Name] = "Search the web through the configured endpoint"
		case "web_fetch":
			snippets[tool.Name] = "Fetch an HTTP(S) URL"
		case "skill":
			snippets[tool.Name] = "Load a bundled skill's full instructions by name"
		case "attachment":
			snippets[tool.Name] = "Inspect files attached to the session (list, read, grep, stats)"
		case "lsp_definition":
			snippets[tool.Name] = "Find a symbol's definition with the language server"
		case "lsp_references":
			snippets[tool.Name] = "Find a symbol's references with the language server"
		case "lsp_rename":
			snippets[tool.Name] = "Plan a symbol rename as anchored edits to apply with edit"
		case "lsp_diagnostics":
			snippets[tool.Name] = "Report a file's language-server diagnostics and their freshness"
		case "checkpoint":
			snippets[tool.Name] = "Save workspace files and conversation before an investigation"
		case "rewind":
			snippets[tool.Name] = "Restore checkpoint files and fork conversation, retaining findings"
		case "manifest_diff":
			snippets[tool.Name] = "Compare two rendered Kubernetes manifest snapshots by resource"
		case "view_image":
			snippets[tool.Name] = "Look at a PNG, JPEG, GIF or WebP image in the workspace"
		default:
			snippets[tool.Name] = label
		}
	}
	return snippets
}

// ToolNames extracts the names from an active tool slice.
func ToolNames(active []agent.AgentTool) []string {
	names := make([]string, len(active))
	for i, tool := range active {
		names[i] = tool.Name
	}
	return names
}

// RebuildSystemPromptOptions contains the active tool set and prompt options.
type RebuildSystemPromptOptions struct {
	ProductName        string
	CustomPrompt       string
	ToolSnippets       map[string]string
	PromptGuidelines   []string
	AppendSystemPrompt string
	Cwd                string
	// ProgressPath is the todo_write checkpoint, empty when the session has none.
	ProgressPath string
	ContextFiles []prompt.ContextFile
	Skills       []skills.Skill
	Rules        []prompt.PromptRule
	GenericRules []string
	Personality  string
	ActiveTools  []agent.AgentTool
}

// RebuildSystemPrompt assembles the full system prompt from the given options,
// including the tool list, guidelines, context files, and skills.
func RebuildSystemPrompt(opts RebuildSystemPromptOptions) string {
	active := opts.ActiveTools
	names := ToolNames(active)
	snippets := opts.ToolSnippets
	if snippets == nil {
		snippets = BuildToolSnippets(active)
	}
	return prompt.BuildSystemPrompt(prompt.BuildSystemPromptOptions{
		ProductName:        opts.ProductName,
		CustomPrompt:       opts.CustomPrompt,
		SelectedTools:      names,
		ToolSnippets:       snippets,
		PromptGuidelines:   opts.PromptGuidelines,
		AppendSystemPrompt: opts.AppendSystemPrompt,
		Cwd:                opts.Cwd,
		ProgressPath:       opts.ProgressPath,
		ContextFiles:       opts.ContextFiles,
		Skills:             opts.Skills,
		Rules:              opts.Rules,
		GenericRules:       opts.GenericRules,
		Personality:        opts.Personality,
	})
}

// AppendCapabilities appends the canonical available-tools block for active to
// base, so a child agent's prompt names the exact tools it holds. It reuses the
// same formatter and snippets as the orchestrator prompt, keeping every agent's
// capability list derived from the real tool registry rather than prose.
func AppendCapabilities(base string, active []agent.AgentTool) string {
	return base + prompt.CapabilitiesSection(ToolNames(active), BuildToolSnippets(active), "")
}
