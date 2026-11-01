package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"go.harness.dev/harness/internal/adapter"
	"go.harness.dev/harness/internal/obs"
)

// Settings layers, lowest precedence first. [Setting.Layer] holds one of them.
const (
	LayerDefault      = "default"
	LayerManaged      = "managed"
	LayerUser         = "user"
	LayerProject      = "project"
	LayerProjectLocal = "project-local"
	LayerProfile      = "profile"
	LayerEnv          = "env"
	LayerFlag         = "flag"
	LayerOverride     = "override"
	LayerRequirement  = "requirement"
)

// Setting is one effective settings value and the layer that set it. Value is
// JSON with credentials redacted.
type Setting struct {
	Key    string          `json:"key"`
	Value  json.RawMessage `json:"value"`
	Layer  string          `json:"layer"`
	Source string          `json:"source,omitempty"`
}

// profilesKey names the settings member that holds named partial settings.
const profilesKey = "profiles"

// overrideSource labels values set with -c.
const overrideSource = "-c"

// settingsLayer is one source of settings: a file, a selected profile inside a
// file, the HARNESS_* variables, the flags, or the -c overrides.
type settingsLayer struct {
	name string
	// file is the settings file, or the source label when origins is empty.
	file string
	// origins names the variable or flag that set each dotted key prefix.
	origins map[string]string
	profile string
	// project marks content read from a repository file.
	project bool
	// isFile marks a settings file, whose named providers entries replace whole.
	isFile   bool
	members  map[string]*settingsNode
	profiles map[string]*settingsNode
	// selectedProfile caches profileLayer's result.
	selectedProfile *settingsLayer
}

func (l *settingsLayer) origin(path []string) string {
	for depth := len(path); depth > 0; depth-- {
		if origin, ok := l.origins[strings.Join(path[:depth], ".")]; ok {
			return origin
		}
	}
	return l.file
}

// where describes the layer for warnings and errors.
func (l *settingsLayer) where(path []string) string {
	switch l.name {
	case LayerProfile:
		return fmt.Sprintf("profile %q in %s", l.profile, l.file)
	case LayerEnv:
		if origin := l.origin(path); origin != "" {
			return "environment variable " + origin
		}
		return "environment variables"
	case LayerFlag:
		if origin := l.origin(path); origin != "" {
			return "flag " + origin
		}
		return "flags"
	case LayerOverride:
		return "-c override"
	case LayerManaged:
		return "managed defaults in " + l.file
	}
	return fmt.Sprintf("%s settings %s", l.name, l.file)
}

// takeProfiles moves the profiles member out of the layer's settings.
func (l *settingsLayer) takeProfiles() error {
	definitions, ok := l.members[profilesKey]
	if !ok {
		return nil
	}
	delete(l.members, profilesKey)
	if definitions.isNull() {
		return nil
	}
	if definitions.object == nil {
		return &Error{Field: profilesKey, Err: fmt.Errorf("%s: must be an object of settings objects", l.where(nil))}
	}
	for name, definition := range definitions.object {
		if definition.object == nil && !definition.isNull() {
			return &Error{Field: profilesKey + "." + name, Err: fmt.Errorf("%s: a profile must be a settings object", l.where(nil))}
		}
	}
	l.profiles = definitions.object
	return nil
}

// profileLayer returns the named profile defined in this layer, or nil. The
// layer is built once, so checks and trust gating see one instance.
func (l *settingsLayer) profileLayer(name string) *settingsLayer {
	definition := l.profiles[name]
	if name == "" || definition == nil || definition.object == nil {
		return nil
	}
	if l.selectedProfile == nil {
		file := l.file
		if l.name == LayerOverride {
			file = overrideSource
		}
		l.selectedProfile = &settingsLayer{name: LayerProfile, file: file, profile: name, project: l.project, isFile: l.isFile, members: definition.clone().object}
	}
	return l.selectedProfile
}

// knownSettingKeys are the top-level settings members, from fileSettings tags.
var knownSettingKeys = func() map[string]bool {
	known := map[string]bool{}
	settingsType := reflect.TypeFor[fileSettings]()
	for index := range settingsType.NumField() {
		field := settingsType.Field(index)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name != "" && name != "-" {
			known[name] = true
		}
	}
	return known
}()

// check reports unknown members (a warning, or an error in strict mode) and
// rejects values of the wrong type, naming the layer and its file.
func (l *settingsLayer) check(strict bool) ([]string, error) {
	var warnings []string
	for _, key := range slices.Sorted(maps.Keys(l.members)) {
		if knownSettingKeys[key] {
			continue
		}
		if strict {
			return nil, &Error{Field: key, Err: fmt.Errorf("unknown setting in %s; remove it or run without -strict-config", l.where([]string{key}))}
		}
		warnings = append(warnings, fmt.Sprintf("unknown setting %q in %s is ignored", key, l.where([]string{key})))
	}
	encoded, err := json.Marshal(l.members)
	if err != nil {
		return nil, &Error{Field: "settings", Err: fmt.Errorf("%s: %w", l.where(nil), err)}
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if strict {
		decoder.DisallowUnknownFields()
	}
	var typed fileSettings
	if err := decoder.Decode(&typed); err != nil {
		return nil, &Error{Field: "settings", Err: fmt.Errorf("%s: %w", l.where(nil), err)}
	}
	return warnings, nil
}

// loadSettingsLayer reads one settings file; a missing file is no layer.
func loadSettingsLayer(name, path string, project bool) (*settingsLayer, error) {
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &Error{Field: "settings", Err: fmt.Errorf("read %s: %w", path, err)}
	}
	members, err := parseSettingsObject(contents)
	if err != nil {
		return nil, &Error{Field: "settings", Err: fmt.Errorf("decode %s: %w", path, err)}
	}
	layer := &settingsLayer{name: name, file: path, project: project, isFile: true, members: members}
	if err := layer.prepare(); err != nil {
		return nil, err
	}
	return layer, nil
}

// prepare moves profiles out of the layer and normalizes aliases in the layer
// and in each profile.
func (l *settingsLayer) prepare() error {
	normalizeAliases(l.members)
	if err := l.takeProfiles(); err != nil {
		return err
	}
	for _, definition := range l.profiles {
		if definition.object != nil {
			normalizeAliases(definition.object)
		}
	}
	return nil
}

// normalizeAliases maps Claude Code's disableAllHooks onto no_hooks, which wins
// when one layer sets both.
func normalizeAliases(members map[string]*settingsNode) {
	alias, ok := members["disableAllHooks"]
	if !ok {
		return
	}
	delete(members, "disableAllHooks")
	if _, set := members["no_hooks"]; !set {
		members["no_hooks"] = alias
	}
}

// defaultManagedSettingsPath is the administrator's file for this OS. It has
// no environment override, so a user's environment cannot drop requirements.
func defaultManagedSettingsPath() string {
	if runtime.GOOS == "darwin" {
		return "/Library/Application Support/Harness/managed-settings.json"
	}
	return "/etc/harness/managed-settings.json"
}

// managedRequirement pins one leaf path to a value.
type managedRequirement struct {
	path  []string
	value *settingsNode
}

// loadManagedSettings reads {"defaults": {...}, "requirements": {...}}. Every
// leaf under requirements is pinned. The file is strict: an administrator's
// typo must not silently drop a requirement.
func loadManagedSettings(path string) (*settingsLayer, []managedRequirement, error) {
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, &Error{Field: "managed settings", Err: fmt.Errorf("read %s: %w", path, err)}
	}
	sections, err := parseSettingsObject(contents)
	if err != nil {
		return nil, nil, &Error{Field: "managed settings", Err: fmt.Errorf("decode %s: %w", path, err)}
	}
	var defaults *settingsLayer
	var requirements []managedRequirement
	for _, section := range slices.Sorted(maps.Keys(sections)) {
		value := sections[section]
		if value.object == nil || section != "defaults" && section != "requirements" {
			return nil, nil, &Error{Field: "managed settings", Err: fmt.Errorf("%s: %q must be one of the objects \"defaults\" or \"requirements\"", path, section)}
		}
		if section == "defaults" {
			defaults = &settingsLayer{name: LayerManaged, file: path, isFile: true, members: value.object}
			if err := defaults.prepare(); err != nil {
				return nil, nil, err
			}
			continue
		}
		pinned := &settingsLayer{name: LayerRequirement, file: path, members: value.object}
		normalizeAliases(pinned.members)
		if _, ok := pinned.members[profilesKey]; ok {
			return nil, nil, &Error{Field: "managed settings", Err: fmt.Errorf("%s: requirements cannot pin profiles", path)}
		}
		if _, err := pinned.check(true); err != nil {
			return nil, nil, err
		}
		visitLeaves(pinned.members, nil, func(leafPath []string, leaf *settingsNode) {
			requirements = append(requirements, managedRequirement{path: leafPath, value: leaf})
		})
	}
	return defaults, requirements, nil
}

// inputKind is how a HARNESS_* variable's text becomes a settings value.
type inputKind int

const (
	inputString inputKind = iota
	inputInt
	inputInt64
	inputBool
	inputFloat
	inputList
)

// flagInput is a flag value that knows whether the caller set it.
type flagInput interface {
	settingsNode() (node *settingsNode, set bool, err error)
}

func (in *StringInput) settingsNode() (*settingsNode, bool, error) {
	if !in.Set {
		return nil, false, nil
	}
	node, err := stringNode(in.Value)
	return node, true, err
}

func (in *IntInput) settingsNode() (*settingsNode, bool, error) {
	if !in.Set {
		return nil, false, nil
	}
	return numberNode(strconv.Itoa(in.Value)), true, nil
}

func (in *BoolInput) settingsNode() (*settingsNode, bool, error) {
	if !in.Set {
		return nil, false, nil
	}
	return boolNode(in.Value), true, nil
}

func (in *StringsInput) settingsNode() (*settingsNode, bool, error) {
	if !in.Set {
		return nil, false, nil
	}
	node, err := stringsNode(in.Values)
	return node, true, err
}

// settingInput connects a settings key to its HARNESS_* variable and flag.
// A new setting with a variable or flag registers here, so both enter the
// layer stack below -c and show up in `config show`.
type settingInput struct {
	key   string
	env   string
	flag  string
	kind  inputKind
	field func(*FlagValues) flagInput
}

var settingInputs = []settingInput{
	{key: "model", env: "HARNESS_MODEL", flag: "model", field: func(f *FlagValues) flagInput { return &f.Model }},
	{key: "plan_model", env: "HARNESS_PLAN_MODEL", flag: "plan-model", field: func(f *FlagValues) flagInput { return &f.PlanModel }},
	{key: "smol_model", env: "HARNESS_SMOL_MODEL", flag: "smol-model", field: func(f *FlagValues) flagInput { return &f.SmolModel }},
	{key: "reasoning", env: "HARNESS_REASONING", flag: "reasoning", field: func(f *FlagValues) flagInput { return &f.Reasoning }},
	{key: "plan_reasoning", env: "HARNESS_PLAN_REASONING", flag: "plan-reasoning", field: func(f *FlagValues) flagInput { return &f.PlanReasoning }},
	{key: "smol_reasoning", env: "HARNESS_SMOL_REASONING", flag: "smol-reasoning", field: func(f *FlagValues) flagInput { return &f.SmolReasoning }},
	{key: "max_tokens", env: "HARNESS_MAX_TOKENS", flag: "max-tokens", kind: inputInt, field: func(f *FlagValues) flagInput { return &f.MaxTokens }},
	{key: "permission_mode_default", env: "HARNESS_PERMISSION_MODE", flag: "permission-mode", field: func(f *FlagValues) flagInput { return &f.PermissionMode }},
	{key: "bash_timeout", env: "HARNESS_BASH_TIMEOUT", flag: "bash-timeout", field: func(f *FlagValues) flagInput { return &f.BashTimeout }},
	{key: "log_level", env: "HARNESS_LOG_LEVEL", flag: "log-level", field: func(f *FlagValues) flagInput { return &f.LogLevel }},
	{key: "log_file", env: "HARNESS_LOG_FILE", flag: "log-file", field: func(f *FlagValues) flagInput { return &f.LogFile }},
	{key: "log_file_max_bytes", env: "HARNESS_LOG_FILE_MAX_BYTES", flag: "log-file-max-bytes", kind: inputInt64, field: func(f *FlagValues) flagInput { return &f.LogFileMaxBytes }},
	{key: "enable_task", env: "HARNESS_ENABLE_TASK", flag: "enable-task", kind: inputBool, field: func(f *FlagValues) flagInput { return &f.EnableTask }},
	{key: "plan", env: "HARNESS_PLAN", flag: "plan", kind: inputBool, field: func(f *FlagValues) flagInput { return &f.Plan }},
	{key: "attach", env: "HARNESS_ATTACH", flag: "attach", kind: inputList, field: func(f *FlagValues) flagInput { return &f.Attach }},
	{key: "enable_web", env: "HARNESS_ENABLE_WEB", flag: "enable-web", kind: inputBool, field: func(f *FlagValues) flagInput { return &f.EnableWeb }},
	{key: "web_search_url", env: "HARNESS_WEB_SEARCH_URL", flag: "web-search-url", field: func(f *FlagValues) flagInput { return &f.WebSearchURL }},
	{key: "agent_dir", env: EnvAgentDir, flag: "agent-dir", field: func(f *FlagValues) flagInput { return &f.AgentDir }},
	{key: "session_dir", env: EnvSessionDir, flag: "session-dir", field: func(f *FlagValues) flagInput { return &f.SessionDir }},
	{key: "anthropic_base_url", env: "HARNESS_ANTHROPIC_BASE_URL", flag: "anthropic-base-url", field: func(f *FlagValues) flagInput { return &f.AnthropicBaseURL }},
	{key: "openai_base_url", env: "HARNESS_OPENAI_BASE_URL", flag: "openai-base-url", field: func(f *FlagValues) flagInput { return &f.OpenAIBaseURL }},
	{key: "faux_script", env: "HARNESS_FAUX_SCRIPT", flag: "faux-script", field: func(f *FlagValues) flagInput { return &f.FauxScript }},
	{key: "skill_paths", flag: "skill", kind: inputList, field: func(f *FlagValues) flagInput { return &f.SkillPaths }},
	{key: "permission_allow_tools", env: "HARNESS_PERMISSION_ALLOW_TOOLS", flag: "allow-tool", kind: inputList, field: func(f *FlagValues) flagInput { return &f.PermissionAllowTools }},
	{key: "permission_deny_tools", env: "HARNESS_PERMISSION_DENY_TOOLS", flag: "deny-tool", kind: inputList, field: func(f *FlagValues) flagInput { return &f.PermissionDenyTools }},
	{key: "retry_enabled", env: "HARNESS_RETRY_ENABLED", kind: inputBool},
	{key: "retry_max_attempts", env: "HARNESS_RETRY_MAX_ATTEMPTS", kind: inputInt},
	{key: "retry_base_delay_ms", env: "HARNESS_RETRY_BASE_DELAY_MS", kind: inputInt},
	{key: "retry_backoff_cap_ms", env: "HARNESS_RETRY_BACKOFF_CAP_MS", kind: inputInt},
	{key: "retry_max_delay_ms", env: "HARNESS_RETRY_MAX_DELAY_MS", kind: inputInt},
	{key: "retry_jitter_min", env: "HARNESS_RETRY_JITTER_MIN", kind: inputFloat},
	{key: "retry_jitter_max", env: "HARNESS_RETRY_JITTER_MAX", kind: inputFloat},
	{key: "mcp_config", env: "HARNESS_MCP_CONFIG", flag: "mcp-config", field: func(f *FlagValues) flagInput { return &f.MCPConfig }},
	{key: "enable_lsp", env: "HARNESS_ENABLE_LSP", flag: "enable-lsp", kind: inputBool, field: func(f *FlagValues) flagInput { return &f.EnableLSP }},
	{key: "lsp_config", env: "HARNESS_LSP_CONFIG", flag: "lsp-config", field: func(f *FlagValues) flagInput { return &f.LSPConfig }},
	{key: "cloud_policy", env: "HARNESS_CLOUD_POLICY", flag: "cloud-policy", field: func(f *FlagValues) flagInput { return &f.CloudPolicy }},
	{key: "no_hooks", env: "HARNESS_NO_HOOKS", flag: "no-hooks", kind: inputBool, field: func(f *FlagValues) flagInput { return &f.NoHooks }},
	{key: "environment", env: "HARNESS_ENVIRONMENT", flag: "environment", field: func(f *FlagValues) flagInput { return &f.Environment }},
}

// parseEnvironmentValue converts a nonblank HARNESS_* value by kind.
func parseEnvironmentValue(kind inputKind, value string) (*settingsNode, error) {
	switch kind {
	case inputInt:
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return nil, err
		}
		return numberNode(strconv.Itoa(parsed)), nil
	case inputInt64:
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, err
		}
		return numberNode(strconv.FormatInt(parsed, 10)), nil
	case inputBool:
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return nil, err
		}
		return boolNode(parsed), nil
	case inputFloat:
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, err
		}
		if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return nil, errors.New("must be finite")
		}
		return numberNode(strconv.FormatFloat(parsed, 'g', -1, 64)), nil
	case inputList:
		return stringsNode(splitCommaList(value))
	}
	return stringNode(value)
}

// environmentLayer reads the HARNESS_* variables plus the Azure endpoint
// variables, which patch providers.azure field by field. A value that fails
// to parse is returned separately: it is an error only if no flag or -c
// override replaces it.
func environmentLayer(lookup func(string) string) (*settingsLayer, map[string]error, error) {
	layer := &settingsLayer{name: LayerEnv, members: map[string]*settingsNode{}, origins: map[string]string{}}
	invalid := map[string]error{}
	for _, input := range settingInputs {
		if input.env == "" {
			continue
		}
		value := strings.TrimSpace(lookup(input.env))
		if value == "" {
			continue
		}
		node, err := parseEnvironmentValue(input.kind, value)
		if err != nil {
			invalid[input.key] = err
			continue
		}
		layer.members[input.key] = node
		layer.origins[input.key] = input.env
	}
	if _, set := layer.members["log_level"]; !set && invalid["log_level"] == nil && strings.TrimSpace(lookup("HARNESS_DEBUG")) != "" {
		node, err := stringNode("debug")
		if err != nil {
			return nil, nil, err
		}
		layer.members["log_level"] = node
		layer.origins["log_level"] = "HARNESS_DEBUG"
	}
	for _, azure := range []struct {
		env  string
		path []string
	}{
		{"AZURE_OPENAI_ENDPOINT", []string{"providers", "azure", "base_url"}},
		{"AZURE_OPENAI_API_VERSION", []string{"providers", "azure", "query", "api-version"}},
	} {
		value := strings.TrimSpace(lookup(azure.env))
		if value == "" {
			continue
		}
		node, err := stringNode(value)
		if err != nil {
			return nil, nil, err
		}
		setMember(layer.members, azure.path, node)
		layer.origins[strings.Join(azure.path, ".")] = azure.env
	}
	return layer, invalid, nil
}

// flagLayer holds every config-bearing flag the caller set.
func flagLayer(flags FlagValues) (*settingsLayer, error) {
	if flags.SkillPaths.Set {
		for _, path := range flags.SkillPaths.Values {
			if strings.TrimSpace(path) == "" {
				return nil, &Error{Field: "skill", Err: errors.New("must not be empty")}
			}
		}
	}
	layer := &settingsLayer{name: LayerFlag, members: map[string]*settingsNode{}, origins: map[string]string{}}
	for _, input := range settingInputs {
		if input.field == nil {
			continue
		}
		node, set, err := input.field(&flags).settingsNode()
		if err != nil {
			return nil, &Error{Field: input.key, Err: err}
		}
		if set {
			layer.members[input.key] = node
			layer.origins[input.key] = "-" + input.flag
		}
	}
	return layer, nil
}

// overrideLayers parses repeated -c key.path=value overrides, one layer each
// in command-line order, so a later override applies on top of an earlier one
// exactly as a higher layer would. The value is JSON when it parses as JSON
// and a string otherwise.
func overrideLayers(overrides []string) ([]*settingsLayer, error) {
	layers := make([]*settingsLayer, 0, len(overrides))
	for _, override := range overrides {
		keyPath, rawValue, ok := strings.Cut(override, "=")
		path := strings.Split(strings.TrimSpace(keyPath), ".")
		if !ok || slices.Contains(path, "") {
			return nil, &Error{Field: overrideSource, Err: fmt.Errorf("%q: want key.path=value", override)}
		}
		value, err := parseSettingsNode([]byte(rawValue))
		if err != nil {
			if value, err = stringNode(rawValue); err != nil {
				return nil, &Error{Field: overrideSource, Err: err}
			}
		}
		layer := &settingsLayer{name: LayerOverride, file: overrideSource, members: map[string]*settingsNode{}}
		setMember(layer.members, path, value)
		if err := layer.prepare(); err != nil {
			return nil, err
		}
		layers = append(layers, layer)
	}
	return layers, nil
}

// trustGate is what a repository file needs before one of its settings applies.
type trustGate int

const (
	gateNone trustGate = iota
	// gateExecutable settings run code, read local files into the model
	// context, write outside the repository, or widen permissions.
	gateExecutable
	// gateCredentials settings choose where credentials are sent.
	gateCredentials
	// gateOperator settings only the operator may set, trusted or not.
	gateOperator
)

var projectGates = map[string]trustGate{
	"mcp_config":                gateExecutable,
	"mcp_servers":               gateExecutable,
	"lsp_config":                gateExecutable,
	"debug_adapters":            gateExecutable,
	"cloud_policy":              gateExecutable,
	"permission_mode_default":   gateExecutable,
	"permission_allow_tools":    gateExecutable,
	"attach":                    gateExecutable,
	"agent_dir":                 gateExecutable,
	"session_dir":               gateExecutable,
	"log_file":                  gateExecutable,
	"faux_script":               gateExecutable,
	"providers":                 gateCredentials,
	"anthropic_base_url":        gateCredentials,
	"openai_base_url":           gateCredentials,
	"web_search_url":            gateCredentials,
	"no_hooks":                  gateOperator,
	"trusted_hook_projects":     gateOperator,
	"trusted_provider_projects": gateOperator,
	"environment":               gateOperator,
	"environments":              gateOperator,
}

// projectGate classifies one repository setting. A model with an @http(s)
// endpoint suffix sends the route's credential there, so it needs provider trust.
func projectGate(key string, value *settingsNode) trustGate {
	switch key {
	case "model", "plan_model", "smol_model":
		var spec string
		if json.Unmarshal(value.scalar, &spec) == nil && (strings.Contains(spec, "@http://") || strings.Contains(spec, "@https://")) {
			return gateCredentials
		}
		return gateNone
	}
	return projectGates[key]
}

// droppedSetting records a repository setting the trust gate removed.
type droppedSetting struct {
	file   string
	key    string
	gate   trustGate
	reason string
}

// applyProjectGates removes repository settings the operator has not trusted.
// Top-level project hooks are not merged here: the hook runner appends them
// per session workspace behind the same trust (internal/hooks.ResolveProject).
func (l *settingsLayer) applyProjectGates(executableTrusted, credentialsTrusted bool) []droppedSetting {
	var dropped []droppedSetting
	for _, key := range slices.Sorted(maps.Keys(l.members)) {
		if key == "hooks" {
			if l.name == LayerProfile {
				dropped = append(dropped, droppedSetting{l.where(nil), key, gateOperator, "project hooks belong at the top level of the project file"})
			}
			delete(l.members, key)
			continue
		}
		gate := projectGate(key, l.members[key])
		var reason string
		switch gate {
		case gateExecutable:
			if !executableTrusted {
				reason = "needs -trust-project-hooks or user trusted_hook_projects"
			}
		case gateCredentials:
			if !credentialsTrusted {
				reason = "needs -trust-project-providers or user trusted_provider_projects"
			}
		case gateOperator:
			reason = "operator-only"
		}
		if reason != "" {
			dropped = append(dropped, droppedSetting{l.where(nil), key, gate, reason})
			delete(l.members, key)
		}
	}
	return dropped
}

// projectRoot is the canonical workspace whose .harness files are project
// layers, or "" when the directory does not exist.
func projectRoot(workingDir string) (string, error) {
	if workingDir == "" {
		var err error
		if workingDir, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	root, err := filepath.Abs(workingDir)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return root, err
}

// listsProject reports whether a trust list names root by absolute canonical path.
func listsProject(trusted []string, root string) bool {
	for _, path := range trusted {
		if !filepath.IsAbs(path) {
			continue
		}
		if canonical, err := filepath.EvalSymlinks(path); err == nil && canonical == root {
			return true
		}
	}
	return false
}

// exclusiveSettings name pairs where setting one in a layer replaces the other
// from lower layers: a selected MCP file replaces the inline server array.
var exclusiveSettings = [][2]string{{"mcp_config", "mcp_servers"}}

func setsValue(node *settingsNode) bool { return node != nil && !node.isNull() }

func mergeStack(stack []*settingsLayer) map[string]*settingsNode {
	merged := map[string]*settingsNode{}
	for _, layer := range stack {
		for _, pair := range exclusiveSettings {
			first, second := layer.members[pair[0]], layer.members[pair[1]]
			if setsValue(first) && second == nil {
				delete(merged, pair[1])
			}
			if setsValue(second) && first == nil {
				delete(merged, pair[0])
			}
		}
		mergeMembers(merged, layer.members, nil, layer.isFile)
	}
	return merged
}

func exclusivePartner(key string) string {
	for _, pair := range exclusiveSettings {
		switch key {
		case pair[0]:
			return pair[1]
		case pair[1]:
			return pair[0]
		}
	}
	return ""
}

// checkRequirements fails when any layer above the managed defaults sets a
// pinned path to another value, naming the key, the managed file and the layer.
func checkRequirements(requirements []managedRequirement, stack []*settingsLayer, managedPath string) error {
	for _, requirement := range requirements {
		key := strings.Join(requirement.path, ".")
		for _, layer := range stack {
			if layer.name == LayerManaged {
				continue
			}
			value, exact, shadowing := memberAt(layer.members, requirement.path)
			conflictPath := requirement.path
			violated := shadowing || exact && !equalSettings(value, requirement.value)
			for depth := 1; shadowing && depth < len(requirement.path); depth++ {
				if held, found, _ := memberAt(layer.members, requirement.path[:depth]); found && held.object == nil {
					conflictPath = requirement.path[:depth]
					break
				}
			}
			if partner := exclusivePartner(key); !violated && !exact && partner != "" && setsValue(layer.members[partner]) {
				// The partner replaces the pinned key, as a selected MCP file replaces mcp_servers.
				conflictPath, value, violated = []string{partner}, layer.members[partner], true
			}
			if violated {
				return &Error{Field: key, Err: fmt.Errorf("pinned to %s by managed requirements in %s; %s sets %s to %s", redactedJSON(requirement.path, requirement.value), managedPath, layer.where(conflictPath), strings.Join(conflictPath, "."), redactedJSON(conflictPath, value))}
			}
		}
	}
	return nil
}

// builtinDefaults are the values a key takes when no layer sets it. The vendor
// ANTHROPIC_BASE_URL and OPENAI_BASE_URL variables sit here, below every
// settings layer.
func builtinDefaults(lookup func(string) string) (map[string]*settingsNode, map[string]string, error) {
	defaults := map[string]*settingsNode{
		"max_tokens":           numberNode(strconv.Itoa(adapter.DefaultMaxTokens)),
		"log_file_max_bytes":   numberNode(strconv.FormatInt(defaultLogFileMax, 10)),
		"enable_task":          boolNode(false),
		"plan":                 boolNode(false),
		"enable_web":           boolNode(false),
		"enable_lsp":           boolNode(false),
		"no_hooks":             boolNode(false),
		"retry_enabled":        boolNode(true),
		"retry_max_attempts":   numberNode(strconv.Itoa(defaultRetryMaxAttempts)),
		"retry_base_delay_ms":  numberNode(strconv.Itoa(defaultRetryBaseDelayMS)),
		"retry_backoff_cap_ms": numberNode(strconv.Itoa(defaultRetryBackoffCapMS)),
		"retry_max_delay_ms":   numberNode(strconv.Itoa(defaultRetryMaxDelayMS)),
		"retry_jitter_min":     numberNode(strconv.FormatFloat(defaultRetryJitterMin, 'g', -1, 64)),
		"retry_jitter_max":     numberNode(strconv.FormatFloat(defaultRetryJitterMax, 'g', -1, 64)),
	}
	origins := map[string]string{}
	for key, value := range map[string]string{
		"model":                   defaultModel,
		"permission_mode_default": defaultPermissionMode,
		"bash_timeout":            defaultBashTimeout,
		"log_level":               defaultLogLevel,
		"agent_dir":               defaultAgentDir(),
		"anthropic_base_url":      strings.TrimSpace(lookup("ANTHROPIC_BASE_URL")),
		"openai_base_url":         strings.TrimSpace(lookup("OPENAI_BASE_URL")),
	} {
		if value == "" {
			continue
		}
		node, err := stringNode(value)
		if err != nil {
			return nil, nil, err
		}
		defaults[key] = node
	}
	if _, ok := defaults["anthropic_base_url"]; ok {
		origins["anthropic_base_url"] = "ANTHROPIC_BASE_URL"
	}
	if _, ok := defaults["openai_base_url"]; ok {
		origins["openai_base_url"] = "OPENAI_BASE_URL"
	}
	return defaults, origins, nil
}

// layerOptions are the inputs that choose and filter layers; none of them is
// itself a setting a layer can change.
type layerOptions struct {
	managedPath    string
	userPath       string
	workingDir     string
	skipProject    bool
	profile        string
	strict         bool
	trustHooks     bool
	trustProviders bool
	flags          FlagValues
	lookup         func(string) string
}

// layeredSettings is the merged result of every settings layer.
type layeredSettings struct {
	settings               fileSettings
	sources                map[string]Setting
	list                   []Setting
	warnings               []string
	untrustedProviderFiles []string
}

// configured reports whether a layer, not the built-in default, set key.
func (s *layeredSettings) configured(key string) bool {
	source, ok := s.sources[key]
	return ok && source.Layer != LayerDefault
}

// layerSet holds every loaded layer before stacking. files run low to high:
// managed defaults, user, then project files once they are read.
type layerSet struct {
	files              []*settingsLayer
	env                *settingsLayer
	flags              *settingsLayer
	overrides          []*settingsLayer
	requirements       []managedRequirement
	invalidEnvironment map[string]error
	profile            string
}

func loadLayerSet(options layerOptions) (*layerSet, error) {
	managed, requirements, err := loadManagedSettings(options.managedPath)
	if err != nil {
		return nil, err
	}
	user, err := loadSettingsLayer(LayerUser, options.userPath, false)
	if err != nil {
		return nil, err
	}
	env, invalidEnvironment, err := environmentLayer(options.lookup)
	if err != nil {
		return nil, err
	}
	flags, err := flagLayer(options.flags)
	if err != nil {
		return nil, err
	}
	overrides, err := overrideLayers(options.flags.Overrides.Values)
	if err != nil {
		return nil, err
	}
	return &layerSet{
		files:              slices.DeleteFunc([]*settingsLayer{managed, user}, func(layer *settingsLayer) bool { return layer == nil }),
		env:                env,
		flags:              flags,
		overrides:          overrides,
		requirements:       requirements,
		invalidEnvironment: invalidEnvironment,
		profile:            options.profile,
	}, nil
}

// addProjectFiles reads the workspace's project and project-local files.
func (set *layerSet) addProjectFiles(root string) error {
	for _, project := range []struct{ name, file string }{{LayerProject, "settings.json"}, {LayerProjectLocal, "settings.local.json"}} {
		layer, err := loadSettingsLayer(project.name, filepath.Join(root, ProjectConfigDirName, project.file), true)
		if err != nil {
			return err
		}
		if layer != nil {
			set.files = append(set.files, layer)
		}
	}
	return nil
}

// stack orders the layers low to high: files, the selected profile from each
// file and each -c, then variables, flags and the -c overrides in order.
func (set *layerSet) stack() []*settingsLayer {
	stack := slices.Clone(set.files)
	for _, layer := range slices.Concat(set.files, set.overrides) {
		if profile := layer.profileLayer(set.profile); profile != nil {
			stack = append(stack, profile)
		}
	}
	stack = append(stack, set.env, set.flags)
	return append(stack, set.overrides...)
}

// resolveLayers merges, low to high: built-in defaults, managed defaults, user
// settings, project settings, project-local settings, the selected profile,
// HARNESS_* variables, flags and -c overrides, then applies managed
// requirements. Project files are read only for a local workspace.
func resolveLayers(options layerOptions) (*layeredSettings, error) {
	set, err := loadLayerSet(options)
	if err != nil {
		return nil, err
	}
	result := &layeredSettings{}
	checked := map[*settingsLayer]bool{}
	checkStack := func(stack []*settingsLayer) error {
		for _, layer := range stack {
			if checked[layer] {
				continue
			}
			checked[layer] = true
			warnings, err := layer.check(options.strict)
			if err != nil {
				return err
			}
			result.warnings = append(result.warnings, warnings...)
		}
		return nil
	}

	// Operator layers alone decide the remote environment and project trust;
	// a repository can set neither.
	stack := set.stack()
	if err := checkStack(stack); err != nil {
		return nil, err
	}
	var operator fileSettings
	if err := decodeMembers(mergeStack(stack), &operator); err != nil {
		return nil, err
	}
	if !options.skipProject && settingString(operator.Environment) == "" {
		root, err := projectRoot(options.workingDir)
		if err != nil {
			return nil, err
		}
		if root != "" {
			if err := set.addProjectFiles(root); err != nil {
				return nil, err
			}
			stack = set.stack()
			if err := checkStack(stack); err != nil {
				return nil, err
			}
			result.applyProjectGates(stack, options.trustHooks || listsProject(operator.TrustedHookProjects, root), options.trustProviders || listsProject(operator.TrustedProviderProjects, root))
		}
	}
	// A SkipProject resolution only locates storage and launch definitions; the
	// full resolution that follows reports a profile no file defines.
	if set.profile != "" && !options.skipProject && !slices.ContainsFunc(stack, func(layer *settingsLayer) bool { return layer.name == LayerProfile }) {
		return nil, &Error{Field: "profile", Err: fmt.Errorf("%q is not defined under %q in any settings file", set.profile, profilesKey)}
	}
	if err := checkRequirements(set.requirements, stack, options.managedPath); err != nil {
		return nil, err
	}
	merged, defaultOrigins, err := mergeEffective(stack, set.requirements, options.lookup)
	if err != nil {
		return nil, err
	}
	if err := decodeMembers(merged, &result.settings); err != nil {
		return nil, err
	}
	result.attribute(merged, stack, set.requirements, options.managedPath, defaultOrigins)
	for _, key := range slices.Sorted(maps.Keys(set.invalidEnvironment)) {
		switch result.sources[key].Layer {
		case LayerFlag, LayerOverride, LayerRequirement:
		default:
			return nil, &Error{Field: key, Err: set.invalidEnvironment[key]}
		}
	}
	return result, nil
}

// mergeEffective merges the stack, applies requirements, then fills built-in
// defaults for keys no layer set. It returns the defaults' origins.
func mergeEffective(stack []*settingsLayer, requirements []managedRequirement, lookup func(string) string) (map[string]*settingsNode, map[string]string, error) {
	merged := mergeStack(stack)
	for _, requirement := range requirements {
		if requirement.value.isNull() {
			deleteMember(merged, requirement.path)
			continue
		}
		setMember(merged, requirement.path, requirement.value.clone())
	}
	defaults, defaultOrigins, err := builtinDefaults(lookup)
	if err != nil {
		return nil, nil, err
	}
	for key, value := range defaults {
		if _, set := merged[key]; !set {
			merged[key] = value
		}
	}
	if _, set := merged["session_dir"]; !set {
		// The session directory follows the effective agent directory.
		var agentDir string
		if node := merged["agent_dir"]; node != nil && json.Unmarshal(node.scalar, &agentDir) != nil {
			return nil, nil, &Error{Field: "agent_dir", Err: errors.New("must be a string")}
		}
		sessionDir, err := stringNode(filepath.Join(strings.TrimSpace(agentDir), "sessions"))
		if err != nil {
			return nil, nil, err
		}
		merged["session_dir"] = sessionDir
	}
	return merged, defaultOrigins, nil
}

// applyProjectGates filters every repository layer and reports, in one
// warning, each setting it dropped and what would admit it.
func (s *layeredSettings) applyProjectGates(stack []*settingsLayer, executableTrusted, credentialsTrusted bool) {
	var dropped []droppedSetting
	for _, layer := range stack {
		if !layer.project {
			continue
		}
		layerDropped := layer.applyProjectGates(executableTrusted, credentialsTrusted)
		for _, setting := range layerDropped {
			if setting.gate == gateCredentials && !slices.Contains(s.untrustedProviderFiles, layer.file) {
				s.untrustedProviderFiles = append(s.untrustedProviderFiles, layer.file)
			}
		}
		dropped = append(dropped, layerDropped...)
	}
	if len(dropped) == 0 {
		return
	}
	parts := make([]string, 0, len(dropped))
	for _, setting := range dropped {
		parts = append(parts, fmt.Sprintf("%s: %s (%s)", setting.file, setting.key, setting.reason))
	}
	s.warnings = append(s.warnings, "project settings ignored: "+strings.Join(parts, "; "))
}

func decodeMembers(members map[string]*settingsNode, target *fileSettings) error {
	encoded, err := json.Marshal(members)
	if err != nil {
		return &Error{Field: "settings", Err: err}
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return &Error{Field: "settings", Err: err}
	}
	return nil
}

// attribute records, for every effective leaf, its value and source: the
// highest layer that touches the path decides. A value there is the last
// write; a null, or a non-object above the path, means a later default or
// requirement filled it.
func (s *layeredSettings) attribute(merged map[string]*settingsNode, stack []*settingsLayer, requirements []managedRequirement, managedPath string, defaultOrigins map[string]string) {
	pinned := map[string]bool{}
	for _, requirement := range requirements {
		pinned[strings.Join(requirement.path, ".")] = true
	}
	s.sources = map[string]Setting{}
	visitLeaves(merged, nil, func(path []string, value *settingsNode) {
		key := strings.Join(path, ".")
		setting := Setting{Key: key, Value: redactedJSON(path, value), Layer: LayerDefault, Source: defaultOrigins[key]}
		if pinned[key] {
			setting.Layer, setting.Source = LayerRequirement, managedPath
		} else {
			for i := len(stack) - 1; i >= 0; i-- {
				held, exact, shadowing := memberAt(stack[i].members, path)
				if exact && !held.isNull() {
					setting.Layer, setting.Source = stack[i].name, stack[i].origin(path)
				}
				if exact || shadowing {
					break
				}
			}
		}
		s.sources[key] = setting
		s.list = append(s.list, setting)
	})
}

// secretSegment matches path segments that name a credential; *_env members
// name a variable, not its value.
var secretSegment = regexp.MustCompile(`(?i)^(key|.*(api[_-]?key|apikey|secret|password|passwd|authorization|credential|cookie).*|(.*[_-])?token)$`)

// secretContainers hold credentials under arbitrary names: request headers,
// query parameters and child-process environments. They count from depth two,
// inside a named entry, so a provider merely named "env" is not masked.
var secretContainers = map[string]bool{"headers": true, "query": true, "env": true}

// arrayItemSegment stands for an array index while redaction walks a value, so
// containers inside array items sit at the same depth as in named entries.
const arrayItemSegment = "[]"

func isSecretPath(path []string) bool {
	for depth, segment := range path {
		if secretSegment.MatchString(segment) && !strings.HasSuffix(strings.ToLower(segment), "_env") {
			return true
		}
		if depth >= 2 && secretContainers[segment] {
			return true
		}
	}
	return false
}

const redactedValue = `"[REDACTED]"`

// redactedJSON renders a value for display. A value under a credential path is
// masked; every other string passes through the log redactor, which catches
// URL userinfo, bearer tokens and sk- keys.
func redactedJSON(path []string, value *settingsNode) json.RawMessage {
	if value == nil {
		return jsonNull
	}
	encoded, err := json.Marshal(redactNode(slices.Clone(path), value.clone()))
	if err != nil {
		return json.RawMessage(redactedValue)
	}
	return encoded
}

// redactNode masks node in place, which must be a private copy.
func redactNode(path []string, node *settingsNode) *settingsNode {
	switch {
	case node.object != nil:
		for key, child := range node.object {
			node.object[key] = redactNode(append(slices.Clip(path), key), child)
		}
		return node
	case node.isArray:
		for i, item := range node.array {
			node.array[i] = redactNode(append(slices.Clip(path), arrayItemSegment), item)
		}
		return node
	case node.isNull():
		return node
	case isSecretPath(path):
		return &settingsNode{scalar: json.RawMessage(redactedValue)}
	}
	var text string
	if json.Unmarshal(node.scalar, &text) != nil {
		return node
	}
	masked := obs.Redact(text)
	if masked == text {
		return node
	}
	redacted, err := stringNode(masked)
	if err != nil {
		// Fail closed: a value that cannot be re-encoded is shown masked.
		return &settingsNode{scalar: json.RawMessage(redactedValue)}
	}
	return redacted
}
