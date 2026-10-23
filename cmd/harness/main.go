// Command harness builds the tempest binary: it runs one prompt or serves durable agent sessions.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"go.harness.dev/harness/internal/adapter"
	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/discovery"
	"go.harness.dev/harness/internal/document"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/mcpclient"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/retry"
	"go.harness.dev/harness/internal/session"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/tools"
	"go.harness.dev/harness/tempest"
)

// exitAborted is the conventional exit status for a run ended by SIGINT.
const exitAborted = 130

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
	Budget       agent.Budget
}

// skillPathsFlag adapts repeated -skill values to config's tracked input, so resolution can still tell flags from defaults.
type skillPathsFlag struct {
	input *config.StringsInput
}

func (f skillPathsFlag) String() string {
	if f.input == nil {
		return ""
	}
	return strings.Join(f.input.Values, ",")
}

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
		case "serve":
			return runServe(args[1:], stdout, stderr)
		case "mcp":
			return runMCP(args[1:], stdout, stderr)
		case "doctor":
			return runDoctor(args[1:], stdout, stderr)
		case "version":
			if len(args) > 1 {
				fmt.Fprintln(stderr, unexpectedArgument("version", args[1]))
				return exitUsage
			}
			return runVersion(stdout)
		}
	}
	return runOneShot(args, stdout, stderr)
}

// runOneShot wires resolved CLI config into one non-interactive agent run, including discovery and optional session continuity.
func runOneShot(args []string, stdout, stderr io.Writer) int {
	opts, err := parseCommandOptions("", args, stderr)
	if err != nil {
		return parseExitCode(err)
	}
	if opts.Version {
		return runVersion(stdout)
	}
	mode, err := resolveOutputFormat(opts.OutputFormat, opts.PrintMode)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if mode == "stream-json" {
		// Let the encoder report EPIPE so it can cancel and join the durable run.
		signal.Ignore(syscall.SIGPIPE)
		defer signal.Reset(syscall.SIGPIPE)
	}
	if err := resolveCommandConfig(&opts); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	setupLogging(opts.Config.LogLevel(), opts.Config.LogFile(), opts.Config.LogFileMaxBytes())
	if opts.Prompt == "" {
		fmt.Fprintln(stderr, "usage: tempest -p <prompt> [-model <id>] [-output-format text|json|stream-json] [-max-turns N] [-max-cost USD] [-max-wall-time DURATION] (tempest -help lists flags and commands)")
		return 1
	}
	permissionMode, err := permission.ParseMode(opts.Config.PermissionModeDefault())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	permissionPolicy := toolPolicy(stderr, opts.Config)
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
		planSelected = fauxModel
		stackModel = fauxModel
		streamFn = fauxProvider.StreamSimple
		stackStreamFn = streamFn
	} else if !providerCredentialPresent(selectedModel.Provider, opts.Secrets) {
		fmt.Fprintf(stderr, "warning: no %s credential resolved; model calls will fail until one is present\n", selectedModel.Provider)
	}
	if opts.Config.Plan() && opts.Config.FauxScript() == "" {
		// The plan flag routes the whole run through the read-only stack on the
		// plan role, not just the plan-agent model.
		stackModel = roles.Plan.Model
		stackStreamFn = roles.Plan.StreamFn
	}
	// Refuse an unenforceable budget before anything is written to disk.
	if err := opts.Budget.Validate(stackModel); err != nil {
		fmt.Fprintf(stderr, "budget error: %v\n", err)
		return 1
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "cwd error: %v\n", err)
		return 1
	}
	// SIGINT/SIGTERM abort the run cleanly: tools are cancelled and joined and
	// the journal records the aborted run. The first signal then restores the
	// default handling, so a second one kills a run stuck in a tool that
	// ignores cancellation.
	runCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	context.AfterFunc(runCtx, stopSignals)
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

	var attachRegistry *document.AttachmentRegistry
	var attachReader *document.CacheRootBlobReader
	if attachPaths := opts.Config.AttachPaths(); len(attachPaths) > 0 {
		attachRegistry, attachReader, err = session.ResolveLocalAttachments(runCtx, attachPaths, filepath.Join(opts.Config.SessionDir(), "attachments"))
		if err != nil {
			fmt.Fprintf(stderr, "attach error: %v\n", err)
			return 1
		}
	}
	stack := session.StackConfigFromConfig(opts.Config)
	stack.Cwd = cwd
	stack.Model = stackModel
	stack.PlanModel = planSelected
	stack.SmolModel = roles.Smol.Model
	stack.StreamFn = stackStreamFn
	stack.SmolStreamFn = roles.Smol.StreamFn
	stack.PermissionMode = permissionMode
	stack.PermissionPolicy = permissionPolicy
	stack.ExcludeTools = splitCSV(opts.ExcludeTools)
	stack.ContextFiles = inputs.ContextFiles
	stack.Skills = inputs.Skills
	stack.GenericRules = inputs.GenericRules
	stack.AttachmentRegistry = attachRegistry
	stack.AttachmentReader = attachReader
	sess, err := openSession(runCtx, tempest.Options{
		SessionDir: opts.Config.SessionDir(),
		Cwd:        cwd,
		Stack:      &stack,
		New: &tempest.NewSession{
			Model:          selectedModel.ID,
			Budget:         opts.Budget,
			PermissionMode: permissionMode,
			MCPServers:     opts.Config.MCPServers(),
		},
	}, opts.Resume, opts.Continue)
	if err != nil {
		fmt.Fprintf(stderr, "session error: %v\n", err)
		if hint := mcpStartupHint(err); hint != "" {
			fmt.Fprintln(stderr, hint)
		}
		return 1
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			slog.Warn("session close failed", "error", err)
		}
	}()
	outcome, outputErr := executeOneShot(runCtx, sess, opts.Prompt, mode, stdout, stderr, 256)
	if outputErr != nil {
		fmt.Fprintf(stderr, "output error: %v\n", outputErr)
		return 1
	}
	if permissionMode == permission.ModePlan {
		collector := permission.NewPlanCollector()
		collector.Observe(outcome.LastAssistantText)
		if formatted := permission.FormatPlan(collector.Items()); formatted != "" {
			fmt.Fprintln(stderr, "Collected plan:")
			fmt.Fprintln(stderr, formatted)
		}
	}
	if mode == "text" {
		fmt.Fprintf(stderr, "session: %s (resume with -resume %s)\n", sess.ID(), sess.ID())
	}
	switch outcome.StopReason {
	case agent.StopError:
		return 1
	case agent.StopAborted:
		return exitAborted
	default:
		return 0
	}
}

type runOutcome struct {
	tempest.RunResult
	SessionID         string
	Elapsed           time.Duration
	LastAssistantText string
}

// runReport is the -output-format json result of one run.
type runReport struct {
	SessionID  string           `json:"session_id"`
	StopReason agent.StopReason `json:"stop_reason"`
	Error      string           `json:"error,omitempty"`
	Turns      int              `json:"turns"`
	Usage      ptypes.Usage     `json:"usage"`
	// CostUSD is null when any call's price was unknown: unknown is not free.
	CostUSD   *float64 `json:"cost_usd"`
	ElapsedMS int64    `json:"elapsed_ms"`
	// Text is the run's last assistant text; Message is the transcript's
	// last message (a tool result when a budget stopped the run mid-task).
	Text    string         `json:"text"`
	Message ptypes.Message `json:"message,omitempty"`
}

// writeRunOutput prints the run result: a runReport in json mode; otherwise
// the last assistant text on stdout and, when the run did not complete, why
// it stopped on stderr.
func writeRunOutput(stdout, stderr io.Writer, mode string, outcome runOutcome, messages []ptypes.Message) {
	var last ptypes.Message
	if len(messages) > 0 {
		last = messages[len(messages)-1]
	}
	if mode == "json" {
		report := makeRunReport(outcome, last)
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "encode run report: %v\n", err)
			return
		}
		fmt.Fprintln(stdout, string(data))
		return
	}
	if outcome.LastAssistantText != "" {
		fmt.Fprintln(stdout, outcome.LastAssistantText)
	}
	switch outcome.StopReason {
	case agent.StopCompleted:
	case agent.StopError:
		fmt.Fprintf(stderr, "run failed after %d turns: %v\n", outcome.Turns, outcome.Err)
	default:
		fmt.Fprintf(stderr, "run stopped: %s after %d turns\n", outcome.StopReason, outcome.Turns)
	}
}

func makeRunReport(outcome runOutcome, last ptypes.Message) runReport {
	report := runReport{SessionID: outcome.SessionID, StopReason: outcome.StopReason, Turns: outcome.Turns, Usage: outcome.Usage, ElapsedMS: outcome.Elapsed.Milliseconds(), Text: outcome.LastAssistantText, Message: last}
	if outcome.Err != nil {
		report.Error = outcome.Err.Error()
	}
	if !outcome.Usage.Cost.Unknown {
		cost := outcome.Usage.Cost.Total
		report.CostUSD = &cost
	}
	return report
}

// parseCommandOptions tracks explicitly passed settings separately so config resolution can preserve precedence.
// command selects the -help text: "" for the one-shot CLI, or a subcommand name.
func parseCommandOptions(command string, args []string, stderr io.Writer) (commandOptions, error) {
	var values config.FlagValues
	fs := flag.NewFlagSet(strings.TrimSpace("tempest "+command), flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		if command == "doctor" {
			fmt.Fprint(fs.Output(), doctorUsage)
		} else {
			fmt.Fprint(fs.Output(), topUsage)
		}
		fs.PrintDefaults()
	}
	fs.StringVar(&values.Model.Value, "model", "", "model ID")
	fs.StringVar(&values.PlanModel.Value, "plan-model", "", "model ID for the read-only plan agent")
	fs.StringVar(&values.SmolModel.Value, "smol-model", "", "model ID for the small helper agent")
	fs.IntVar(&values.MaxTokens.Value, "max-tokens", 0, "maximum model output tokens")
	fs.StringVar(&values.PermissionMode.Value, "permission-mode", "", "tool permission mode: default, plan, acceptEdits, bypass")
	fs.StringVar(&values.BashTimeout.Value, "bash-timeout", "", "default bash command timeout, e.g. 10m (default 10m)")
	fs.StringVar(&values.LogLevel.Value, "log-level", "", "log level: debug, info, warn, error")
	fs.StringVar(&values.LogFile.Value, "log-file", "", "diagnostic log file")
	fs.IntVar(&values.LogFileMaxBytes.Value, "log-file-max-bytes", 0, "diagnostic log rotation size in bytes")
	fs.BoolVar(&values.EnableTask.Value, "enable-task", false, "enable the delegated task tool with explore and general subagents")
	fs.BoolVar(&values.Plan.Value, "plan", false, "run the read-only plan stack")
	fs.Var(skillPathsFlag{input: &values.Attach}, "attach", "file to attach to the session (repeatable)")
	fs.BoolVar(&values.EnableWeb.Value, "enable-web", false, "enable gated web_search and web_fetch tools")
	fs.StringVar(&values.AgentDir.Value, "agent-dir", "", "agent data directory")
	fs.StringVar(&values.SessionDir.Value, "session-dir", "", "session data directory")
	fs.StringVar(&values.MCPConfig.Value, "mcp-config", "", "operator-selected MCP server JSON file (empty disables MCP)")
	addIntelligenceFlags(fs, &values)
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
	fs.StringVar(&printMode, "print", "text", "legacy output mode alias: text, json, stream-json")
	fs.StringVar(&outputFormat, "output-format", "", "output mode: text, json, stream-json")
	fs.StringVar(&excludeTools, "exclude-tools", "", "comma-separated tool names to exclude")
	fs.BoolVar(&version, "version", false, "print build version")
	fs.StringVar(&resume, "resume", "", "resume a session by id")
	fs.BoolVar(&continueRecent, "continue", false, "continue the most recent session for this directory")
	var budget agent.Budget
	fs.IntVar(&budget.MaxTurns, "max-turns", 0, "stop the run after this many model calls (0 = unlimited)")
	fs.Float64Var(&budget.MaxCostUSD, "max-cost", 0, "stop the run once its model calls cost this many USD (0 = unlimited; refused when the model's price is unknown)")
	fs.DurationVar(&budget.MaxWallTime, "max-wall-time", 0, "stop the run at the first turn boundary after this much time, e.g. 10m (0 = unlimited)")
	if err := fs.Parse(args); err != nil {
		return commandOptions{}, usageFailure(err)
	}
	if fs.NArg() > 0 {
		err := unexpectedArgument(command, fs.Arg(0))
		fmt.Fprintln(stderr, err)
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
		case "bash-timeout":
			values.BashTimeout.Set = true
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
		case "mcp-config":
			values.MCPConfig.Set = true
		case "enable-lsp", "lsp-config", "cloud-policy":
			markIntelligenceFlag(f.Name, &values)
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
	return commandOptions{flags: values, Secrets: secrets, Prompt: prompt, PrintMode: printMode, OutputFormat: outputFormat, ExcludeTools: excludeTools, Version: version, Resume: resume, Continue: continueRecent, Budget: budget}, nil
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
		case "OPENROUTER_API_KEY":
			value = os.Getenv("OPENROUTER_API_KEY")
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
	case "text", "json", "stream-json":
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported output format %q (use text, json or stream-json)", mode)
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

// openSession keeps -continue scoped to the CLI's working directory. A resumed
// session gets a new prompt, just as it does without -resume.
func openSession(ctx context.Context, options tempest.Options, resumeID string, continueRecent bool) (*tempest.Session, error) {
	if resumeID != "" || continueRecent {
		resume := &tempest.ResumeSession{
			SessionID:      resumeID,
			Model:          options.New.Model,
			Budget:         options.New.Budget,
			PermissionMode: options.New.PermissionMode,
			MCPServers:     options.New.MCPServers,
		}
		if continueRecent {
			path, err := journal.Recent(ctx, options.SessionDir, options.Cwd)
			if err != nil {
				return nil, err
			}
			resume.Path = path
		}
		options.New = nil
		options.Resume = resume
	}
	return tempest.Open(ctx, options)
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

const topUsage = `Usage:
  tempest -p <prompt> [flags]   run one prompt non-interactively
  tempest <command> [flags]

Commands:
  serve     serve durable sessions over ACP on stdio, or ACP WebSocket and A2A with -listen
  mcp       run as a stdio MCP server that exposes the tempest_run tool
  doctor    check configuration and credentials offline; --ping probes the configured models
  version   print build version

Run 'tempest <command> -help' for that command's flags.

Flags:
`

const doctorUsage = `Usage: tempest doctor [--ping] [-models] [flags]

Prints the resolved configuration and credential checks without network access.
  --ping    send one 1-token request to each configured model (default, plan, smol); billable
  -models   also list the embedded model catalog with prices

Flags:
`

// exitUsage is the exit status for a malformed command line (an unknown flag,
// a bad flag value or a stray argument), as with Go's flag package.
const exitUsage = 2

// usageError is a command-line mistake. Its message is already on stderr
// when it reaches a caller, which only turns it into exitUsage.
type usageError struct{ message string }

func (e usageError) Error() string { return e.message }

// usageFailure classifies a flag-set parse error. -help is not a mistake and
// keeps its own error; the flag package has already printed either one.
func usageFailure(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return err
	}
	return usageError{message: err.Error()}
}

// parseExitCode keeps -help at status 1, as before, and gives every real
// command-line mistake exitUsage so scripts can tell it from a failed run.
func parseExitCode(err error) int {
	if errors.As(err, new(usageError)) {
		return exitUsage
	}
	return 1
}

// unexpectedArgument names a positional argument the command doesn't take.
// Go's flag parsing stops at the first non-flag, so in `tempest run -p hi`
// the stray word is "run" and -p was never read.
func unexpectedArgument(command, arg string) error {
	switch command {
	case "":
		return usageError{message: fmt.Sprintf("tempest: unexpected argument %q; pass the prompt with -p \"<prompt>\" or start with a command (serve, mcp, doctor, version); see 'tempest -help'", arg)}
	case "version":
		return usageError{message: fmt.Sprintf("tempest version: unexpected argument %q; version takes no arguments", arg)}
	default:
		return usageError{message: fmt.Sprintf("tempest %s: unexpected argument %q; %s takes flags only; see 'tempest %s -help'", command, arg, command, command)}
	}
}

// toolPolicy builds the configured allow/deny policy and warns on stderr about
// names that match no tool. Every command that accepts -allow-tool or
// -deny-tool builds its policy here, so none of them drops a typo silently.
func toolPolicy(stderr io.Writer, cfg *config.ResolvedConfig) permission.Policy {
	allow, deny := cfg.PermissionAllowTools(), cfg.PermissionDenyTools()
	warnUnknownToolNames(stderr, "-allow-tool", allow)
	warnUnknownToolNames(stderr, "-deny-tool", deny)
	policy := buildPermissionPolicy(allow, deny)
	policy.CloudRules = cfg.CloudRules()
	return policy
}

// addIntelligenceFlags registers the language-server and cloud policy
// flags every session-creating command accepts.
func addIntelligenceFlags(fs *flag.FlagSet, values *config.FlagValues) {
	fs.BoolVar(&values.EnableLSP.Value, "enable-lsp", false, "enable the lsp_* tools; language servers are trusted executables started on first use")
	fs.StringVar(&values.LSPConfig.Value, "lsp-config", "", `operator language-server JSON file {"servers":[...]} (empty selects the built-in gopls, yaml-language-server, terraform-ls and helm_ls definitions)`)
	fs.StringVar(&values.CloudPolicy.Value, "cloud-policy", "", `operator kube-context policy JSON file {"rules":[{"tool":"kubectl","context":"dev","action":"allow_write"}]}`)
}

// markIntelligenceFlag records that an intelligence flag was set, so an
// explicit -enable-lsp=false or empty file outranks the environment.
func markIntelligenceFlag(name string, values *config.FlagValues) {
	switch name {
	case "enable-lsp":
		values.EnableLSP.Set = true
	case "lsp-config":
		values.LSPConfig.Set = true
	case "cloud-policy":
		values.CloudPolicy.Set = true
	}
}

// warnUnknownToolNames flags allow/deny entries that match no built-in tool.
// MCP tools are discovered after startup, so mcp__ names are never warned about.
func warnUnknownToolNames(stderr io.Writer, flagName string, names []string) {
	for _, name := range names {
		if tools.AllToolNames[tools.ToolName(name)] || strings.HasPrefix(name, "mcp__") {
			continue
		}
		known := make([]string, 0, len(tools.AllToolNames))
		for tool := range tools.AllToolNames {
			known = append(known, string(tool))
		}
		slices.Sort(known)
		fmt.Fprintf(stderr, "warning: %s %q matches no built-in tool (known: %s; MCP tools are named mcp__<server>__<tool>)\n", flagName, name, strings.Join(known, ", "))
	}
}

// mcpStartupHint tells the operator how to get past a server that failed to
// start; per 05-interop.md a startup failure aborts the session on purpose.
func mcpStartupHint(err error) string {
	var startup *mcpclient.StartupError
	if !errors.As(err, &startup) {
		return ""
	}
	return fmt.Sprintf(`hint: MCP server %q failed to start, so no session was opened. Fix its entry, remove it or set "disabled": true on it in the -mcp-config file, or pass -mcp-config "" to run without MCP.`, startup.Server)
}
