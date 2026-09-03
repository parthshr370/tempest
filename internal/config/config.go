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
	"strconv"
	"strings"

	"go.harness.dev/harness/internal/adapter"
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
	defaultModel          = "claude-opus-4-8"
	defaultPermissionMode = "default"
	defaultLogLevel       = "info"
	defaultLogFileMax     = 100 << 20
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
	Model                StringInput
	PlanModel            StringInput
	SmolModel            StringInput
	MaxTokens            IntInput
	PermissionMode       StringInput
	LogLevel             StringInput
	LogFile              StringInput
	LogFileMaxBytes      IntInput
	EnableTask           BoolInput
	Plan                 BoolInput
	Attach               StringsInput
	EnableWeb            BoolInput
	WebSearchURL         StringInput
	AgentDir             StringInput
	SessionDir           StringInput
	AnthropicBaseURL     StringInput
	OpenAIBaseURL        StringInput
	AnthropicAPIKey      StringInput
	AnthropicAuthToken   StringInput
	FauxScript           StringInput
	SkillPaths           StringsInput
	PermissionAllowTools StringsInput
	PermissionDenyTools  StringsInput
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

// Config is the raw configuration boundary. Flags win over HARNESS_* values,
// which win over settings.json, which win over built-in defaults.
type Config struct {
	Flags        FlagValues
	LookupEnv    func(string) string
	SettingsPath string
}

// ResolvedConfig is the immutable, non-secret configuration used after startup.
type ResolvedConfig struct {
	model                 string
	planModel             string
	smolModel             string
	maxTokens             int
	permissionModeDefault string
	logLevel              string
	logFile               string
	enableTask            bool
	plan                  bool
	attachPaths           []string
	logFileMaxBytes       int64
	enableWeb             bool
	webSearchURL          string
	agentDir              string
	sessionDir            string
	anthropicBaseURL      string
	openAIBaseURL         string
	fauxScript            string
	settingsPath          string
	skillPaths            []string
	permissionAllowTools  []string
	permissionDenyTools   []string
	retry                 RetrySettings
}

// Model returns the default model specification.
func (c *ResolvedConfig) Model() string { return c.model }

// PlanModel returns the plan-agent model specification.
func (c *ResolvedConfig) PlanModel() string { return c.planModel }

// SmolModel returns the small-helper model specification.
func (c *ResolvedConfig) SmolModel() string { return c.smolModel }

// MaxTokens returns the maximum model output tokens.
func (c *ResolvedConfig) MaxTokens() int { return c.maxTokens }

// PermissionModeDefault returns the configured default tool permission mode.
func (c *ResolvedConfig) PermissionModeDefault() string { return c.permissionModeDefault }

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

// SettingsPath returns the settings file read during resolution.
func (c *ResolvedConfig) SettingsPath() string { return c.settingsPath }

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

// Error describes a configuration field that could not be resolved.
type Error struct {
	Field string
	Err   error
}

// Error returns a stable, field-qualified configuration failure.
func (e *Error) Error() string { return fmt.Sprintf("config %s: %v", e.Field, e.Err) }

// Unwrap returns the underlying parsing or filesystem failure.
func (e *Error) Unwrap() error { return e.Err }

// fileSettings keeps scalar values as pointers so an omitted setting won't
// shadow an environment value or built-in default.
type fileSettings struct {
	Model                 *string  `json:"model"`
	PlanModel             *string  `json:"plan_model"`
	SmolModel             *string  `json:"smol_model"`
	MaxTokens             *int     `json:"max_tokens"`
	PermissionModeDefault *string  `json:"permission_mode_default"`
	LogLevel              *string  `json:"log_level"`
	LogFile               *string  `json:"log_file"`
	LogFileMaxBytes       *int64   `json:"log_file_max_bytes"`
	EnableWeb             *bool    `json:"enable_web"`
	WebSearchURL          *string  `json:"web_search_url"`
	AgentDir              *string  `json:"agent_dir"`
	EnableTask            *bool    `json:"enable_task"`
	Plan                  *bool    `json:"plan"`
	Attach                []string `json:"attach"`
	SessionDir            *string  `json:"session_dir"`
	AnthropicBaseURL      *string  `json:"anthropic_base_url"`
	OpenAIBaseURL         *string  `json:"openai_base_url"`
	FauxScript            *string  `json:"faux_script"`
	SkillPaths            []string `json:"skill_paths"`
	PermissionAllowTools  []string `json:"permission_allow_tools"`
	PermissionDenyTools   []string `json:"permission_deny_tools"`
	RetryEnabled          *bool    `json:"retry_enabled"`
	RetryMaxAttempts      *int     `json:"retry_max_attempts"`
	RetryBaseDelayMS      *int     `json:"retry_base_delay_ms"`
	RetryBackoffCapMS     *int     `json:"retry_backoff_cap_ms"`
	RetryMaxDelayMS       *int     `json:"retry_max_delay_ms"`
	RetryJitterMin        *float64 `json:"retry_jitter_min"`
	RetryJitterMax        *float64 `json:"retry_jitter_max"`
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

// resolveToolList resolves a repeatable tool-name list from flag, then env, then settings.
func resolveToolList(flag StringsInput, env string, settings []string) []string {
	if flag.Set {
		return append([]string(nil), flag.Values...)
	}
	if strings.TrimSpace(env) != "" {
		return splitCommaList(env)
	}
	return append([]string(nil), settings...)
}

// Resolve reads settings once and returns validated values with no secret data.
func (c Config) Resolve() (*ResolvedConfig, error) {
	lookup := c.LookupEnv
	if lookup == nil {
		lookup = os.Getenv
	}
	settingsPath := c.SettingsPath
	if settingsPath == "" {
		settingsPath = GetSettingsPath()
	}
	settings, err := loadSettings(settingsPath)
	if err != nil {
		return nil, err
	}
	if c.Flags.AnthropicAPIKey.Set && c.Flags.AnthropicAuthToken.Set {
		return nil, &Error{Field: "credentials", Err: errors.New("-anthropic-api-key and -anthropic-auth-token cannot be used together")}
	}

	agentDir := resolveString(c.Flags.AgentDir, lookup(EnvAgentDir), settings.AgentDir, defaultAgentDir())
	sessionDir := resolveString(c.Flags.SessionDir, lookup(EnvSessionDir), settings.SessionDir, filepath.Join(agentDir, "sessions"))
	maxTokens, err := resolveInt(c.Flags.MaxTokens, lookup("HARNESS_MAX_TOKENS"), settings.MaxTokens, adapter.DefaultMaxTokens)
	if err != nil || maxTokens <= 0 {
		if err == nil {
			err = errors.New("must be positive")
		}
		return nil, &Error{Field: "max_tokens", Err: err}
	}
	logFileMax, err := resolveInt64(c.Flags.LogFileMaxBytes, lookup("HARNESS_LOG_FILE_MAX_BYTES"), settings.LogFileMaxBytes, defaultLogFileMax)
	if err != nil || logFileMax <= 0 {
		if err == nil {
			err = errors.New("must be positive")
		}
		return nil, &Error{Field: "log_file_max_bytes", Err: err}
	}
	webEnabled, err := resolveBool(c.Flags.EnableWeb, lookup("HARNESS_ENABLE_WEB"), settings.EnableWeb, false)
	if err != nil {
		return nil, &Error{Field: "enable_web", Err: err}
	}
	retryEnabled, err := resolveBool(BoolInput{}, lookup("HARNESS_RETRY_ENABLED"), settings.RetryEnabled, true)
	if err != nil {
		return nil, &Error{Field: "retry_enabled", Err: err}
	}
	taskEnabled, err := resolveBool(c.Flags.EnableTask, lookup("HARNESS_ENABLE_TASK"), settings.EnableTask, false)
	if err != nil {
		return nil, &Error{Field: "enable_task", Err: err}
	}
	planEnabled, err := resolveBool(c.Flags.Plan, lookup("HARNESS_PLAN"), settings.Plan, false)
	if err != nil {
		return nil, &Error{Field: "plan", Err: err}
	}
	retryMaxAttempts, err := resolveInt(IntInput{}, lookup("HARNESS_RETRY_MAX_ATTEMPTS"), settings.RetryMaxAttempts, 11)
	if err != nil || retryMaxAttempts < 1 {
		if err == nil {
			err = errors.New("must be at least one")
		}
		return nil, &Error{Field: "retry_max_attempts", Err: err}
	}
	retryBaseDelayMS, err := resolveInt(IntInput{}, lookup("HARNESS_RETRY_BASE_DELAY_MS"), settings.RetryBaseDelayMS, 500)
	if err != nil || retryBaseDelayMS <= 0 || retryBaseDelayMS > maxRetryDurationMS {
		if err == nil {
			err = errors.New("must be positive and representable as a duration")
		}
		return nil, &Error{Field: "retry_base_delay_ms", Err: err}
	}
	retryBackoffCapMS, err := resolveInt(IntInput{}, lookup("HARNESS_RETRY_BACKOFF_CAP_MS"), settings.RetryBackoffCapMS, 8000)
	if err != nil || retryBackoffCapMS < retryBaseDelayMS || retryBackoffCapMS > maxRetryDurationMS {
		if err == nil {
			err = errors.New("must be at least retry_base_delay_ms and representable as a duration")
		}
		return nil, &Error{Field: "retry_backoff_cap_ms", Err: err}
	}
	retryMaxDelayMS, err := resolveInt(IntInput{}, lookup("HARNESS_RETRY_MAX_DELAY_MS"), settings.RetryMaxDelayMS, 300000)
	if err != nil {
		return nil, &Error{Field: "retry_max_delay_ms", Err: err}
	}
	if retryMaxDelayMS > maxRetryDurationMS {
		return nil, &Error{Field: "retry_max_delay_ms", Err: errors.New("must be representable as a duration")}
	}
	retryJitterMin, err := resolveFloat(lookup("HARNESS_RETRY_JITTER_MIN"), settings.RetryJitterMin, 0.75)
	if err != nil || math.IsNaN(retryJitterMin) || math.IsInf(retryJitterMin, 0) || retryJitterMin < 0 || retryJitterMin > 1 {
		if err == nil {
			err = errors.New("must be finite and within [0, 1]")
		}
		return nil, &Error{Field: "retry_jitter_min", Err: err}
	}
	retryJitterMax, err := resolveFloat(lookup("HARNESS_RETRY_JITTER_MAX"), settings.RetryJitterMax, 1)
	if err != nil || math.IsNaN(retryJitterMax) || math.IsInf(retryJitterMax, 0) || retryJitterMax < 0 || retryJitterMax > 1 || retryJitterMax < retryJitterMin {
		if err == nil {
			err = errors.New("must be finite, within [0, 1], and at least retry_jitter_min")
		}
		return nil, &Error{Field: "retry_jitter_max", Err: err}
	}
	attachPaths := resolveToolList(c.Flags.Attach, lookup("HARNESS_ATTACH"), settings.Attach)
	var skillPaths []string
	if c.Flags.SkillPaths.Set {
		skillPaths = append([]string(nil), c.Flags.SkillPaths.Values...)
		for _, path := range skillPaths {
			if strings.TrimSpace(path) == "" {
				return nil, &Error{Field: "skill", Err: errors.New("must not be empty")}
			}
		}
	} else {
		skillPaths = append([]string(nil), settings.SkillPaths...)
	}
	permissionAllowTools := resolveToolList(c.Flags.PermissionAllowTools, lookup("HARNESS_PERMISSION_ALLOW_TOOLS"), settings.PermissionAllowTools)
	permissionDenyTools := resolveToolList(c.Flags.PermissionDenyTools, lookup("HARNESS_PERMISSION_DENY_TOOLS"), settings.PermissionDenyTools)
	resolved := &ResolvedConfig{
		model:                 resolveString(c.Flags.Model, lookup("HARNESS_MODEL"), settings.Model, defaultModel),
		planModel:             resolveString(c.Flags.PlanModel, lookup("HARNESS_PLAN_MODEL"), settings.PlanModel, ""),
		smolModel:             resolveString(c.Flags.SmolModel, lookup("HARNESS_SMOL_MODEL"), settings.SmolModel, ""),
		maxTokens:             maxTokens,
		permissionModeDefault: resolveString(c.Flags.PermissionMode, lookup("HARNESS_PERMISSION_MODE"), settings.PermissionModeDefault, defaultPermissionMode),
		logLevel:              resolveLogLevel(c.Flags.LogLevel, lookup("HARNESS_LOG_LEVEL"), lookup("HARNESS_DEBUG"), settings.LogLevel),
		logFile:               resolveString(c.Flags.LogFile, lookup("HARNESS_LOG_FILE"), settings.LogFile, ""),
		logFileMaxBytes:       logFileMax,
		enableWeb:             webEnabled,
		enableTask:            taskEnabled,
		plan:                  planEnabled,
		attachPaths:           attachPaths,
		webSearchURL:          resolveString(c.Flags.WebSearchURL, lookup("HARNESS_WEB_SEARCH_URL"), settings.WebSearchURL, ""),
		agentDir:              ExpandTildePath(agentDir),
		sessionDir:            ExpandTildePath(sessionDir),
		anthropicBaseURL:      resolveString(c.Flags.AnthropicBaseURL, lookup("HARNESS_ANTHROPIC_BASE_URL"), settings.AnthropicBaseURL, ""),
		openAIBaseURL:         resolveString(c.Flags.OpenAIBaseURL, lookup("HARNESS_OPENAI_BASE_URL"), settings.OpenAIBaseURL, ""),
		fauxScript:            resolveString(c.Flags.FauxScript, lookup("HARNESS_FAUX_SCRIPT"), settings.FauxScript, ""),
		settingsPath:          settingsPath,
		skillPaths:            skillPaths,
		permissionAllowTools:  permissionAllowTools,
		permissionDenyTools:   permissionDenyTools,
		retry: RetrySettings{
			Enabled:      retryEnabled,
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

// validateResolved catches values that are individually parsed but can't safely
// enter the rest of the runtime after source precedence is applied.
func validateResolved(c *ResolvedConfig) error {
	for field, model := range map[string]string{"model": c.model, "plan_model": c.planModel, "smol_model": c.smolModel} {
		if strings.TrimSpace(model) == "" && field != "model" {
			continue
		}
		if err := adapter.ValidateModelSpec(model); err != nil {
			return &Error{Field: field, Err: err}
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
	switch c.permissionModeDefault {
	case "default", "plan", "acceptEdits", "bypass":
	default:
		return &Error{Field: "permission_mode_default", Err: fmt.Errorf("unknown mode %q", c.permissionModeDefault)}
	}
	switch strings.ToLower(c.logLevel) {
	case "debug", "info", "warn", "warning", "error":
	default:
		return &Error{Field: "log_level", Err: fmt.Errorf("unknown level %q", c.logLevel)}
	}
	return nil
}

// loadSettings treats a missing settings file as no overrides, but keeps real
// I/O and decode failures tied to the settings field.
func loadSettings(path string) (fileSettings, error) {
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileSettings{}, nil
	}
	if err != nil {
		return fileSettings{}, &Error{Field: "settings", Err: fmt.Errorf("read %s: %w", path, err)}
	}
	var settings fileSettings
	if err := json.Unmarshal(contents, &settings); err != nil {
		return fileSettings{}, &Error{Field: "settings", Err: fmt.Errorf("decode %s: %w", path, err)}
	}
	return settings, nil
}

// resolveString applies configuration precedence while treating blank values as
// unset, so they don't hide a lower-priority source.
func resolveString(flag StringInput, env string, file *string, fallback string) string {
	if flag.Set {
		return strings.TrimSpace(flag.Value)
	}
	if value := strings.TrimSpace(env); value != "" {
		return value
	}
	if file != nil {
		return strings.TrimSpace(*file)
	}
	return fallback
}

// resolveLogLevel resolves the explicit log level before its environment
// aliases, then settings and the built-in default.
func resolveLogLevel(flag StringInput, logLevelEnv, debugEnv string, file *string) string {
	if flag.Set {
		return strings.TrimSpace(flag.Value)
	}
	if value := strings.TrimSpace(logLevelEnv); value != "" {
		return value
	}
	if strings.TrimSpace(debugEnv) != "" {
		return "debug"
	}
	if file != nil {
		return strings.TrimSpace(*file)
	}
	return defaultLogLevel
}

// resolveInt preserves the flag, environment, settings, and fallback precedence
// while delaying environment parsing until it wins.
func resolveInt(flag IntInput, env string, file *int, fallback int) (int, error) {
	if flag.Set {
		return flag.Value, nil
	}
	if value := strings.TrimSpace(env); value != "" {
		parsed, err := strconv.Atoi(value)
		return parsed, err
	}
	if file != nil {
		return *file, nil
	}
	return fallback, nil
}

// resolveInt64 mirrors integer precedence for byte-sized settings that need an
// int64 fallback.
func resolveInt64(flag IntInput, env string, file *int64, fallback int64) (int64, error) {
	if flag.Set {
		return int64(flag.Value), nil
	}
	if value := strings.TrimSpace(env); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		return parsed, err
	}
	if file != nil {
		return *file, nil
	}
	return fallback, nil
}

// resolveBool uses BoolInput.Set so an explicit false flag still outranks every
// lower-priority source.
func resolveBool(flag BoolInput, env string, file *bool, fallback bool) (bool, error) {
	if flag.Set {
		return flag.Value, nil
	}

	if value := strings.TrimSpace(env); value != "" {
		parsed, err := strconv.ParseBool(value)
		return parsed, err
	}
	if file != nil {
		return *file, nil
	}
	return fallback, nil
}

// resolveFloat has no flag source, so environment overrides settings before the
// built-in fallback.
func resolveFloat(env string, file *float64, fallback float64) (float64, error) {
	if value := strings.TrimSpace(env); value != "" {
		return strconv.ParseFloat(value, 64)
	}
	if file != nil {
		return *file, nil
	}
	return fallback, nil
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
