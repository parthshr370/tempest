// Package config holds the product layout constants and resolves one immutable
// runtime configuration from flags, environment variables, and settings files.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.harness.dev/harness/internal/adapter"
	"go.harness.dev/harness/internal/cloudcmd"
	"go.harness.dev/harness/internal/dap"
	"go.harness.dev/harness/internal/hooks"
	"go.harness.dev/harness/internal/lsp"
	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/internal/obs"
)

const (
	// PackageName is the Go module path of the harness.
	PackageName = "go.harness.dev/harness"
	// AppName is the lowercase application identifier.
	AppName = "harness"
	// AppTitle is the human-readable application name.
	AppTitle = "Harness"
	// ConfigDirName is the per-user config directory name.
	ConfigDirName = "harness"
	// ProjectConfigDirName is the native per-project configuration directory.
	ProjectConfigDirName = ".harness"

	// EnvAgentDir overrides the resolved agent directory.
	EnvAgentDir = "HARNESS_AGENT_DIR"
	// EnvSessionDir overrides the resolved session directory.
	EnvSessionDir = "HARNESS_SESSION_DIR"
)

const (
	defaultModel             = "claude-opus-4-8"
	defaultPermissionMode    = "default"
	defaultLogLevel          = "info"
	defaultLogFileMax        = 100 << 20
	defaultBashTimeout       = "10m"
	defaultRetryMaxAttempts  = 11
	defaultRetryBaseDelayMS  = 500
	defaultRetryBackoffCapMS = 8000
	defaultRetryMaxDelayMS   = 300000
	defaultRetryJitterMin    = 0.75
	defaultRetryJitterMax    = 1.0
)

// StringInput retains both a flag value and whether the caller set it.
type StringInput struct {
	Value string
	Set   bool
}

// StringsInput retains repeatable flag values and whether the caller set them.
type StringsInput struct {
	Values []string
	Set    bool
}

// IntInput retains both an integer flag value and whether the caller set it.
type IntInput struct {
	Value int
	Set   bool
}

// BoolInput retains both a boolean flag value and whether the caller set it.
type BoolInput struct {
	Value bool
	Set   bool
}

// FlagValues is the typed collection of configuration-bearing command flags.
type FlagValues struct {
	Model                 StringInput
	PlanModel             StringInput
	SmolModel             StringInput
	FallbackModels        StringsInput
	Reasoning             StringInput
	PlanReasoning         StringInput
	SmolReasoning         StringInput
	MaxTokens             IntInput
	PermissionMode        StringInput
	BashTimeout           StringInput
	LogLevel              StringInput
	LogFile               StringInput
	LogFileMaxBytes       IntInput
	EnableTask            BoolInput
	Plan                  BoolInput
	Attach                StringsInput
	EnableWeb             BoolInput
	WebSearchURL          StringInput
	AgentDir              StringInput
	SessionDir            StringInput
	AnthropicBaseURL      StringInput
	OpenAIBaseURL         StringInput
	AnthropicAPIKey       StringInput
	AnthropicAuthToken    StringInput
	FauxScript            StringInput
	SkillPaths            StringsInput
	PermissionAllowTools  StringsInput
	PermissionDenyTools   StringsInput
	MCPConfig             StringInput
	EnableLSP             BoolInput
	LSPConfig             StringInput
	CloudPolicy           StringInput
	NoHooks               BoolInput
	TrustProjectHooks     BoolInput
	TrustProjectProviders BoolInput
	Environment           StringInput
	EnvironmentCommand    StringInput
	// Profile selects profiles.<name> from the settings files.
	Profile StringInput
	// StrictConfig makes unknown settings errors instead of warnings.
	StrictConfig BoolInput
	// Overrides are -c key.path=value settings, the highest layer.
	Overrides StringsInput
}

// RetrySettings is the immutable numeric retry policy resolved at startup.
type RetrySettings struct {
	Enabled      bool
	MaxAttempts  int
	BaseDelayMS  int
	BackoffCapMS int
	MaxDelayMS   int
	JitterMin    float64
	JitterMax    float64
}

// SecretResolver supplies provider credentials at process startup. Resolved
// configuration retains neither this resolver nor the values it returns.
type SecretResolver interface {
	Resolve(name string) (value string, present bool)
}

// SecretResolverFunc adapts a function to [SecretResolver].
type SecretResolverFunc func(name string) (value string, present bool)

// Resolve implements [SecretResolver].
func (f SecretResolverFunc) Resolve(name string) (string, bool) { return f(name) }

// Config is the raw configuration boundary. Settings layer, low to high:
// built-in defaults, managed defaults, user settings.json, project
// .harness/settings.json, project .harness/settings.local.json, the selected
// profile, HARNESS_* variables, flags, and -c overrides. Managed requirements
// pin keys above all of them.
type Config struct {
	Flags        FlagValues
	LookupEnv    func(string) string
	SettingsPath string
	// ManagedSettingsPath overrides the administrator's managed settings file,
	// for tests and embedders; empty selects the per-OS path.
	ManagedSettingsPath string
	WorkingDir          string
	// SkipProject prevents host-path discovery for a remote workspace.
	SkipProject bool
}

// ResolvedConfig is the immutable runtime configuration used after startup.
// MCP endpoint credentials stay in memory; callers must not log its private fields.
type ResolvedConfig struct {
	model                  string
	planModel              string
	smolModel              string
	fallbackModels         map[string][]string
	reasoning              string
	planReasoning          string
	smolReasoning          string
	maxTokens              int
	maxTokensConfigured    bool
	permissionModeDefault  string
	bashTimeout            time.Duration
	logLevel               string
	logFile                string
	enableTask             bool
	plan                   bool
	attachPaths            []string
	logFileMaxBytes        int64
	enableWeb              bool
	webSearchURL           string
	agentDir               string
	sessionDir             string
	anthropicBaseURL       string
	openAIBaseURL          string
	fauxScript             string
	settingsPath           string
	skillPaths             []string
	permissionAllowTools   []string
	permissionDenyTools    []string
	retry                  RetrySettings
	mcpServers             []mcpconfig.Server
	lsp                    lsp.Config
	cloudRules             []cloudcmd.Rule
	hooks                  hooks.Settings
	providers              map[string]adapter.ProviderConfig
	untrustedProviderFiles []string
	debugAdapters          map[string]dap.Adapter
	environment            string
	environments           map[string]EnvironmentProcess
	settings               []Setting
	warnings               []string
	workspace              *workspaceSettings
	workspaceListing       WorkspaceListing
}

// workspaceSettings resolves another session workspace's settings on the
// same operator layers.
type workspaceSettings struct {
	resolver *layerResolver
	// root is the workspace whose project files this configuration holds.
	root string
}

// Model returns the default model specification.
func (c *ResolvedConfig) Model() string { return c.model }

// PlanModel returns the plan-agent model specification.
func (c *ResolvedConfig) PlanModel() string { return c.planModel }

// SmolModel returns the small-helper model specification.
func (c *ResolvedConfig) SmolModel() string { return c.smolModel }

// Reasoning returns the default role's reasoning effort: "" when unset (the
// provider default), "off", or one of minimal, low, medium, high.
func (c *ResolvedConfig) Reasoning() string { return c.reasoning }

// PlanReasoning returns the plan role's reasoning effort, inheriting the
// default role's when its own is unset.
func (c *ResolvedConfig) PlanReasoning() string { return firstSet(c.planReasoning, c.reasoning) }

// SmolReasoning returns the smol role's reasoning effort, inheriting the
// default role's when its own is unset.
func (c *ResolvedConfig) SmolReasoning() string { return firstSet(c.smolReasoning, c.reasoning) }

// ReasoningLevels are the accepted reasoning settings; "off" asks for no
// reasoning, the rest map to each provider's effort or thinking budget.
var ReasoningLevels = []string{"off", "minimal", "low", "medium", "high"}

func firstSet(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// MaxTokens returns the maximum model output tokens.
func (c *ResolvedConfig) MaxTokens() int { return c.maxTokens }

// MaxTokensConfigured reports whether the user set an output-token cap.
func (c *ResolvedConfig) MaxTokensConfigured() bool { return c.maxTokensConfigured }

// PermissionModeDefault returns the configured default tool permission mode.
func (c *ResolvedConfig) PermissionModeDefault() string { return c.permissionModeDefault }

// BashTimeout returns the default limit for each shell command.
func (c *ResolvedConfig) BashTimeout() time.Duration { return c.bashTimeout }

// LogLevel returns the configured log level.
func (c *ResolvedConfig) LogLevel() string { return c.logLevel }

// LogFile returns the diagnostic log path.
func (c *ResolvedConfig) LogFile() string { return c.logFile }

// LogFileMaxBytes returns the diagnostic log rotation threshold.
func (c *ResolvedConfig) LogFileMaxBytes() int64 { return c.logFileMaxBytes }

// EnableTask reports whether the delegated task tool is enabled.
func (c *ResolvedConfig) EnableTask() bool { return c.enableTask }

// Plan reports whether the run uses the read-only plan stack.
func (c *ResolvedConfig) Plan() bool { return c.plan }

// AttachPaths returns a defensive copy of startup attachment file paths.
func (c *ResolvedConfig) AttachPaths() []string {
	return append([]string(nil), c.attachPaths...)
}

// EnableWeb reports whether web tools are enabled.
func (c *ResolvedConfig) EnableWeb() bool { return c.enableWeb }

// WebSearchURL returns the configured web search endpoint.
func (c *ResolvedConfig) WebSearchURL() string { return c.webSearchURL }

// AgentDir returns the agent data directory.
func (c *ResolvedConfig) AgentDir() string { return c.agentDir }

// SessionDir returns the session data directory.
func (c *ResolvedConfig) SessionDir() string { return c.sessionDir }

// AnthropicBaseURL returns the Anthropic-compatible base URL.
func (c *ResolvedConfig) AnthropicBaseURL() string { return c.anthropicBaseURL }

// OpenAIBaseURL returns the OpenAI-compatible base URL.
func (c *ResolvedConfig) OpenAIBaseURL() string { return c.openAIBaseURL }

// FauxScript returns the hermetic faux-provider response script path.
func (c *ResolvedConfig) FauxScript() string { return c.fauxScript }

// SettingsPath returns the user settings file read during resolution.
func (c *ResolvedConfig) SettingsPath() string { return c.settingsPath }

// Settings returns every effective settings value with the layer and file,
// variable or flag that set it, sorted by key, credentials redacted. It is the
// same merged tree the resolved accessors were decoded from.
func (c *ResolvedConfig) Settings() []Setting {
	settings := make([]Setting, len(c.settings))
	for i, setting := range c.settings {
		setting.Value = slices.Clone(setting.Value)
		settings[i] = setting
	}
	return settings
}

// Warnings returns configuration problems that did not stop startup:
// unknown settings and repository settings the trust gate ignored. Callers
// log them once.
func (c *ResolvedConfig) Warnings() []string { return slices.Clone(c.warnings) }

// SkillPaths returns a defensive copy of explicit skill locations.
func (c *ResolvedConfig) SkillPaths() []string {
	return append([]string(nil), c.skillPaths...)
}

// PermissionAllowTools returns a defensive copy of tools forced to allow.
func (c *ResolvedConfig) PermissionAllowTools() []string {
	return append([]string(nil), c.permissionAllowTools...)
}

// PermissionDenyTools returns a defensive copy of tools forced to deny.
func (c *ResolvedConfig) PermissionDenyTools() []string {
	return append([]string(nil), c.permissionDenyTools...)
}

// Retry returns the resolved retry policy.
func (c *ResolvedConfig) Retry() RetrySettings { return c.retry }

// Default caps for the session-start workspace listing.
const (
	DefaultWorkspaceListingMaxFiles = 10000
	DefaultWorkspaceListingMaxBytes = 4096
)

// WorkspaceListing is the session-start file listing policy. The zero value
// lists with the default caps.
type WorkspaceListing struct {
	// Disabled is `workspace_listing: false`: new sessions take no listing.
	Disabled bool
	// MaxFiles is the most paths read from git or the directory walk.
	MaxFiles int
	// MaxBytes bounds the listed lines; past it the listing folds to
	// per-directory counts and then cuts with a truncation line.
	MaxBytes int
}

// WorkspaceListing returns the session-start file listing policy.
func (c *ResolvedConfig) WorkspaceListing() WorkspaceListing { return c.workspaceListing }

// Error describes a configuration field that could not be resolved.
type Error struct {
	Field string
	Err   error
}

// Error returns a stable, field-qualified configuration failure. Text that
// looks like a credential is masked, whatever produced the error.
func (e *Error) Error() string { return obs.Redact(fmt.Sprintf("config %s: %v", e.Field, e.Err)) }

// Unwrap returns the underlying parsing or filesystem failure.
func (e *Error) Unwrap() error { return e.Err }

// fileSettings is the schema of every settings layer and of their merge. Scalar
// values are pointers so an omitted setting is distinguishable from a zero.
// Its json tags are the known top-level settings keys.
type fileSettings struct {
	Model                   *string                           `json:"model"`
	PlanModel               *string                           `json:"plan_model"`
	SmolModel               *string                           `json:"smol_model"`
	FallbackModels          map[string][]string               `json:"fallback_models"`
	Reasoning               *string                           `json:"reasoning"`
	PlanReasoning           *string                           `json:"plan_reasoning"`
	SmolReasoning           *string                           `json:"smol_reasoning"`
	MaxTokens               *int                              `json:"max_tokens"`
	PermissionModeDefault   *string                           `json:"permission_mode_default"`
	BashTimeout             *string                           `json:"bash_timeout"`
	LogLevel                *string                           `json:"log_level"`
	LogFile                 *string                           `json:"log_file"`
	LogFileMaxBytes         *int64                            `json:"log_file_max_bytes"`
	EnableWeb               *bool                             `json:"enable_web"`
	WebSearchURL            *string                           `json:"web_search_url"`
	AgentDir                *string                           `json:"agent_dir"`
	EnableTask              *bool                             `json:"enable_task"`
	Plan                    *bool                             `json:"plan"`
	Attach                  []string                          `json:"attach"`
	SessionDir              *string                           `json:"session_dir"`
	AnthropicBaseURL        *string                           `json:"anthropic_base_url"`
	OpenAIBaseURL           *string                           `json:"openai_base_url"`
	FauxScript              *string                           `json:"faux_script"`
	SkillPaths              []string                          `json:"skill_paths"`
	PermissionAllowTools    []string                          `json:"permission_allow_tools"`
	PermissionDenyTools     []string                          `json:"permission_deny_tools"`
	RetryEnabled            *bool                             `json:"retry_enabled"`
	RetryMaxAttempts        *int                              `json:"retry_max_attempts"`
	RetryBaseDelayMS        *int                              `json:"retry_base_delay_ms"`
	RetryBackoffCapMS       *int                              `json:"retry_backoff_cap_ms"`
	RetryMaxDelayMS         *int                              `json:"retry_max_delay_ms"`
	RetryJitterMin          *float64                          `json:"retry_jitter_min"`
	RetryJitterMax          *float64                          `json:"retry_jitter_max"`
	MCPConfig               *string                           `json:"mcp_config"`
	MCPServers              []mcpconfig.Server                `json:"mcp_servers"`
	EnableLSP               *bool                             `json:"enable_lsp"`
	LSPConfig               *string                           `json:"lsp_config"`
	CloudPolicy             *string                           `json:"cloud_policy"`
	Hooks                   map[string][]hooks.Group          `json:"hooks"`
	NoHooks                 *bool                             `json:"no_hooks"`
	DisableAllHooks         *bool                             `json:"disableAllHooks"`
	TrustedHookProjects     []string                          `json:"trusted_hook_projects"`
	Providers               map[string]adapter.ProviderConfig `json:"providers"`
	TrustedProviderProjects []string                          `json:"trusted_provider_projects"`
	DebugAdapters           map[string]dap.Adapter            `json:"debug_adapters"`
	Environment             *string                           `json:"environment"`
	Environments            map[string]EnvironmentProcess     `json:"environments"`
	WorkspaceListing        *bool                             `json:"workspace_listing"`
	ListingMaxFiles         *int                              `json:"workspace_listing_max_files"`
	ListingMaxBytes         *int                              `json:"workspace_listing_max_bytes"`
}

// maxRetryDurationMS is the largest millisecond value convertible to a
// time.Duration (int64 nanoseconds) without overflow.
const maxRetryDurationMS = math.MaxInt64 / 1_000_000

// splitCommaList splits a comma-separated value into trimmed, non-empty items.
func splitCommaList(value string) []string {
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			items = append(items, trimmed)
		}
	}
	return items
}

// Resolve merges every settings layer once and returns validated runtime values.
func (c Config) Resolve() (*ResolvedConfig, error) {
	lookup := c.LookupEnv
	if lookup == nil {
		lookup = os.Getenv
	}
	settingsPath := c.SettingsPath
	if settingsPath == "" {
		settingsPath = GetSettingsPath()
	}
	managedPath := c.ManagedSettingsPath
	if managedPath == "" {
		managedPath = defaultManagedSettingsPath()
	}
	if c.Flags.AnthropicAPIKey.Set && c.Flags.AnthropicAuthToken.Set {
		return nil, &Error{Field: "credentials", Err: errors.New("-anthropic-api-key and -anthropic-auth-token cannot be used together")}
	}
	strict, err := flagOrEnvBool(c.Flags.StrictConfig, lookup("HARNESS_STRICT_CONFIG"))
	if err != nil {
		return nil, &Error{Field: "strict_config", Err: err}
	}
	trustHooks, err := flagOrEnvBool(c.Flags.TrustProjectHooks, lookup("HARNESS_TRUST_PROJECT_HOOKS"))
	if err != nil {
		return nil, &Error{Field: "trust_project_hooks", Err: err}
	}
	trustProviders, err := flagOrEnvBool(c.Flags.TrustProjectProviders, lookup("HARNESS_TRUST_PROJECT_PROVIDERS"))
	if err != nil {
		return nil, &Error{Field: "trust_project_providers", Err: err}
	}
	resolver, err := newLayerResolver(layerOptions{
		managedPath:    managedPath,
		userPath:       settingsPath,
		skipProject:    c.SkipProject,
		profile:        flagOrEnvString(c.Flags.Profile, lookup("HARNESS_PROFILE")),
		strict:         strict,
		trustHooks:     trustHooks,
		trustProviders: trustProviders,
		flags:          c.Flags,
		lookup:         lookup,
	})
	if err != nil {
		return nil, err
	}
	root, err := resolver.workspaceRoot(c.WorkingDir)
	if err != nil {
		return nil, err
	}
	layered, err := resolver.resolve(root)
	if err != nil {
		return nil, err
	}
	settings := layered.settings
	environmentName, environments, err := resolveEnvironments(c.Flags, lookup, settings)
	if err != nil {
		return nil, err
	}
	providers, err := resolveProviders(settings)
	if err != nil {
		return nil, &Error{Field: "providers", Err: err}
	}
	// -mcp-config and HARNESS_MCP_CONFIG already reached settings as layers.
	mcpServers, err := resolveMCP(StringInput{}, "", settings)
	if err != nil {
		return nil, &Error{Field: "mcp_config", Err: err}
	}
	lspConfig, err := resolveLSP(settings)
	if err != nil {
		return nil, err
	}
	cloudRules, err := resolveCloudRules(settings)
	if err != nil {
		return nil, err
	}

	maxTokens := settingValue(settings.MaxTokens)
	if maxTokens <= 0 {
		return nil, &Error{Field: "max_tokens", Err: errors.New("must be positive")}
	}
	bashTimeout, err := time.ParseDuration(settingString(settings.BashTimeout))
	if err != nil || bashTimeout <= 0 || bashTimeout > 2147483*time.Second {
		if err == nil {
			err = errors.New("must be positive and no greater than 2147483s")
		}
		return nil, &Error{Field: "bash_timeout", Err: err}
	}
	logFileMax := settingValue(settings.LogFileMaxBytes)
	if logFileMax <= 0 {
		return nil, &Error{Field: "log_file_max_bytes", Err: errors.New("must be positive")}
	}
	hookSettings, err := layered.hooks()
	if err != nil {
		return nil, err
	}
	retryMaxAttempts := settingValue(settings.RetryMaxAttempts)
	if retryMaxAttempts < 1 {
		return nil, &Error{Field: "retry_max_attempts", Err: errors.New("must be at least one")}
	}
	retryBaseDelayMS := settingValue(settings.RetryBaseDelayMS)
	if retryBaseDelayMS <= 0 || retryBaseDelayMS > maxRetryDurationMS {
		return nil, &Error{Field: "retry_base_delay_ms", Err: errors.New("must be positive and representable as a duration")}
	}
	retryBackoffCapMS := settingValue(settings.RetryBackoffCapMS)
	if retryBackoffCapMS < retryBaseDelayMS || retryBackoffCapMS > maxRetryDurationMS {
		return nil, &Error{Field: "retry_backoff_cap_ms", Err: errors.New("must be at least retry_base_delay_ms and representable as a duration")}
	}
	retryMaxDelayMS := settingValue(settings.RetryMaxDelayMS)
	if retryMaxDelayMS > maxRetryDurationMS {
		return nil, &Error{Field: "retry_max_delay_ms", Err: errors.New("must be representable as a duration")}
	}
	retryJitterMin := settingValue(settings.RetryJitterMin)
	if math.IsNaN(retryJitterMin) || math.IsInf(retryJitterMin, 0) || retryJitterMin < 0 || retryJitterMin > 1 {
		return nil, &Error{Field: "retry_jitter_min", Err: errors.New("must be finite and within [0, 1]")}
	}
	retryJitterMax := settingValue(settings.RetryJitterMax)
	if math.IsNaN(retryJitterMax) || math.IsInf(retryJitterMax, 0) || retryJitterMax < 0 || retryJitterMax > 1 || retryJitterMax < retryJitterMin {
		return nil, &Error{Field: "retry_jitter_max", Err: errors.New("must be finite, within [0, 1], and at least retry_jitter_min")}
	}
	listingMaxFiles := settingValue(settings.ListingMaxFiles)
	if listingMaxFiles <= 0 {
		return nil, &Error{Field: "workspace_listing_max_files", Err: errors.New("must be positive")}
	}
	listingMaxBytes := settingValue(settings.ListingMaxBytes)
	if listingMaxBytes <= 0 {
		return nil, &Error{Field: "workspace_listing_max_bytes", Err: errors.New("must be positive")}
	}
	debugAdapters, err := dap.Resolve(settings.DebugAdapters)
	if err != nil {
		return nil, &Error{Field: "debug_adapters", Err: err}
	}
	fallbackModels, err := resolveFallbackModels(settings.FallbackModels)
	if err != nil {
		return nil, err
	}
	resolved := &ResolvedConfig{
		environment:            environmentName,
		environments:           environments,
		model:                  settingString(settings.Model),
		planModel:              settingString(settings.PlanModel),
		smolModel:              settingString(settings.SmolModel),
		fallbackModels:         fallbackModels,
		reasoning:              strings.ToLower(settingString(settings.Reasoning)),
		planReasoning:          strings.ToLower(settingString(settings.PlanReasoning)),
		smolReasoning:          strings.ToLower(settingString(settings.SmolReasoning)),
		maxTokens:              maxTokens,
		maxTokensConfigured:    layered.configured("max_tokens"),
		permissionModeDefault:  settingString(settings.PermissionModeDefault),
		bashTimeout:            bashTimeout,
		logLevel:               settingString(settings.LogLevel),
		logFile:                settingString(settings.LogFile),
		logFileMaxBytes:        logFileMax,
		enableWeb:              settingValue(settings.EnableWeb),
		enableTask:             settingValue(settings.EnableTask),
		plan:                   settingValue(settings.Plan),
		attachPaths:            slices.Clone(settings.Attach),
		webSearchURL:           settingString(settings.WebSearchURL),
		agentDir:               ExpandTildePath(settingString(settings.AgentDir)),
		sessionDir:             ExpandTildePath(settingString(settings.SessionDir)),
		anthropicBaseURL:       settingString(settings.AnthropicBaseURL),
		openAIBaseURL:          settingString(settings.OpenAIBaseURL),
		fauxScript:             settingString(settings.FauxScript),
		settingsPath:           settingsPath,
		skillPaths:             slices.Clone(settings.SkillPaths),
		permissionAllowTools:   slices.Clone(settings.PermissionAllowTools),
		permissionDenyTools:    slices.Clone(settings.PermissionDenyTools),
		mcpServers:             mcpServers,
		lsp:                    lspConfig,
		cloudRules:             cloudRules,
		hooks:                  hookSettings,
		providers:              providers,
		untrustedProviderFiles: layered.untrustedProviderFiles,
		debugAdapters:          debugAdapters,
		settings:               layered.list,
		warnings:               slices.Concat(resolver.operatorWarnings, layered.warnings),
		workspace:              &workspaceSettings{resolver: resolver, root: root},
		workspaceListing:       WorkspaceListing{Disabled: !settingValue(settings.WorkspaceListing), MaxFiles: listingMaxFiles, MaxBytes: listingMaxBytes},
		retry: RetrySettings{
			Enabled:      settingValue(settings.RetryEnabled),
			MaxAttempts:  retryMaxAttempts,
			BaseDelayMS:  retryBaseDelayMS,
			BackoffCapMS: retryBackoffCapMS,
			MaxDelayMS:   retryMaxDelayMS,
			JitterMin:    retryJitterMin,
			JitterMax:    retryJitterMax,
		},
	}
	if err := validateResolved(resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}

// settingString trims a merged string setting; unset is empty.
func settingString(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

// settingValue dereferences a merged setting. Unset is the zero value, which
// only happens for keys without a built-in default.
func settingValue[T any](value *T) T {
	var zero T
	if value == nil {
		return zero
	}
	return *value
}

// validateResolved catches values that are individually parsed but can't safely
// enter the rest of the runtime after source precedence is applied.
func validateResolved(c *ResolvedConfig) error {
	for field, model := range map[string]string{"model": c.model, "plan_model": c.planModel, "smol_model": c.smolModel} {
		if strings.TrimSpace(model) == "" && field != "model" {
			continue
		}
		if err := adapter.ValidateModelSpecWithProviders(model, c.providers); err != nil {
			return &Error{Field: field, Err: err}
		}
		if c.maxTokensConfigured && strings.HasPrefix(strings.TrimSpace(model), "openai-chatgpt:") {
			return &Error{Field: "max_tokens", Err: errors.New("ChatGPT plan usage does not support explicit output-token caps; use turn or wall-time limits")}
		}
	}
	for field, baseURL := range map[string]string{"anthropic_base_url": c.anthropicBaseURL, "openai_base_url": c.openAIBaseURL} {
		if baseURL == "" {
			continue
		}
		if err := adapter.ValidateBaseURL(baseURL); err != nil {
			return &Error{Field: field, Err: err}
		}
	}
	for field, level := range map[string]string{"reasoning": c.reasoning, "plan_reasoning": c.planReasoning, "smol_reasoning": c.smolReasoning} {
		if level != "" && !slices.Contains(ReasoningLevels, level) {
			return &Error{Field: field, Err: fmt.Errorf("unknown level %q (valid: %s)", level, strings.Join(ReasoningLevels, ", "))}
		}
	}
	switch c.permissionModeDefault {
	case "default", "plan", "acceptEdits", "bypass":
	default:
		return &Error{Field: "permission_mode_default", Err: fmt.Errorf("unknown mode %q (valid modes: default, plan, acceptEdits, bypass)", c.permissionModeDefault)}
	}
	switch strings.ToLower(c.logLevel) {
	case "debug", "info", "warn", "warning", "error":
	default:
		return &Error{Field: "log_level", Err: fmt.Errorf("unknown level %q", c.logLevel)}
	}
	return validateFallbackModels(c)
}

// Hooks returns an owned snapshot of the resolved hook settings, including a
// trusted project's hooks for the resolved workspace.
func (c *ResolvedConfig) Hooks() hooks.Settings { return c.hooks.Clone() }

// WorkspaceHooks returns the hook settings for a session in cwd: the same
// resolution as [Config.Resolve] with cwd's project files in place of the
// resolved workspace's, so trust, -c overrides and managed requirements
// apply alike. The warnings name the repository settings that resolution
// ignored; they are empty when cwd is the resolved workspace.
func (c *ResolvedConfig) WorkspaceHooks(cwd string) (hooks.Settings, []string, error) {
	if c.workspace == nil {
		return c.Hooks(), nil, nil
	}
	root, err := c.workspace.resolver.workspaceRoot(cwd)
	if err != nil {
		return hooks.Settings{}, nil, err
	}
	if root == c.workspace.root {
		return c.Hooks(), nil, nil
	}
	layered, err := c.workspace.resolver.resolve(root)
	if err != nil {
		return hooks.Settings{}, nil, err
	}
	settings, err := layered.hooks()
	if err != nil {
		return hooks.Settings{}, nil, err
	}
	return settings, layered.warnings, nil
}

// hooks validates the merged hook settings.
func (s *layeredSettings) hooks() (hooks.Settings, error) {
	settings := hooks.Settings{Hooks: s.settings.Hooks, NoHooks: settingValue(s.settings.NoHooks)}
	if err := settings.Validate(); err != nil {
		return hooks.Settings{}, &Error{Field: "hooks", Err: err}
	}
	return settings, nil
}

// DebugAdapters returns an owned snapshot of trusted debugger definitions.
func (c *ResolvedConfig) DebugAdapters() map[string]dap.Adapter {
	result := make(map[string]dap.Adapter, len(c.debugAdapters))
	for name, adapter := range c.debugAdapters {
		adapter.Args = slices.Clone(adapter.Args)
		adapter.Launch = cloneDebugArguments(adapter.Launch)
		adapter.Attach = cloneDebugArguments(adapter.Attach)
		result[name] = adapter
	}
	return result
}

func cloneDebugArguments(arguments map[string]json.RawMessage) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(arguments))
	for key, value := range arguments {
		result[key] = slices.Clone(value)
	}
	return result
}

// flagOrEnvString resolves an input that is not a settings key, such as the
// profile name: the flag when set, else the nonblank variable.
func flagOrEnvString(flag StringInput, env string) string {
	if flag.Set {
		return strings.TrimSpace(flag.Value)
	}
	return strings.TrimSpace(env)
}

// flagOrEnvBool resolves a boolean that is not a settings key, such as project
// trust. An explicit false flag still outranks the variable.
func flagOrEnvBool(flag BoolInput, env string) (bool, error) {
	if flag.Set {
		return flag.Value, nil
	}
	if value := strings.TrimSpace(env); value != "" {
		return strconv.ParseBool(value)
	}
	return false, nil
}

// ExpandTildePath expands a leading ~ or ~/ in path to the user's home directory.
func ExpandTildePath(path string) string { return normalizePath(path) }

// GetAgentDir returns the agent config directory, honoring EnvAgentDir and
// falling back to ~/.config/harness/agent.
func GetAgentDir() string {
	if envDir := strings.TrimSpace(os.Getenv(EnvAgentDir)); envDir != "" {
		return ExpandTildePath(envDir)
	}
	return defaultAgentDir()
}

// defaultAgentDir still gives callers a relative agent location when home
// lookup fails, rather than making config resolution fail.
func defaultAgentDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(ConfigDirName, "agent")
	}
	return filepath.Join(home, ".config", ConfigDirName, "agent")
}

// GetModelsPath returns the path to models.json in the agent directory.
func GetModelsPath() string { return filepath.Join(GetAgentDir(), "models.json") }

// GetAuthPath returns the path to auth.json in the agent directory.
func GetAuthPath() string { return filepath.Join(GetAgentDir(), "auth.json") }

// GetSettingsPath returns the path to settings.json in the harness config directory.
func GetSettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(ConfigDirName, "settings.json")
	}
	return filepath.Join(home, ".config", ConfigDirName, "settings.json")
}

func normalizePath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}
