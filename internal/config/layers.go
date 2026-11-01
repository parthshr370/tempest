package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
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
// file, the HARNESS_* variables, the flags, or the -c overrides. Its members
// hold only names the settings schema declares; prepare removes the rest.
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
	isFile  bool
	members map[string]*settingsNode
	// unknown lists the members prepare removed, as dotted paths.
	unknown []string
	// selectedProfile is this layer's definition of the selected profile.
	// profileUnset marks a null definition or a null profiles member, which
	// unsets the profile's definitions in lower layers.
	selectedProfile *settingsLayer
	profileUnset    bool
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

// takeProfile moves the profiles member out of the layer and returns the
// definition of name: nil when the layer has none, a null node when the layer
// unsets it.
func (l *settingsLayer) takeProfile(name string) (*settingsNode, error) {
	definitions, ok := l.members[profilesKey]
	if !ok {
		return nil, nil
	}
	delete(l.members, profilesKey)
	if definitions.isNull() {
		if name == "" {
			return nil, nil
		}
		return definitions, nil
	}
	if definitions.object == nil {
		return nil, &Error{Field: profilesKey, Err: fmt.Errorf("%s: must be an object of settings objects", l.where(nil))}
	}
	for profile, definition := range definitions.object {
		if definition.object == nil && !definition.isNull() {
			return nil, &Error{Field: profilesKey + "." + profile, Err: fmt.Errorf("%s: a profile must be a settings object", l.where(nil))}
		}
	}
	if name == "" {
		return nil, nil
	}
	return definitions.object[name], nil
}

// check reports the unknown members prepare removed (a warning, or an error
// in strict mode) and rejects values of the wrong type, naming the layer and
// its file.
func (l *settingsLayer) check(strict bool) ([]string, error) {
	if strict && len(l.unknown) > 0 {
		return nil, &Error{Field: l.unknown[0], Err: fmt.Errorf("unknown setting in %s; remove it or run without -strict-config", l.where(nil))}
	}
	warnings := make([]string, 0, len(l.unknown))
	for _, path := range l.unknown {
		warnings = append(warnings, fmt.Sprintf("unknown setting %q in %s is ignored", path, l.where(nil)))
	}
	var typed fileSettings
	if err := decodeSettings(l.members, &typed); err != nil {
		return nil, &Error{Field: "settings", Err: fmt.Errorf("%s: %w", l.where(nil), err)}
	}
	return warnings, nil
}

// maxSettingsFileBytes bounds one settings file; real ones are kilobytes.
const maxSettingsFileBytes = 4 << 20

// readSettingsFile reads a regular file of bounded size. found is false for
// a missing file. Checking the kind before opening keeps a FIFO or device
// node in a repository from blocking or flooding startup.
func readSettingsFile(path string) (contents []byte, found bool, err error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	if !info.Mode().IsRegular() {
		return nil, true, errors.New("not a regular file")
	}
	if info.Size() > maxSettingsFileBytes {
		return nil, true, fmt.Errorf("larger than %d bytes", maxSettingsFileBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, true, err
	}
	defer file.Close()
	contents, err = io.ReadAll(io.LimitReader(file, maxSettingsFileBytes+1))
	if err == nil && len(contents) > maxSettingsFileBytes {
		err = fmt.Errorf("larger than %d bytes", maxSettingsFileBytes)
	}
	return contents, true, err
}

// loadSettingsLayer reads one settings file and selects profile from it; a
// missing file is no layer.
func loadSettingsLayer(name, path string, project bool, profile string) (*settingsLayer, error) {
	contents, found, err := readSettingsFile(path)
	if !found {
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
	if err := layer.prepare(profile); err != nil {
		return nil, err
	}
	return layer, nil
}

// prepare moves profiles out of the layer, keeps the definition of the
// selected profile as its own layer, and in both removes the members the
// schema does not declare and normalizes aliases. Every layer built from
// free-form input goes through here before any gate or check reads it.
func (l *settingsLayer) prepare(profile string) error {
	definition, err := l.takeProfile(profile)
	if err != nil {
		return err
	}
	l.unknown = pruneUnknownSettings(l.members)
	normalizeAliases(l.members)
	switch {
	case definition == nil:
	case definition.isNull():
		l.profileUnset = true
	default:
		selected := &settingsLayer{name: LayerProfile, file: l.file, profile: profile, project: l.project, isFile: l.isFile, members: definition.object}
		selected.unknown = pruneUnknownSettings(selected.members)
		normalizeAliases(selected.members)
		l.selectedProfile = selected
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
func loadManagedSettings(path, profile string) (*settingsLayer, []managedRequirement, error) {
	contents, found, err := readSettingsFile(path)
	if !found {
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
			if err := defaults.prepare(profile); err != nil {
				return nil, nil, err
			}
			continue
		}
		pinned := &settingsLayer{name: LayerRequirement, file: path, members: value.object}
		if _, ok := pinned.members[profilesKey]; ok {
			return nil, nil, &Error{Field: "managed settings", Err: fmt.Errorf("%s: requirements cannot pin profiles", path)}
		}
		pinned.unknown = pruneUnknownSettings(pinned.members)
		normalizeAliases(pinned.members)
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
// layer stack below -c and show up in `config show`. A dotted key sets one
// member of an object setting, so the input merges with the object's other
// members from lower layers.
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
	{key: "fallback_models.default", env: "HARNESS_FALLBACK_MODELS", flag: "fallback-model", kind: inputList, field: func(f *FlagValues) flagInput { return &f.FallbackModels }},
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
	{key: "workspace_listing", env: "HARNESS_WORKSPACE_LISTING", kind: inputBool},
	{key: "workspace_listing_max_files", env: "HARNESS_WORKSPACE_LISTING_MAX_FILES", kind: inputInt},
	{key: "workspace_listing_max_bytes", env: "HARNESS_WORKSPACE_LISTING_MAX_BYTES", kind: inputInt},
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
		setMember(layer.members, strings.Split(input.key, "."), node)
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
			setMember(layer.members, strings.Split(input.key, "."), node)
			layer.origins[input.key] = "-" + input.flag
		}
	}
	return layer, nil
}

// overrideLayers parses repeated -c key.path=value overrides, one layer each
// in command-line order, so a later override applies on top of an earlier one
// exactly as a higher layer would. The value is JSON when it parses as JSON
// and a string otherwise.
func overrideLayers(overrides []string, profile string) ([]*settingsLayer, error) {
	layers := make([]*settingsLayer, 0, len(overrides))
	for index, override := range overrides {
		keyPath, rawValue, ok := strings.Cut(override, "=")
		path := strings.Split(strings.TrimSpace(keyPath), ".")
		if !ok || slices.Contains(path, "") {
			// The argument is not echoed: a mistyped override may hold a credential.
			return nil, &Error{Field: overrideSource, Err: fmt.Errorf("override %d of %d is malformed; want key.path=value", index+1, len(overrides))}
		}
		value, err := parseSettingsNode([]byte(rawValue))
		if err != nil {
			if value, err = stringNode(rawValue); err != nil {
				return nil, &Error{Field: overrideSource, Err: err}
			}
		}
		layer := &settingsLayer{name: LayerOverride, file: overrideSource, members: map[string]*settingsNode{}}
		setMember(layer.members, path, value)
		if err := layer.prepare(profile); err != nil {
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
	// context, write outside the repository, widen permissions, open a
	// network channel the model can send repository contents through, or
	// raise a resource cap above the operator's value.
	gateExecutable
	// gateCredentials settings choose where credentials are sent.
	gateCredentials
	// gateOperator settings only the operator may set, trusted or not.
	gateOperator
)

// projectGates holds what a repository setting needs. For a key
// narrowsPermissions checks by value, it is what a loosening value needs.
var projectGates = map[string]trustGate{
	"hooks":                       gateExecutable,
	"mcp_config":                  gateExecutable,
	"mcp_servers":                 gateExecutable,
	"lsp_config":                  gateExecutable,
	"enable_lsp":                  gateExecutable,
	"debug_adapters":              gateExecutable,
	"cloud_policy":                gateExecutable,
	"permission_mode_default":     gateExecutable,
	"permission_allow_tools":      gateExecutable,
	"permission_deny_tools":       gateExecutable,
	"plan":                        gateExecutable,
	"enable_web":                  gateExecutable,
	"attach":                      gateExecutable,
	"skill_paths":                 gateExecutable,
	"agent_dir":                   gateExecutable,
	"session_dir":                 gateExecutable,
	"log_file":                    gateExecutable,
	"faux_script":                 gateExecutable,
	"providers":                   gateCredentials,
	"anthropic_base_url":          gateCredentials,
	"openai_base_url":             gateCredentials,
	"web_search_url":              gateCredentials,
	"no_hooks":                    gateOperator,
	"trusted_hook_projects":       gateOperator,
	"trusted_provider_projects":   gateOperator,
	"environment":                 gateOperator,
	"environments":                gateOperator,
	"workspace_listing":           gateExecutable,
	"workspace_listing_max_files": gateExecutable,
	"workspace_listing_max_bytes": gateExecutable,
}

// projectGate classifies one repository setting. A model with an @http(s)
// endpoint suffix sends the route's credential there, so it needs provider
// trust; so does a backup chain naming one. operator is the operator's own
// settings, nil for none.
func projectGate(key string, value *settingsNode, operator *fileSettings) trustGate {
	switch key {
	case "model", "plan_model", "smol_model", "fallback_models":
		if namesModelEndpoint(value) {
			return gateCredentials
		}
		return gateNone
	}
	if narrowsPermissions(key, value, operator) {
		return gateNone
	}
	return projectGates[key]
}

// namesModelEndpoint reports whether any model spec in value, a spec or an
// object or array of them, carries an @http(s) endpoint suffix.
func namesModelEndpoint(value *settingsNode) bool {
	if value == nil {
		return false
	}
	for _, member := range value.object {
		if namesModelEndpoint(member) {
			return true
		}
	}
	if slices.ContainsFunc(value.array, namesModelEndpoint) {
		return true
	}
	var spec string
	return json.Unmarshal(value.scalar, &spec) == nil && (strings.Contains(spec, "@http://") || strings.Contains(spec, "@https://"))
}

// planModeNode is the strictest permission mode, which no lower mode is
// stricter than.
var planModeNode = &settingsNode{scalar: json.RawMessage(`"plan"`)}

// narrowsPermissions reports whether a repository value can only tighten the
// permissions, capabilities and resource caps the layers below it set, so it
// applies without trust: plan mode, web tools off, a deny list, which an
// untrusted repository adds to the lists below it (unionProjectDenies), and
// the session-start listing off or capped at most at the operator's values.
// Every other value of these keys, null included, could lift an operator's
// restriction.
func narrowsPermissions(key string, value *settingsNode, operator *fileSettings) bool {
	if operator == nil {
		operator = &fileSettings{}
	}
	switch key {
	case "plan":
		return equalSettings(value, boolNode(true))
	case "enable_web":
		return equalSettings(value, boolNode(false))
	case "permission_mode_default":
		return equalSettings(value, planModeNode)
	case "permission_deny_tools":
		return value.isArray
	case "workspace_listing":
		operatorLists := operator.WorkspaceListing == nil || *operator.WorkspaceListing
		return equalSettings(value, boolNode(false)) || operatorLists && equalSettings(value, boolNode(true))
	case "workspace_listing_max_files":
		return withinCap(value, operator.ListingMaxFiles, DefaultWorkspaceListingMaxFiles)
	case "workspace_listing_max_bytes":
		return withinCap(value, operator.ListingMaxBytes, DefaultWorkspaceListingMaxBytes)
	}
	return false
}

// withinCap reports whether value is a whole number from 1 to the operator's
// cap, or to builtin when the operator set none.
func withinCap(value *settingsNode, operator *int, builtin int) bool {
	var limit int
	if json.Unmarshal(value.scalar, &limit) != nil || limit < 1 {
		return false
	}
	ceiling := builtin
	if operator != nil {
		ceiling = *operator
	}
	return limit <= ceiling
}

// profileGate classifies a setting of a profile as a repository would change
// it. Hooks are operator-only there: a profile replaces hook arrays, which
// would remove the operator's hooks, while a project file appends to them.
func profileGate(key string, value *settingsNode, operator *fileSettings) trustGate {
	if key == "hooks" {
		return gateOperator
	}
	return projectGate(key, value, operator)
}

// projectTrust is what the operator trusts a workspace's settings to do.
// operator is the operator's own settings, the ceiling for caps a repository
// may only lower; nil means the built-in values.
type projectTrust struct {
	executable  bool
	credentials bool
	operator    *fileSettings
}

func (t projectTrust) any() bool { return t.executable || t.credentials }

// refusal says why a repository setting under gate does not apply, or "".
func (t projectTrust) refusal(gate trustGate) string {
	switch gate {
	case gateExecutable:
		if !t.executable {
			return "needs -trust-project-hooks or user trusted_hook_projects"
		}
	case gateCredentials:
		if !t.credentials {
			return "needs -trust-project-providers or user trusted_provider_projects"
		}
	case gateOperator:
		return "operator-only"
	}
	return ""
}

// droppedSetting records a repository setting the trust rules removed or, for
// a null profile definition, could not remove.
type droppedSetting struct {
	layer  *settingsLayer
	key    string
	gate   trustGate
	reason string
}

// hookUnsetReason explains why a repository's null hooks are ignored.
const hookUnsetReason = "a repository adds hooks and cannot remove them"

// applyProjectGates removes repository settings the operator has not trusted.
// Admitted top-level hooks lose their nulls, so the project adds hooks
// (appendProjectHooks) and never unsets the operator's.
func (l *settingsLayer) applyProjectGates(trust projectTrust) []droppedSetting {
	classify := projectGate
	if l.name == LayerProfile {
		classify = profileGate
	}
	var dropped []droppedSetting
	for _, key := range slices.Sorted(maps.Keys(l.members)) {
		gate := classify(key, l.members[key], trust.operator)
		if reason := trust.refusal(gate); reason != "" {
			dropped = append(dropped, droppedSetting{l, key, gate, reason})
			delete(l.members, key)
		}
	}
	if hooks := l.members["hooks"]; hooks != nil {
		if hooks.isNull() {
			dropped = append(dropped, droppedSetting{l, "hooks", gateOperator, hookUnsetReason})
			delete(l.members, "hooks")
		}
		for _, event := range slices.Sorted(maps.Keys(hooks.object)) {
			if hooks.object[event].isNull() {
				dropped = append(dropped, droppedSetting{l, "hooks." + event, gateOperator, hookUnsetReason})
				delete(hooks.object, event)
			}
		}
	}
	return dropped
}

// appendProjectHooks makes each repository file's hook arrays extend the
// arrays the layers below it merge to, so a trusted project adds to the
// operator's hooks per event. Layers above still replace or unset them, and
// managed requirements see the combined arrays.
func appendProjectHooks(stack []*settingsLayer) {
	for index, layer := range stack {
		hooks := layer.members["hooks"]
		if !layer.project || layer.name == LayerProfile || hooks == nil || hooks.object == nil {
			continue
		}
		below := mergeStack(stack[:index])["hooks"]
		if below == nil || below.object == nil {
			continue
		}
		for event, groups := range hooks.object {
			if lower := below.object[event]; groups.isArray && lower != nil && lower.isArray {
				groups.array = append(lower.clone().array, groups.array...)
			}
		}
	}
}

// unionProjectDenies makes each deny list of an untrusted repository, from a
// project file or its profile, add to the list the layers below it merge to:
// the repository denies more tools and cannot remove an operator's entry. A
// list that adds no entry is removed, so config show keeps naming the layer
// that set the entries. Layers above still replace or unset the list, and
// managed requirements see the combined list.
func unionProjectDenies(stack []*settingsLayer) {
	const key = "permission_deny_tools"
	for index, layer := range stack {
		deny := layer.members[key]
		if !layer.project || deny == nil {
			continue
		}
		below := mergeStack(stack[:index])[key]
		if below == nil || !below.isArray {
			continue
		}
		union := below.clone()
		for _, entry := range deny.array {
			if !slices.ContainsFunc(union.array, func(held *settingsNode) bool { return equalSettings(held, entry) }) {
				union.array = append(union.array, entry)
			}
		}
		if len(union.array) == len(below.array) {
			delete(layer.members, key)
			continue
		}
		layer.members[key] = union
	}
}

// keepUnremovable is what a repository's null profile definition leaves of
// the lower definitions: from operator files, the keys the repository could
// not have set, or set to null, itself under trust.
func keepUnremovable(profiles []*settingsLayer, unset *settingsLayer, trust projectTrust) ([]*settingsLayer, []droppedSetting) {
	null := &settingsNode{scalar: jsonNull}
	var kept []*settingsLayer
	var dropped []droppedSetting
	for _, profile := range profiles {
		if profile.project {
			continue
		}
		remaining := &settingsLayer{name: profile.name, file: profile.file, profile: profile.profile, isFile: profile.isFile, members: map[string]*settingsNode{}}
		for _, key := range slices.Sorted(maps.Keys(profile.members)) {
			value := profile.members[key]
			gate := profileGate(key, value, trust.operator)
			if trust.refusal(gate) == "" {
				gate = profileGate(key, null, trust.operator)
			}
			if reason := trust.refusal(gate); reason != "" {
				remaining.members[key] = value
				dropped = append(dropped, droppedSetting{unset, fmt.Sprintf("null profile %q kept %s from %s", profile.profile, key, profile.file), gate, reason})
			}
		}
		if len(remaining.members) > 0 {
			kept = append(kept, remaining)
		}
	}
	return kept, dropped
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
		"max_tokens":                  numberNode(strconv.Itoa(adapter.DefaultMaxTokens)),
		"log_file_max_bytes":          numberNode(strconv.FormatInt(defaultLogFileMax, 10)),
		"enable_task":                 boolNode(false),
		"plan":                        boolNode(false),
		"enable_web":                  boolNode(false),
		"enable_lsp":                  boolNode(false),
		"no_hooks":                    boolNode(false),
		"retry_enabled":               boolNode(true),
		"retry_max_attempts":          numberNode(strconv.Itoa(defaultRetryMaxAttempts)),
		"retry_base_delay_ms":         numberNode(strconv.Itoa(defaultRetryBaseDelayMS)),
		"retry_backoff_cap_ms":        numberNode(strconv.Itoa(defaultRetryBackoffCapMS)),
		"retry_max_delay_ms":          numberNode(strconv.Itoa(defaultRetryMaxDelayMS)),
		"retry_jitter_min":            numberNode(strconv.FormatFloat(defaultRetryJitterMin, 'g', -1, 64)),
		"retry_jitter_max":            numberNode(strconv.FormatFloat(defaultRetryJitterMax, 'g', -1, 64)),
		"workspace_listing":           boolNode(true),
		"workspace_listing_max_files": numberNode(strconv.Itoa(DefaultWorkspaceListingMaxFiles)),
		"workspace_listing_max_bytes": numberNode(strconv.Itoa(DefaultWorkspaceListingMaxBytes)),
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
	settings fileSettings
	sources  map[string]Setting
	list     []Setting
	// warnings are about the workspace's project files only; the resolver
	// holds the operator layers' own.
	warnings               []string
	untrustedProviderFiles []string
}

// configured reports whether a layer, not the built-in default, set key.
func (s *layeredSettings) configured(key string) bool {
	source, ok := s.sources[key]
	return ok && source.Layer != LayerDefault
}

// layerSet holds the operator's layers: files (managed defaults, then user),
// variables, flags and -c overrides.
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
	managed, requirements, err := loadManagedSettings(options.managedPath, options.profile)
	if err != nil {
		return nil, err
	}
	user, err := loadSettingsLayer(LayerUser, options.userPath, false, options.profile)
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
	overrides, err := overrideLayers(options.flags.Overrides.Values, options.profile)
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

// stack orders the layers low to high: operator files, then project files,
// the selected profile's definitions in that file order with -c last, then
// variables, flags and the -c overrides in order. A null definition drops
// the definitions below it; from a repository file it keeps what the
// repository could not remove, which the returned settings report.
func (set *layerSet) stack(project []*settingsLayer, trust projectTrust) ([]*settingsLayer, []droppedSetting) {
	files := slices.Concat(set.files, project)
	var profiles []*settingsLayer
	var kept []droppedSetting
	for _, layer := range slices.Concat(files, set.overrides) {
		if layer.profileUnset {
			if !layer.project {
				profiles = nil
				continue
			}
			var unremovable []droppedSetting
			profiles, unremovable = keepUnremovable(profiles, layer, trust)
			kept = append(kept, unremovable...)
		}
		if layer.selectedProfile != nil {
			profiles = append(profiles, layer.selectedProfile)
		}
	}
	return slices.Concat(files, profiles, []*settingsLayer{set.env, set.flags}, set.overrides), kept
}

// layerResolver resolves the settings of any workspace on top of the
// operator's layers, which it loads, checks and decodes once. It is read-only
// after construction, so sessions in different workspaces share it.
type layerResolver struct {
	set     *layerSet
	options layerOptions
	// operator is the operator layers alone. They decide the remote
	// environment and project trust; a repository can set neither.
	operator         fileSettings
	operatorWarnings []string
}

func newLayerResolver(options layerOptions) (*layerResolver, error) {
	set, err := loadLayerSet(options)
	if err != nil {
		return nil, err
	}
	// The flags now live in set; drop the credentials among them.
	options.flags = FlagValues{}
	resolver := &layerResolver{set: set, options: options}
	stack, _ := set.stack(nil, projectTrust{})
	for _, layer := range stack {
		warnings, err := layer.check(options.strict)
		if err != nil {
			return nil, err
		}
		resolver.operatorWarnings = append(resolver.operatorWarnings, warnings...)
	}
	if err := decodeMembers(mergeStack(stack), &resolver.operator); err != nil {
		return nil, err
	}
	return resolver, nil
}

// workspaceRoot is the canonical workspace whose project files apply for a
// session in workingDir, or "" when none do: no such directory, a remote
// environment, or SkipProject.
func (r *layerResolver) workspaceRoot(workingDir string) (string, error) {
	if r.options.skipProject || settingString(r.operator.Environment) != "" {
		return "", nil
	}
	return projectRoot(workingDir)
}

// projectTrust is what the operator trusts the workspace at root to do.
func (r *layerResolver) projectTrust(root string) projectTrust {
	return projectTrust{
		executable:  r.options.trustHooks || listsProject(r.operator.TrustedHookProjects, root),
		credentials: r.options.trustProviders || listsProject(r.operator.TrustedProviderProjects, root),
		operator:    &r.operator,
	}
}

// loadProjectLayers reads the project and project-local files of root and
// applies the trust gates to each and to its selected profile. A file that
// cannot be read or decoded, or holds a wrong type, is skipped with a
// warning when the workspace is untrusted: a repository must not stop a run
// it may not configure. Strict mode and a trusted workspace make it an error.
func (r *layerResolver) loadProjectLayers(root string, trust projectTrust) ([]*settingsLayer, []droppedSetting, []string, error) {
	var layers []*settingsLayer
	var dropped []droppedSetting
	var warnings []string
	for _, project := range []struct{ name, file string }{{LayerProject, "settings.json"}, {LayerProjectLocal, "settings.local.json"}} {
		path := filepath.Join(root, ProjectConfigDirName, project.file)
		layer, layerDropped, layerWarnings, err := r.loadProjectLayer(project.name, path, trust)
		if err != nil {
			if r.options.strict || trust.any() {
				return nil, nil, nil, err
			}
			warnings = append(warnings, fmt.Sprintf("%v; the untrusted project file is skipped", err))
			continue
		}
		if layer != nil {
			layers = append(layers, layer)
			dropped = append(dropped, layerDropped...)
			warnings = append(warnings, layerWarnings...)
		}
	}
	return layers, dropped, warnings, nil
}

// loadProjectLayer gates before checking types, so a value the trust rules
// drop cannot make the file invalid.
func (r *layerResolver) loadProjectLayer(name, path string, trust projectTrust) (*settingsLayer, []droppedSetting, []string, error) {
	layer, err := loadSettingsLayer(name, path, true, r.set.profile)
	if err != nil || layer == nil {
		return nil, nil, nil, err
	}
	var dropped []droppedSetting
	var warnings []string
	for _, gated := range []*settingsLayer{layer, layer.selectedProfile} {
		if gated == nil {
			continue
		}
		dropped = append(dropped, gated.applyProjectGates(trust)...)
		layerWarnings, err := gated.check(r.options.strict)
		if err != nil {
			return nil, nil, nil, err
		}
		warnings = append(warnings, layerWarnings...)
	}
	return layer, dropped, warnings, nil
}

// resolve merges, low to high: built-in defaults, managed defaults, user
// settings, the project and project-local settings of root (none when root
// is ""), the selected profile, HARNESS_* variables, flags and -c overrides,
// then applies managed requirements.
func (r *layerResolver) resolve(root string) (*layeredSettings, error) {
	result := &layeredSettings{}
	var project []*settingsLayer
	var dropped []droppedSetting
	var trust projectTrust
	if root != "" {
		trust = r.projectTrust(root)
		var err error
		if project, dropped, result.warnings, err = r.loadProjectLayers(root, trust); err != nil {
			return nil, err
		}
	}
	stack, kept := r.set.stack(project, trust)
	result.reportDropped(dropped, kept)
	appendProjectHooks(stack)
	if !trust.executable {
		unionProjectDenies(stack)
	}
	// A SkipProject resolution only locates storage and launch definitions; the
	// full resolution that follows reports a profile no file defines.
	if r.set.profile != "" && !r.options.skipProject && !slices.ContainsFunc(stack, func(layer *settingsLayer) bool { return layer.name == LayerProfile }) {
		return nil, &Error{Field: "profile", Err: fmt.Errorf("%q is not defined under %q in any settings file, or a higher file unsets it with null", r.set.profile, profilesKey)}
	}
	if err := checkRequirements(r.set.requirements, stack, r.options.managedPath); err != nil {
		return nil, err
	}
	merged, defaultOrigins, err := mergeEffective(stack, r.set.requirements, r.options.lookup)
	if err != nil {
		return nil, err
	}
	if err := decodeMembers(merged, &result.settings); err != nil {
		return nil, err
	}
	result.attribute(merged, stack, r.set.requirements, r.options.managedPath, defaultOrigins)
	for _, key := range slices.Sorted(maps.Keys(r.set.invalidEnvironment)) {
		switch result.sources[key].Layer {
		case LayerFlag, LayerOverride, LayerRequirement:
		default:
			return nil, &Error{Field: key, Err: r.set.invalidEnvironment[key]}
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

// reportDropped names, in one warning, each repository setting the trust
// rules dropped or a repository's null profile could not remove, and what
// would admit it.
func (s *layeredSettings) reportDropped(dropped, kept []droppedSetting) {
	for _, setting := range dropped {
		if setting.gate == gateCredentials && !slices.Contains(s.untrustedProviderFiles, setting.layer.file) {
			s.untrustedProviderFiles = append(s.untrustedProviderFiles, setting.layer.file)
		}
	}
	all := slices.Concat(dropped, kept)
	if len(all) == 0 {
		return
	}
	parts := make([]string, 0, len(all))
	for _, setting := range all {
		parts = append(parts, fmt.Sprintf("%s: %s (%s)", setting.layer.where(nil), setting.key, setting.reason))
	}
	s.warnings = append(s.warnings, "project settings ignored: "+strings.Join(parts, "; "))
}

// decodeMembers decodes merged settings into target.
func decodeMembers(members map[string]*settingsNode, target *fileSettings) error {
	if err := decodeSettings(members, target); err != nil {
		return &Error{Field: "settings", Err: err}
	}
	return nil
}

// decodeSettings is the only way settings members become typed settings.
// It decodes a copy with every member the schema does not declare exactly
// removed, so encoding/json's case-insensitive member matching can never
// apply a spelling that trust gates and requirement checks did not see.
func decodeSettings(members map[string]*settingsNode, target *fileSettings) error {
	declared := (&settingsNode{object: members}).clone().object
	pruneUnknownSettings(declared)
	encoded, err := json.Marshal(declared)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
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
// inside a named entry, so a provider merely named "env" is not masked. The
// match folds case as encoding/json does, so the redactor fails closed for
// any spelling that reaches it, whether or not the schema would keep it.
var secretContainers = []string{"headers", "query", "env"}

// arrayItemSegment stands for an array index while redaction walks a value, so
// containers inside array items sit at the same depth as in named entries.
const arrayItemSegment = "[]"

func isSecretPath(path []string) bool {
	for depth, segment := range path {
		if secretSegment.MatchString(segment) && !strings.HasSuffix(strings.ToLower(segment), "_env") {
			return true
		}
		if depth >= 2 && slices.ContainsFunc(secretContainers, func(container string) bool { return strings.EqualFold(segment, container) }) {
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
