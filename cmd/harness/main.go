// Command harness runs a generic coding agent once and writes its final result.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"go.harness.dev/harness/internal/adapter"
	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/discovery"
	"go.harness.dev/harness/internal/document"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/retry"
	"go.harness.dev/harness/internal/session"
	"go.harness.dev/harness/internal/session/journal"
)

// commandOptions keeps parsed CLI-only inputs beside the resolved config and secret lookup they feed into the runtime.
type commandOptions struct {
	flags        config.FlagValues
	Config       *config.ResolvedConfig
	Secrets      config.SecretResolver
	Prompt       string
	PrintMode    string
	OutputFormat string
	ExcludeTools string
	Resume       string
	Continue     bool
	Version      bool
}

// skillPathsFlag adapts repeated -skill values to config's tracked input, so resolution can still tell flags from defaults.
type skillPathsFlag struct {
	input *config.StringsInput
}

func (f skillPathsFlag) String() string { return strings.Join(f.input.Values, ",") }

func (f skillPathsFlag) Set(value string) error {
	f.input.Values = append(f.input.Values, value)
	f.input.Set = true
	return nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches the named inspection commands before treating other arguments as a one-shot prompt.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "doctor":
			return runDoctor(args[1:], stdout, stderr)
		case "version":
			return runVersion(stdout)
		}
	}
	return runOneShot(args, stdout, stderr)
}

// runOneShot wires resolved CLI config into one non-interactive agent run, including discovery and optional session continuity.
func runOneShot(args []string, stdout, stderr io.Writer) int {
	opts, err := parseCommandOptions(args, stderr)
	if err != nil {
		return 1
	}
	if opts.Version {
		return runVersion(stdout)
	}
	if err := resolveCommandConfig(&opts); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	setupLogging(opts.Config.LogLevel(), opts.Config.LogFile(), opts.Config.LogFileMaxBytes())
	if opts.Prompt == "" {
		fmt.Fprintln(stderr, "usage: harness -p <prompt> [-model <id>] [-output-format text|json]")
		return 1
	}
	permissionMode, err := permission.ParseMode(opts.Config.PermissionModeDefault())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	permissionPolicy := buildPermissionPolicy(opts.Config.PermissionAllowTools(), opts.Config.PermissionDenyTools())
	var retryConfig *retry.Config
	if settings := opts.Config.Retry(); settings.Enabled {
		resolvedRetry := retry.DefaultConfig()
		resolvedRetry.MaxAttempts = settings.MaxAttempts
		resolvedRetry.BaseDelay = time.Duration(settings.BaseDelayMS) * time.Millisecond
		resolvedRetry.BackoffCap = time.Duration(settings.BackoffCapMS) * time.Millisecond
		resolvedRetry.MaxDelay = time.Duration(settings.MaxDelayMS) * time.Millisecond
		resolvedRetry.Jitter = retry.Jitter{Min: settings.JitterMin, Max: settings.JitterMax}
		retryConfig = &resolvedRetry
	}
	roles, err := adapter.ResolveRoles(adapter.RoleRoutingConfig{
		DefaultModel:     opts.Config.Model(),
		PlanModel:        opts.Config.PlanModel(),
		SmolModel:        opts.Config.SmolModel(),
		AnthropicBaseURL: opts.Config.AnthropicBaseURL(),
		OpenAIBaseURL:    opts.Config.OpenAIBaseURL(),
		MaxTokens:        opts.Config.MaxTokens(),
		Retry:            retryConfig,
		SecretLookup:     opts.Secrets.Resolve,
	})
	if err != nil {
		fmt.Fprintf(stderr, "provider config error: %v\n", err)
		return 1
	}
	streamFn := roles.Default.StreamFn
	selectedModel := roles.Default.Model
	planSelected := roles.Plan.Model
	stackModel := roles.Default.Model
	stackStreamFn := roles.Default.StreamFn
	if opts.Config.FauxScript() != "" {
		fauxProvider, fauxModel, err := loadFauxScript(opts.Config.FauxScript())
		if err != nil {
			fmt.Fprintf(stderr, "faux script error: %v\n", err)
			return 1
		}
		selectedModel = fauxModel
		stackModel = fauxModel
		streamFn = fauxProvider.StreamSimple
		stackStreamFn = streamFn
	} else if !providerCredentialPresent(selectedModel.Provider, opts.Secrets) {
		fmt.Fprintf(stderr, "warning: no %s credential resolved; model calls will fail until one is present\n", selectedModel.Provider)
	}
	if opts.Config.Plan() {
		// The plan flag routes the whole run through the read-only stack on the
		// plan role, not just the plan-agent model.
		stackModel = roles.Plan.Model
		stackStreamFn = roles.Plan.StreamFn
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "cwd error: %v\n", err)
		return 1
	}
	runCtx := context.Background()
	inputs, err := discovery.Discover(runCtx, discovery.Options{
		Cwd:                  cwd,
		AgentDir:             opts.Config.AgentDir(),
		SkillPaths:           opts.Config.SkillPaths(),
		IncludeDefaultSkills: true,
	})
	if err != nil {
		fmt.Fprintf(stderr, "context discovery error: %v\n", err)
		return 1
	}
	for _, diagnostic := range inputs.Diagnostics {
		slog.Warn("discovery diagnostic", "code", diagnostic.Code, "path", diagnostic.Path, "message", diagnostic.Message)
	}

	sessionStore, seedMessages, err := openSession(runCtx, opts.Config.SessionDir(), cwd, opts.Resume, opts.Continue, selectedModel)
	if err != nil {
		fmt.Fprintf(stderr, "session error: %v\n", err)
		return 1
	}
	if sessionStore != nil {
		defer func() {
			if err := sessionStore.Close(); err != nil {
				slog.Warn("session close failed", "error", err)
			}
		}()
	}
	var attachRegistry *document.AttachmentRegistry
	var attachReader *document.CacheRootBlobReader
	if attachPaths := opts.Config.AttachPaths(); len(attachPaths) > 0 {
		attachRegistry, attachReader, err = session.ResolveLocalAttachments(runCtx, attachPaths, filepath.Join(opts.Config.SessionDir(), "attachments"))
		if err != nil {
			fmt.Fprintf(stderr, "attach error: %v\n", err)
			return 1
		}
	}
	stack, err := session.BuildAgentStack(session.StackConfig{
		Cwd:                cwd,
		Model:              stackModel,
		PlanModel:          planSelected,
		SmolModel:          roles.Smol.Model,
		StreamFn:           stackStreamFn,
		SmolStreamFn:       roles.Smol.StreamFn,
		PermissionMode:     permissionMode,
		PermissionPolicy:   permissionPolicy,
		ExcludeTools:       splitCSV(opts.ExcludeTools),
		EnableTask:         opts.Config.EnableTask(),
		EnableWeb:          opts.Config.EnableWeb(),
		WebSearchURL:       opts.Config.WebSearchURL(),
		ContextFiles:       inputs.ContextFiles,
		Skills:             inputs.Skills,
		GenericRules:       inputs.GenericRules,
		InitialMessages:    seedMessages,
		AttachmentRegistry: attachRegistry,
		AttachmentReader:   attachReader,
	})
	if err != nil {
		fmt.Fprintf(stderr, "agent stack error: %v\n", err)
		return 1
	}
	mode, err := resolveOutputFormat(opts.OutputFormat, opts.PrintMode)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	completion, err := session.RunCompletionLoop(runCtx, stack.Agent, session.InitialPrompt{Text: opts.Prompt}, session.CompletionOptions{Enabled: permissionMode != permission.ModePlan, ProjectDir: cwd})
	if err != nil {
		fmt.Fprintf(stderr, "prompt error: %v\n", err)
		return 1
	}
	if completion.Status == session.PromiseMaxContinuations {
		fmt.Fprintf(stderr, "completion warning: %s\n", completion.Reason)
	}
	state := stack.Agent.State()
	if sessionStore != nil {
		persistNewMessages(runCtx, sessionStore, state.Messages, seedMessages)
	}
	if len(state.Messages) == 0 {
		return 0
	}
	last := state.Messages[len(state.Messages)-1]
	lastText := assistantText(last)
	if mode == "json" {
		data, _ := json.MarshalIndent(last, "", "  ")
		fmt.Fprintln(stdout, string(data))
	} else if lastText != "" {
		fmt.Fprintln(stdout, lastText)
	} else {
		data, _ := json.MarshalIndent(last, "", "  ")
		fmt.Fprintln(stdout, string(data))
	}
	if permissionMode == permission.ModePlan {
		collector := permission.NewPlanCollector()
		collector.Observe(lastText)
		if formatted := permission.FormatPlan(collector.Items()); formatted != "" {
			fmt.Fprintln(stderr, "Collected plan:")
			fmt.Fprintln(stderr, formatted)
		}
	}
	return 0
}

// parseCommandOptions tracks explicitly passed settings separately so config resolution can preserve precedence.
func parseCommandOptions(args []string, stderr io.Writer) (commandOptions, error) {
	var values config.FlagValues
	fs := flag.NewFlagSet("harness", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&values.Model.Value, "model", "", "model ID")
	fs.StringVar(&values.PlanModel.Value, "plan-model", "", "model ID for the read-only plan agent")
	fs.StringVar(&values.SmolModel.Value, "smol-model", "", "model ID for the small helper agent")
	fs.IntVar(&values.MaxTokens.Value, "max-tokens", 0, "maximum model output tokens")
	fs.StringVar(&values.PermissionMode.Value, "permission-mode", "", "tool permission mode: default, plan, acceptEdits, bypass")
	fs.StringVar(&values.LogLevel.Value, "log-level", "", "log level: debug, info, warn, error")
	fs.StringVar(&values.LogFile.Value, "log-file", "", "diagnostic log file")
	fs.IntVar(&values.LogFileMaxBytes.Value, "log-file-max-bytes", 0, "diagnostic log rotation size in bytes")
	fs.BoolVar(&values.EnableTask.Value, "enable-task", false, "enable the delegated task tool with explore and general subagents")
	fs.BoolVar(&values.Plan.Value, "plan", false, "run the read-only plan stack")
	fs.Var(skillPathsFlag{input: &values.Attach}, "attach", "file to attach to the session (repeatable)")
	fs.BoolVar(&values.EnableWeb.Value, "enable-web", false, "enable gated web_search and web_fetch tools")
	fs.StringVar(&values.AgentDir.Value, "agent-dir", "", "agent data directory")
	fs.StringVar(&values.SessionDir.Value, "session-dir", "", "session data directory")
	fs.StringVar(&values.AnthropicBaseURL.Value, "anthropic-base-url", "", "Anthropic-compatible base URL")
	fs.StringVar(&values.OpenAIBaseURL.Value, "openai-base-url", "", "OpenAI-compatible base URL")
	fs.StringVar(&values.AnthropicAPIKey.Value, "anthropic-api-key", "", "Anthropic API key")
	fs.StringVar(&values.AnthropicAuthToken.Value, "anthropic-auth-token", "", "authorization bearer token for an Anthropic-compatible proxy")
	fs.StringVar(&values.FauxScript.Value, "faux-script", "", "path to a hermetic faux-provider response script")
	fs.Var(skillPathsFlag{input: &values.SkillPaths}, "skill", "additional skill path (repeatable)")
	fs.Var(skillPathsFlag{input: &values.PermissionAllowTools}, "allow-tool", "tool to always allow (repeatable)")
	fs.Var(skillPathsFlag{input: &values.PermissionDenyTools}, "deny-tool", "tool to always deny (repeatable)")
	var prompt, printMode, outputFormat, excludeTools string
	var version bool
	var resume string
	var continueRecent bool
	fs.StringVar(&prompt, "p", "", "single prompt to run (non-interactive)")
	fs.StringVar(&printMode, "print", "text", "legacy output mode alias: text, json")
	fs.StringVar(&outputFormat, "output-format", "", "output mode: text, json")
	fs.StringVar(&excludeTools, "exclude-tools", "", "comma-separated tool names to exclude")
	fs.BoolVar(&version, "version", false, "print build version")
	fs.StringVar(&resume, "resume", "", "resume a session by id")
	fs.BoolVar(&continueRecent, "continue", false, "continue the most recent session for this directory")
	if err := fs.Parse(args); err != nil {
		return commandOptions{}, err
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "model":
			values.Model.Set = true
		case "plan-model":
			values.PlanModel.Set = true
		case "smol-model":
			values.SmolModel.Set = true
		case "max-tokens":
			values.MaxTokens.Set = true
		case "permission-mode":
			values.PermissionMode.Set = true
		case "log-level":
			values.LogLevel.Set = true
		case "log-file":
			values.LogFile.Set = true
		case "log-file-max-bytes":
			values.LogFileMaxBytes.Set = true
		case "enable-task":
			values.EnableTask.Set = true
		case "plan":
			values.Plan.Set = true
		case "attach":
			values.Attach.Set = true
		case "enable-web":
			values.EnableWeb.Set = true
		case "web-search-url":
			values.WebSearchURL.Set = true
		case "agent-dir":
			values.AgentDir.Set = true
		case "session-dir":
			values.SessionDir.Set = true
		case "anthropic-base-url":
			values.AnthropicBaseURL.Set = true
		case "openai-base-url":
			values.OpenAIBaseURL.Set = true
		case "anthropic-api-key":
			values.AnthropicAPIKey.Set = true
		case "anthropic-auth-token":
			values.AnthropicAuthToken.Set = true
		case "faux-script":
			values.FauxScript.Set = true
		}
	})
	secrets := secretResolver(values)
	return commandOptions{flags: values, Secrets: secrets, Prompt: prompt, PrintMode: printMode, OutputFormat: outputFormat, ExcludeTools: excludeTools, Version: version, Resume: resume, Continue: continueRecent}, nil
}

// resolveCommandConfig rejects incompatible session flags before resolving the config's precedence layers.
func resolveCommandConfig(opts *commandOptions) error {
	if opts.Resume != "" && opts.Continue {
		return fmt.Errorf("-resume and -continue cannot be used together")
	}
	resolved, err := config.Config{Flags: opts.flags}.Resolve()
	if err != nil {
		return err
	}
	opts.Config = resolved
	return nil
}

// secretResolver gives model routing the CLI's flag-first, environment-second credential lookup.
func secretResolver(values config.FlagValues) config.SecretResolver {
	return config.SecretResolverFunc(func(name string) (string, bool) {
		var value string
		switch name {
		case "ANTHROPIC_API_KEY":
			value = values.AnthropicAPIKey.Value
			if value == "" {
				value = os.Getenv("ANTHROPIC_OAUTH_TOKEN")
			}
			if value == "" {
				value = os.Getenv("ANTHROPIC_API_KEY")
			}
		case "ANTHROPIC_AUTH_TOKEN":
			value = values.AnthropicAuthToken.Value
			if value == "" {
				value = os.Getenv("ANTHROPIC_AUTH_TOKEN")
			}
		case "OPENAI_API_KEY":
			value = os.Getenv("OPENAI_API_KEY")
		case "GEMINI_API_KEY":
			value = os.Getenv("GEMINI_API_KEY")
		case "GOOGLE_API_KEY":
			value = os.Getenv("GOOGLE_API_KEY")
		}
		value = strings.TrimSpace(value)
		return value, value != ""
	})
}

// providerCredentialPresent reports whether the selected provider can find one of its supported credentials.
func providerCredentialPresent(provider string, secrets config.SecretResolver) bool {
	if secrets == nil {
		return false
	}
	switch provider {
	case "openai":
		_, present := secrets.Resolve("OPENAI_API_KEY")
		return present
	case "openrouter":
		_, present := secrets.Resolve("OPENROUTER_API_KEY")
		return present
	case "google":
		_, gemini := secrets.Resolve("GEMINI_API_KEY")
		_, googleKey := secrets.Resolve("GOOGLE_API_KEY")
		return gemini || googleKey
	default:
		_, apiKey := secrets.Resolve("ANTHROPIC_API_KEY")
		_, authToken := secrets.Resolve("ANTHROPIC_AUTH_TOKEN")
		return apiKey || authToken
	}
}

// resolveOutputFormat preserves -print as a fallback while letting -output-format win.
func resolveOutputFormat(outputFormat, printMode string) (string, error) {
	mode := strings.TrimSpace(outputFormat)
	if mode == "" {
		mode = strings.TrimSpace(printMode)
	}
	if mode == "" {
		mode = "text"
	}
	switch mode {
	case "text", "json":
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported output format %q (use text or json)", mode)
	}
}

func splitCSV(s string) []string {
	var result []string
	for _, value := range strings.Split(s, ",") {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func assistantText(message ptypes.Message) string { return session.AssistantMessageText(message) }

// buildPermissionPolicy converts allow and deny tool lists into a permission.Policy.
// A tool named in both lists is denied (deny wins). The one-shot CLI is headless, so
// the policy carries no Prompter and prompt decisions resolve to denials.
func buildPermissionPolicy(allow, deny []string) permission.Policy {
	overrides := make(map[string]permission.Decision)
	for _, name := range allow {
		overrides[name] = permission.DecideAllow
	}
	for _, name := range deny {
		overrides[name] = permission.DecideDeny
	}
	if len(overrides) == 0 {
		return permission.Policy{}
	}
	return permission.Policy{Overrides: overrides}
}

// openSession resolves or creates the session journal and returns the store plus
// the messages to seed the agent (nil for a fresh session). A -resume id or
// -continue reopens an existing journal for appending; otherwise a new session is
// created and its model recorded.
func openSession(ctx context.Context, sessionDir, cwd, resumeID string, continueRecent bool, model ptypes.Model) (*journal.Store, []ptypes.Message, error) {
	switch {
	case resumeID != "":
		path, err := journal.Resolve(ctx, sessionDir, cwd, resumeID)
		if err != nil {
			return nil, nil, err
		}
		return openResumedSession(ctx, path, model)
	case continueRecent:
		path, err := journal.Recent(ctx, sessionDir, cwd)
		if err != nil {
			return nil, nil, err
		}
		return openResumedSession(ctx, path, model)
	default:
		store, err := journal.Create(ctx, sessionDir, cwd, journal.Options{})
		if err != nil {
			return nil, nil, err
		}
		if err := store.AppendModelChange(ctx, model.ID, "default"); err != nil {
			_ = store.Close()
			return nil, nil, fmt.Errorf("record session model: %w", err)
		}
		return store, nil, nil
	}
}

// openResumedSession records a model change when a resumed journal no longer matches today's default model or role.
func openResumedSession(ctx context.Context, path string, model ptypes.Model) (*journal.Store, []ptypes.Message, error) {
	store, loaded, err := journal.OpenForResume(ctx, path, journal.Options{})
	if err != nil {
		return nil, nil, err
	}
	role := loaded.Role
	if role == "" {
		role = "default"
	}
	if loaded.Model != model.ID || role != "default" {
		if err := store.AppendModelChange(ctx, model.ID, "default"); err != nil {
			_ = store.Close()
			return nil, nil, fmt.Errorf("record resumed session model: %w", err)
		}
	}
	return store, loaded.Messages, nil
}

// persistNewMessages appends the messages produced after the seeded prefix. A
// persistence failure is logged, not fatal: losing the journal must not fail a
// run that already completed.
func persistNewMessages(ctx context.Context, store *journal.Store, messages, seed []ptypes.Message) {
	if len(messages) < len(seed) {
		slog.Warn("session not persisted: history diverged from the resume point (mid-run compaction)")
		return
	}
	for index := range seed {
		if !reflect.DeepEqual(messages[index], seed[index]) {
			slog.Warn("session not persisted: history diverged from the resume point (mid-run compaction)")
			return
		}
	}
	for _, message := range messages[len(seed):] {
		if err := store.AppendMessage(ctx, message); err != nil {
			slog.Warn("session persistence failed", "error", err)
			return
		}
	}
}

// setupLogging installs the redacting default logger and optionally tees diagnostics into the rotating log file.
func setupLogging(levelValue, path string, maxBytes int64) {
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(levelValue)) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	newLogger := func(w io.Writer) *slog.Logger {
		return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level, ReplaceAttr: redactAttr}))
	}
	slog.SetDefault(newLogger(os.Stderr))
	if path != "" {
		if rw, err := newRotatingWriter(path, maxBytes); err != nil {
			slog.Warn("could not open diagnostic log file; logging to stderr only", "path", path, "error", err)
		} else {
			slog.SetDefault(newLogger(io.MultiWriter(os.Stderr, rw)))
		}
	}
}
