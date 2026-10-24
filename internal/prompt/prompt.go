// Package prompt builds the capability-templated system prompt for a coding session.
package prompt

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"go.harness.dev/harness/internal/skills"
)

// ContextFile pairs a file path with its content for inclusion in the system prompt.
type ContextFile struct {
	Path    string
	Content string
}

// BuildSystemPromptOptions controls the typed data projected into the system prompt.
type BuildSystemPromptOptions struct {
	ProductName        string
	CustomPrompt       string
	SelectedTools      []string
	ToolSnippets       map[string]string
	PromptGuidelines   []string
	AppendSystemPrompt string
	Cwd                string
	// ProgressPath is the todo_write checkpoint, empty when the session has none.
	ProgressPath      string
	ContextFiles      []ContextFile
	Skills            []skills.Skill
	Rules             []PromptRule
	GenericRules      []string
	Personality       string
	RemoteEnvironment bool
	Now               time.Time
}

// BuildSystemPrompt renders the embedded system prompt template. Custom prompts
// replace the default template while retaining only the session capability,
// append-content, context, skills, and date/cwd projections.
func BuildSystemPrompt(options BuildSystemPromptOptions) string {
	now := options.Now
	if now.IsZero() {
		now = time.Now()
	}

	tools := append([]string(nil), options.SelectedTools...)
	promptTools := make([]PromptTool, 0, len(tools))
	for _, name := range tools {
		promptTools = append(promptTools, PromptTool{Name: name, Label: options.ToolSnippets[name]})
	}

	promptSkills := visiblePromptSkills(options.Skills)
	data := PromptData{
		ProductName:         defaultProductName(options.ProductName),
		CustomPrompt:        options.CustomPrompt,
		HasCustomPrompt:     options.CustomPrompt != "",
		CapabilitiesSection: CapabilitiesSection(tools, options.ToolSnippets, options.ProgressPath, options.RemoteEnvironment),
		AppendSystemPrompt:  options.AppendSystemPrompt,
		Cwd:                 filepath.ToSlash(options.Cwd),
		Date:                now.Format("2006-01-02"),
		ContextFiles:        options.ContextFiles,
		HasContextFiles:     len(options.ContextFiles) > 0,
		Tools:               promptTools,
		Skills:              promptSkills,
		HasSkills:           hasTool(tools, "read") && len(promptSkills) > 0,
		Rules:               options.Rules,
		HasRules:            len(options.Rules) > 0,
		GenericRules:        compactStrings(options.GenericRules),
		Guidelines:          buildGuidelines(tools, options.PromptGuidelines),
		Personality:         options.Personality,
		HasRead:             hasTool(tools, "read"),
		HasBash:             hasTool(tools, "bash"),
		HasEdit:             hasTool(tools, "edit"),
		HasHashline:         hasTool(tools, "hashline"),
		HasWrite:            hasTool(tools, "write"),
		HasGrep:             hasTool(tools, "grep"),
		HasFind:             hasTool(tools, "find"),
		HasLs:               hasTool(tools, "ls"),
		HasTodoWrite:        hasTool(tools, "todo_write"),
		HasTask:             hasTool(tools, "task"),
		HasSkill:            hasTool(tools, "skill"),
		HasWebSearch:        hasTool(tools, "web_search"),
		HasWebFetch:         hasTool(tools, "web_fetch"),
		HasAttachment:       hasTool(tools, "attachment"),
	}
	data.HasGenericRules = len(data.GenericRules) > 0
	return renderSystemPrompt(data)
}

func defaultProductName(productName string) string {
	if strings.TrimSpace(productName) == "" {
		return "Harness"
	}
	return productName
}

// visiblePromptSkills hides skills the model can't invoke, so the prompt doesn't advertise dead capabilities.
func visiblePromptSkills(all []skills.Skill) []PromptSkill {
	visible := make([]PromptSkill, 0, len(all))
	for _, skill := range all {
		if skill.DisableModelInvocation {
			continue
		}
		visible = append(visible, PromptSkill{
			Name:        skill.Name,
			Description: skill.Description,
			Location:    "skill://" + skill.Name,
		})
	}
	return visible
}

// CapabilitiesSection renders the canonical, authoritative block naming the
// tools the agent actually holds this session plus how its runtime works.
// names preserves order; snippets supplies each tool's one-line purpose. A tool
// without a snippet is skipped. progressPath is the todo_write checkpoint in
// the session data directory, empty when the session has none.
func CapabilitiesSection(names []string, snippets map[string]string, progressPath string, remoteEnvironment bool) string {
	lines := make([]string, 0, len(names))
	for _, name := range names {
		if snippets == nil || snippets[name] == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", name, snippets[name]))
	}
	if len(lines) == 0 {
		return ""
	}
	block := "\n\n<available_tools>\n" +
		"These are the only tools available in this session. Use exactly these. Do not assume a tool, subagent, or capability that is not listed. If a task needs something not listed here, say so plainly instead of inventing a tool.\n" +
		strings.Join(lines, "\n") +
		"\n</available_tools>"

	runtime := []string{
		"- You run as an automated agent inside an isolated, ephemeral sandbox. Your working directory (shown below) is the project workspace: make file edits and run commands there.",
		"- The sandbox is ephemeral: nothing persists beyond this build session. Network access exists and can be flaky or limited; retry transient failures when appropriate. Destructive shell commands are refused.",
	}
	if has(names, "read") {
		runtime = append(runtime, `- Tool output is capped. When a result ends with "Full output: <path>", the complete output is saved at that path. Read it back with the read tool instead of re-running the command.`)
	}
	if has(names, "todo_write") && progressPath != "" {
		runtime = append(runtime, "- Your live progress checkpoint is local://progress.md (session data, outside the workspace); todo_write rewrites it on every update. Read it with the read tool to recover the plan and current step after an interruption.")
	}
	if has(names, "read") {
		runtime = append(runtime, "- read and grep take internal URLs: skill://<name>/<file> (skill files), job:// and job://<id>/<artifact> (background jobs, e.g. output.log), history://<id> (a child session's transcript) and local://<path>, a private scratch directory shared by this session tree. To hand a large payload to a child or parent, write it to local://<name> and pass that URL.")
		if has(names, "bash") && !remoteEnvironment {
			runtime = append(runtime, "- In local sessions, bash expands skill:// and local:// words to paths.")
		}
	}
	return block + "\n\n<runtime>\n" + strings.Join(runtime, "\n") + "\n</runtime>"
}

func has(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

// buildGuidelines preserves caller order while dropping duplicate rules before adding the session defaults.
func buildGuidelines(tools []string, extra []string) []string {
	seen := map[string]bool{}
	guidelines := make([]string, 0, len(extra)+2)
	add := func(guideline string) {
		guideline = strings.TrimSpace(guideline)
		if guideline == "" || seen[guideline] {
			return
		}
		seen[guideline] = true
		guidelines = append(guidelines, guideline)
	}
	if hasTool(tools, "bash") && !hasTool(tools, "grep") && !hasTool(tools, "find") && !hasTool(tools, "ls") {
		add("Use bash for file operations like ls, grep, and find")
	}
	for _, guideline := range extra {
		add(guideline)
	}
	add("Be concise in your responses")
	add("Show file paths clearly when working with files")
	return guidelines
}

func compactStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func hasTool(tools []string, name string) bool {
	return has(tools, name)
}
