// Package agentfiles loads agent types defined as markdown files with YAML
// frontmatter and maps them onto extension.AgentPreset. An agent file is
// prompt text: it can narrow a child's tools and pick its model, never widen
// what the child may do.
package agentfiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
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

// claudeModelRoles maps Claude Code's agent model aliases onto roles, so a
// .claude/agents file's model field needs no edit.
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

// Presets returns the agents' presets in load order.
func Presets(agents []Agent) []extension.AgentPreset {
	presets := make([]extension.AgentPreset, 0, len(agents))
	for _, agent := range agents {
		presets = append(presets, agent.Preset)
	}
	return presets
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
	// ProjectDir is the repository root that holds .harness/agents and
	// .claude/agents. Its files are read through it: one that links outside
	// it is skipped.
	ProjectDir string
	// AgentDir holds agents/ for the user's own files.
	AgentDir string
	// Providers are the trusted configured routes a model spec may name.
	Providers map[string]adapter.ProviderConfig
}

const (
	// MaxFileBytes bounds one agent file. Its body becomes a child's
	// instructions, so it gets the room the default context_files.max_bytes
	// gives all repository instructions together.
	MaxFileBytes = 64 << 10
	// MaxAgents bounds how many agent files load: each one's name and
	// description are listed in every root request's system prompt.
	MaxAgents = 64
	// maxDirEntries bounds the entries read from one agent directory.
	maxDirEntries = 1024
)

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

// Load reads the *.md files of each agent directory, sorted by name within
// a directory, up to MaxAgents files of at most MaxFileBytes each. The first
// definition of a name wins. Invalid, shadowed, oversized, non-regular and
// out-of-repository files, and files past the limits, are diagnostics, not
// failures; only an unreadable directory fails.
func Load(ctx context.Context, options Options) ([]Agent, []Diagnostic, error) {
	var agents []Agent
	var diagnostics []Diagnostic
	var project *os.Root
	if options.ProjectDir != "" {
		var err error
		project, err = os.OpenRoot(options.ProjectDir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("open project directory %s: %w", options.ProjectDir, err)
		}
		if project != nil {
			defer project.Close()
		}
	}
	byName := map[string]string{}
	for _, dir := range Dirs(options) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		files := plainFiles
		if project != nil && within(options.ProjectDir, dir) {
			files = rootFiles(project)
		}
		names, err := files.list(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		var skipped *skipError
		if errors.As(err, &skipped) {
			diagnostics = append(diagnostics, Diagnostic{Path: dir, Message: fmt.Sprintf("agent directory %s: %s", dir, skipped), Err: err})
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read agent directory %s: %w", dir, err)
		}
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			path := filepath.Join(dir, name)
			if len(agents) == MaxAgents {
				diagnostics = append(diagnostics, Diagnostic{Path: path, Message: fmt.Sprintf("not loaded: at most %d agent files load", MaxAgents)})
				continue
			}
			agent, err := parseFile(files, path, options.Providers)
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

// skipError is a directory or file skipped rather than read.
type skipError struct{ reason string }

func (e *skipError) Error() string { return e.reason }

// fileSource opens agent files: plainly for the user's agent dir, through
// the project root for the repository's.
type fileSource struct {
	stat func(string) (os.FileInfo, error)
	open func(string) (*os.File, error)
	// name maps an absolute path to the name stat and open take.
	name func(string) (string, error)
	// escapes reports whether a path that failed to open leaves the source.
	escapes func(string) bool
}

var plainFiles = fileSource{
	stat:    os.Stat,
	open:    os.Open,
	name:    func(path string) (string, error) { return path, nil },
	escapes: func(string) bool { return false },
}

func rootFiles(root *os.Root) fileSource {
	dir := root.Name()
	return fileSource{
		stat: root.Stat,
		open: root.Open,
		name: func(path string) (string, error) {
			rel, err := filepath.Rel(dir, path)
			if err != nil || !filepath.IsLocal(rel) {
				return "", &skipError{"outside the repository"}
			}
			return rel, nil
		},
		escapes: func(path string) bool {
			resolved, err := filepath.EvalSymlinks(path)
			canonical, rootErr := filepath.EvalSymlinks(dir)
			return err == nil && rootErr == nil && !within(canonical, resolved)
		},
	}
}

// list returns dir's *.md names, sorted, reading at most maxDirEntries
// entries.
func (files fileSource) list(dir string) ([]string, error) {
	name, err := files.name(dir)
	if err != nil {
		return nil, err
	}
	handle, err := files.open(name)
	if err != nil {
		if files.escapes(dir) {
			return nil, &skipError{"skipped: the agent directory links outside the repository"}
		}
		return nil, err
	}
	defer handle.Close()
	entries, err := handle.ReadDir(maxDirEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > maxDirEntries {
		return nil, &skipError{fmt.Sprintf("skipped: more than %d entries", maxDirEntries)}
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// read returns a regular file's bytes, refusing one over MaxFileBytes.
// Checking the type before opening keeps a FIFO from blocking the load.
func (files fileSource) read(path string) ([]byte, error) {
	name, err := files.name(path)
	if err != nil {
		return nil, err
	}
	info, err := files.stat(name)
	if err != nil {
		if files.escapes(path) {
			return nil, errors.New("links outside the repository")
		}
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if info.Size() > MaxFileBytes {
		return nil, fmt.Errorf("%d bytes, over the %d KiB agent file limit", info.Size(), MaxFileBytes>>10)
	}
	handle, err := files.open(name)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	data, err := io.ReadAll(io.LimitReader(handle, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("over the %d KiB agent file limit", MaxFileBytes>>10)
	}
	return data, nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && filepath.IsLocal(rel)
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
	return parseFile(plainFiles, path, providers)
}

func parseFile(files fileSource, path string, providers map[string]adapter.ProviderConfig) (Agent, error) {
	data, err := files.read(path)
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
	if schema == nil {
		// Compile reads empty input as "no schema"; a key given empty is a
		// mistake, not a request for free-form output.
		return nil, errors.New("must be a JSON Schema object, not empty")
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
