// Package agentfiles loads agent types defined as markdown files with YAML
// frontmatter and maps them onto extension.AgentPreset. An agent file is
// prompt text: it can narrow a child's tools and pick its model, never widen
// what the child may do.
package agentfiles

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"go.harness.dev/harness/internal/adapter"
	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/outputschema"
	"go.harness.dev/harness/internal/skills"
	"go.harness.dev/harness/internal/tools"
	"go.harness.dev/harness/tempest/extension"
)

// Model role aliases an agent file may name instead of a model spec.
const (
	RoleDefault = "default"
	RolePlan    = "plan"
	RoleSmol    = "smol"
)

// claudeModelRoles maps Claude Code's agent model aliases onto roles so
// .claude/agents files load unchanged.
var claudeModelRoles = map[string]string{"inherit": RoleDefault, "sonnet": RoleDefault, "opus": RoleDefault, "haiku": RoleSmol}

// claudeToolNames maps Claude Code tool names onto the harness tools that do
// the same job, so a .claude/agents allowlist narrows to the same set.
var claudeToolNames = map[string]string{
	"Read": "read", "Write": "write", "Edit": "edit", "MultiEdit": "edit", "Grep": "grep", "Glob": "find",
	"LS": "ls", "Bash": "bash", "WebFetch": "web_fetch", "WebSearch": "web_search", "TodoWrite": "todo_write", "Task": "task",
}

// reservedNames are the task tool's built-in roles.
var reservedNames = []string{"explore", "general"}

// Agent is one loaded agent file.
type Agent struct {
	Preset extension.AgentPreset
	// Source is the file that defined it.
	Source string
}

// FieldError is an invalid agent file; Field names the frontmatter key, or
// "frontmatter" / "body" for the file's structure.
type FieldError struct {
	Path  string
	Field string
	Err   error
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("agent file %s: %s: %v", e.Path, e.Field, e.Err)
}
func (e *FieldError) Unwrap() error { return e.Err }

// Diagnostic is a file skipped at load: invalid, or shadowed by an earlier
// definition of the same name.
type Diagnostic struct {
	Path    string
	Message string
	Err     error
}

// Options locates and validates agent files.
type Options struct {
	// ProjectDir holds .harness/agents and .claude/agents.
	ProjectDir string
	// AgentDir holds agents/ for user-level files.
	AgentDir string
	// Providers are the trusted configured routes a model spec may name.
	Providers map[string]adapter.ProviderConfig
}

// Dirs lists the agent directories in precedence order.
func Dirs(options Options) []string {
	var dirs []string
	if options.ProjectDir != "" {
		dirs = append(dirs, filepath.Join(options.ProjectDir, config.ProjectConfigDirName, "agents"), filepath.Join(options.ProjectDir, ".claude", "agents"))
	}
	if options.AgentDir != "" {
		dirs = append(dirs, filepath.Join(options.AgentDir, "agents"))
	}
	return dirs
}

// Load reads every *.md file of each agent directory, sorted by name within a
// directory. The first definition of a name wins; invalid and shadowed files
// are diagnostics, not failures. Only an unreadable directory fails.
func Load(options Options) ([]Agent, []Diagnostic, error) {
	var agents []Agent
	var diagnostics []Diagnostic
	byName := map[string]string{}
	for _, dir := range Dirs(options) {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read agent directory %s: %w", dir, err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
				names = append(names, entry.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			path := filepath.Join(dir, name)
			agent, err := Parse(path, options.Providers)
			if err != nil {
				diagnostics = append(diagnostics, Diagnostic{Path: path, Message: err.Error(), Err: err})
				continue
			}
			if first, taken := byName[agent.Preset.ID]; taken {
				diagnostics = append(diagnostics, Diagnostic{Path: path, Message: fmt.Sprintf("agent %q is already defined by %s; this file is ignored", agent.Preset.ID, first)})
				continue
			}
			byName[agent.Preset.ID] = path
			agents = append(agents, agent)
		}
	}
	return agents, diagnostics, nil
}

// frontmatter is the YAML header. Tools and output_schema keep their raw
// node so both list and string forms parse; unknown keys are ignored.
type frontmatter struct {
	Name         string    `yaml:"name"`
	Description  string    `yaml:"description"`
	Tools        yaml.Node `yaml:"tools"`
	Model        string    `yaml:"model"`
	Reasoning    string    `yaml:"reasoning"`
	OutputSchema yaml.Node `yaml:"output_schema"`
}

// Parse reads one agent file.
func Parse(path string, providers map[string]adapter.ProviderConfig) (Agent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Agent{}, &FieldError{Path: path, Field: "file", Err: err}
	}
	header, body, ok := split(string(data))
	if !ok {
		return Agent{}, &FieldError{Path: path, Field: "frontmatter", Err: errors.New("file must start with a --- delimited YAML header")}
	}
	fail := func(field string, err error) (Agent, error) {
		return Agent{}, &FieldError{Path: path, Field: field, Err: err}
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(header), &fm); err != nil {
		return fail("frontmatter", err)
	}
	name := strings.TrimSpace(fm.Name)
	if name == "" {
		return fail("name", errors.New("required"))
	}
	if problems := skills.ValidateName(name); len(problems) > 0 {
		return fail("name", errors.New(strings.Join(problems, "; ")))
	}
	if slices.Contains(reservedNames, name) {
		return fail("name", fmt.Errorf("%q is a built-in task role", name))
	}
	description := strings.TrimSpace(fm.Description)
	if problems := skills.ValidateDescription(description); len(problems) > 0 {
		return fail("description", errors.New(strings.Join(problems, "; ")))
	}
	allowed, err := toolList(fm.Tools)
	if err != nil {
		return fail("tools", err)
	}
	model, err := modelSetting(fm.Model, providers)
	if err != nil {
		return fail("model", err)
	}
	reasoning := strings.ToLower(strings.TrimSpace(fm.Reasoning))
	if reasoning != "" && !slices.Contains(config.ReasoningLevels, reasoning) {
		return fail("reasoning", fmt.Errorf("unknown level %q (valid: %s)", reasoning, strings.Join(config.ReasoningLevels, ", ")))
	}
	schema, err := outputSchema(fm.OutputSchema)
	if err != nil {
		return fail("output_schema", err)
	}
	instructions := strings.TrimSpace(body)
	if instructions == "" {
		return fail("body", errors.New("the instructions below the header are empty"))
	}
	return Agent{Source: path, Preset: extension.AgentPreset{
		ID: name, Version: version(data), Description: description, Instructions: instructions,
		Model: model, AllowedTools: allowed, Reasoning: reasoning, OutputSchema: schema,
	}}, nil
}

// split separates a leading --- header from the body.
func split(content string) (header, body string, ok bool) {
	normalized := strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\r", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return "", "", false
	}
	rest := normalized[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", false
	}
	body = rest[end+len("\n---"):]
	if newline := strings.IndexByte(body, '\n'); newline >= 0 {
		body = body[newline+1:]
	} else {
		body = ""
	}
	return rest[:end], body, true
}

// toolList accepts a YAML list or a comma-separated string. Absent means the
// default child tool set; an empty list gives the child no tools. Every name
// must be a harness tool (or a Claude Code name for one) or an MCP tool,
// whose server is only known once the session starts.
func toolList(node yaml.Node) ([]string, error) {
	var raw []string
	if node.Kind == 0 || node.Tag == "!!null" {
		return nil, nil // absent or blank: the default child tool set
	}
	switch node.Kind {
	case yaml.ScalarNode:
		for _, item := range strings.Split(node.Value, ",") {
			if item = strings.TrimSpace(item); item != "" {
				raw = append(raw, item)
			}
		}
	case yaml.SequenceNode:
		if err := node.Decode(&raw); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("must be a list of tool names or a comma-separated string")
	}
	allowed := make([]string, 0, len(raw))
	for _, name := range raw {
		name = strings.TrimSpace(name)
		if mapped, ok := claudeToolNames[name]; ok {
			name = mapped
		}
		if !tools.AllToolNames[tools.ToolName(name)] && !strings.HasPrefix(name, "mcp__") {
			return nil, fmt.Errorf("unknown tool %q", name)
		}
		if !slices.Contains(allowed, name) {
			allowed = append(allowed, name)
		}
	}
	return allowed, nil
}

// modelSetting keeps a role alias or validates a model spec. A spec with its
// own endpoint would send the route's credential to a URL named by a
// repository file, so agent files may not carry one.
func modelSetting(raw string, providers map[string]adapter.ProviderConfig) (string, error) {
	model := strings.TrimSpace(raw)
	if role, ok := claudeModelRoles[strings.ToLower(model)]; ok {
		return role, nil
	}
	switch model {
	case "", RoleDefault, RolePlan, RoleSmol:
		return model, nil
	}
	if adapter.HasModelBaseURL(model) {
		return "", errors.New("an @http(s) endpoint is not allowed in an agent file; define the route in settings providers")
	}
	if err := adapter.ValidateModelSpecWithProviders(model, providers); err != nil {
		return "", err
	}
	return model, nil
}

// outputSchema accepts a YAML mapping or a JSON string and returns compact
// JSON that compiles as an output schema.
func outputSchema(node yaml.Node) (json.RawMessage, error) {
	if node.Kind == 0 || node.Tag == "!!null" {
		return nil, nil
	}
	var raw json.RawMessage
	switch node.Kind {
	case yaml.ScalarNode:
		raw = json.RawMessage(node.Value)
	case yaml.MappingNode:
		var value map[string]any
		if err := node.Decode(&value); err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		raw = encoded
	default:
		return nil, errors.New("must be a JSON Schema object")
	}
	schema, err := outputschema.Compile(raw)
	if err != nil {
		return nil, err
	}
	return schema.Raw(), nil
}

// version changes whenever the file does, so a task recorded against an
// older definition fails clearly instead of running a different agent.
func version(data []byte) uint32 {
	hash := fnv.New32a()
	hash.Write(data) // hash.Hash writes never fail.
	if sum := hash.Sum32(); sum != 0 {
		return sum
	}
	return 1
}
