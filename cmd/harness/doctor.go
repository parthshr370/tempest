package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"go.harness.dev/harness/internal/adapter"
	"go.harness.dev/harness/internal/catalog"
	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hooks"
	"go.harness.dev/harness/internal/obs"
	"go.harness.dev/harness/internal/retry"
	"go.harness.dev/harness/internal/tools"
)

// runDoctor stays offline unless --ping explicitly requests billable provider probes.
func runDoctor(args []string, stdout, stderr io.Writer) int {
	ping, listModels := false, false
	filtered := make([]string, 0, len(args))
	for _, arg := range args {
		switch arg {
		case "--ping", "-ping":
			ping = true
		case "--models", "-models":
			listModels = true
		default:
			filtered = append(filtered, arg)
		}
	}
	args = filtered
	opts, err := parseCommandOptions("doctor", args, stderr)
	if err != nil {
		return parseExitCode(err)
	}
	if err := resolveCommandConfig(&opts); err != nil {
		fmt.Fprintf(stderr, "doctor configuration error: %v\n", err)
		return 1
	}
	roles, err := adapter.ResolveRoles(adapter.RoleRoutingConfig{
		DefaultModel:       opts.Config.Model(),
		PlanModel:          opts.Config.PlanModel(),
		SmolModel:          opts.Config.SmolModel(),
		AnthropicBaseURL:   opts.Config.AnthropicBaseURL(),
		OpenAIBaseURL:      opts.Config.OpenAIBaseURL(),
		MaxTokens:          opts.Config.MaxTokens(),
		SecretLookup:       opts.Secrets.Resolve,
		Providers:          opts.Config.Providers(),
		ChatGPTCredentials: adapter.OpenAIChatGPTCredentials(""),
	})
	if err != nil {
		fmt.Fprintf(stderr, "doctor configuration error: %v\n", err)
		return 1
	}
	providerRegistry, _ := adapter.Providers(opts.Config.Providers())
	var credentials []string
	for _, provider := range providerRegistry {
		credentials = append(credentials, provider.APIKey, provider.Credential(opts.Secrets.Resolve))
		for key, value := range provider.Headers {
			credentials = append(credentials, value)
			if strings.EqualFold(key, "authorization") || strings.EqualFold(key, provider.AuthHeader) {
				prefix := "Bearer "
				if provider.AuthPrefix != nil {
					prefix = *provider.AuthPrefix
				}
				if prefix != "" {
					credentials = append(credentials, strings.TrimPrefix(value, prefix))
				}
			}
		}
		for _, value := range provider.Query {
			credentials = append(credentials, value)
		}
	}
	for _, name := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_OAUTH_TOKEN"} {
		if value, ok := opts.Secrets.Resolve(name); ok {
			credentials = append(credentials, value)
		}
	}
	stdout = obs.RedactingWriter{Writer: stdout, Values: credentials}
	stderr = obs.RedactingWriter{Writer: stderr, Values: credentials}
	fmt.Fprintln(stdout, "resolved config:")
	fmt.Fprintf(stdout, "  model: %s\n", opts.Config.Model())
	fmt.Fprintf(stdout, "  plan_model: %s\n", roleSpec(opts.Config.PlanModel(), opts.Config.Model()))
	fmt.Fprintf(stdout, "  smol_model: %s\n", roleSpec(opts.Config.SmolModel(), opts.Config.Model()))
	fmt.Fprintf(stdout, "  reasoning: %s\n", reasoningSetting(opts.Config.Reasoning()))
	fmt.Fprintf(stdout, "  plan_reasoning: %s\n", reasoningSetting(opts.Config.PlanReasoning()))
	fmt.Fprintf(stdout, "  smol_reasoning: %s\n", reasoningSetting(opts.Config.SmolReasoning()))
	fmt.Fprintf(stdout, "  max_tokens: %d\n", opts.Config.MaxTokens())
	fmt.Fprintf(stdout, "  permission_mode_default: %s\n", opts.Config.PermissionModeDefault())
	fmt.Fprintf(stdout, "  log_level: %s\n", opts.Config.LogLevel())
	fmt.Fprintf(stdout, "  log_file: %s\n", opts.Config.LogFile())
	fmt.Fprintf(stdout, "  enable_web: %t\n", opts.Config.EnableWeb())
	fmt.Fprintf(stdout, "  web_search_url: %s\n", redactedURL(opts.Config.WebSearchURL()))
	fmt.Fprintf(stdout, "  anthropic_base_url: %s\n", redactedURL(opts.Config.AnthropicBaseURL()))
	fmt.Fprintf(stdout, "  openai_base_url: %s\n", redactedURL(opts.Config.OpenAIBaseURL()))
	fmt.Fprintln(stdout, "  provider_credentials: REDACTED")
	fmt.Fprintln(stdout, "models:")
	printRole(stdout, "default", roles.Default, opts.Config.Reasoning())
	printRole(stdout, "plan", roles.Plan, opts.Config.PlanReasoning())
	printRole(stdout, "smol", roles.Smol, opts.Config.SmolReasoning())
	fmt.Fprintln(stdout, "providers:")
	providers, err := adapter.Providers(opts.Config.Providers())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	for _, name := range slices.Sorted(maps.Keys(providers)) {
		p := providers[name]
		if name == "openai-chatgpt" {
			printChatGPTDoctor(stdout)
			continue
		}
		noAuth := p.NoAuth != nil && *p.NoAuth
		p.NoAuth = nil
		fmt.Fprintf(stdout, "  %s: api=%s protocol=%s key=%s present=%s auth_required=%s base_url=%s\n", name, p.API(), p.Protocol, p.APIKeyEnv, yesNo(p.CredentialPresent(opts.Secrets.Resolve)), yesNo(!noAuth), redactedURL(p.BaseURL))
	}
	for _, path := range opts.Config.UntrustedProviderFiles() {
		fmt.Fprintf(stdout, "  UNTRUSTED %s (providers not loaded; use -trust-project-providers)\n", path)
	}
	if listModels {
		fmt.Fprintf(stdout, "models: (pricing as of %s, USD per million tokens)\n", catalog.AsOf())
		for _, m := range catalog.Models() {
			if m.Cost.Unknown {
				fmt.Fprintf(stdout, "  %s/%s: window=%d max_tokens=%d cost=unknown\n", m.Provider, m.ID, m.ContextWindow, m.MaxTokens)
			} else {
				fmt.Fprintf(stdout, "  %s/%s: window=%d max_tokens=%d cost={in=%.2f out=%.2f cache_read=%.2f cache_write=%.2f}\n",
					m.Provider, m.ID, m.ContextWindow, m.MaxTokens, m.Cost.Input, m.Cost.Output, m.Cost.CacheRead, m.Cost.CacheWrite)
			}
		}
	} else {
		fmt.Fprintf(stdout, "catalog: %d models (pricing as of %s); list them with 'tempest doctor -models'\n", len(catalog.Models()), catalog.AsOf())
	}
	fmt.Fprintln(stdout, "path checks:")
	fmt.Fprintf(stdout, "  config: %s\n", opts.Config.SettingsPath())
	fmt.Fprintf(stdout, "  agent: %s\n", opts.Config.AgentDir())
	fmt.Fprintf(stdout, "  session: %s\n", opts.Config.SessionDir())
	if gitPath, err := exec.LookPath("git"); err == nil {
		fmt.Fprintf(stdout, "PASS checkpoint git: %s\n", gitPath)
	} else {
		fmt.Fprintln(stdout, "WARN checkpoint git: unavailable; tool calls still run, but checkpoint capture and rewind need git")
	}
	printCodingToolChecks(stdout)

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	hookSettings, skippedHooks, err := hooks.ResolveProject(opts.Config.Hooks(), cwd)
	if err != nil {
		fmt.Fprintf(stderr, "doctor hooks: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "hooks: disabled=%t\n", hookSettings.NoHooks)
	for _, event := range hooks.Events {
		for _, group := range hookSettings.Hooks[event] {
			for _, command := range group.Hooks {
				timeout := command.Timeout
				if timeout == 0 {
					timeout = hooks.DefaultTimeout(event).Seconds()
				}
				fmt.Fprintf(stdout, "  %s matcher=%q timeout=%gs command=%s\n", event, group.Matcher, timeout, command.Command)
			}
		}
	}
	var toolNames []string
	for name := range tools.AllToolNames {
		toolNames = append(toolNames, string(name))
	}
	for _, warning := range hookSettings.UnmatchedToolMatchers(toolNames) {
		fmt.Fprintf(stdout, "  WARN %s (Tempest tool names are case-sensitive; MCP names require discovery)\n", warning)
	}
	for _, path := range skippedHooks {
		fmt.Fprintf(stdout, "  UNTRUSTED %s (hooks not loaded)\n", path)
	}
	printPathCheck(stdout, "settings file", opts.Config.SettingsPath())
	printRoleCredentialCheck(stdout, "default", roles.Default.Model.Provider, opts)
	printRoleCredentialCheck(stdout, "plan", roles.Plan.Model.Provider, opts)
	printRoleCredentialCheck(stdout, "smol", roles.Smol.Model.Provider, opts)
	if ping {
		return pingModels(stdout, opts)
	}
	return 0
}

func roleSpec(role, fallback string) string {
	if role == "" {
		return fallback
	}
	return role
}

// reasoningSetting renders a configured reasoning level; unset leaves the
// provider's own default in place.
func reasoningSetting(level string) string {
	if level == "" {
		return "provider default"
	}
	return level
}

// effectiveReasoning is what a role's requests actually carry: a level the
// model can't use is dropped before the request is built.
func effectiveReasoning(level string, model types.Model) string {
	if level == "" || level == "off" {
		return reasoningSetting(level)
	}
	if !model.Reasoning {
		return level + " (ignored: no reasoning support)"
	}
	return level
}

func printRole(w io.Writer, name string, router adapter.ProviderRouter, reasoning string) {
	price := "known"
	if router.Model.ReportsBilledCost() {
		price = "billed"
	} else if router.Model.Cost.Unknown {
		price = "unknown"
	}
	fmt.Fprintf(w, "  %s: model=%s provider=%s api=%s base_url=%s cost=%s reasoning=%s images=%s\n", name, router.Model.ID, router.Model.Provider, router.Model.API, redactedURL(router.Model.BaseURL), price, effectiveReasoning(reasoning, router.Model), yesNo(router.Model.SupportsVision()))
}

func printPathCheck(w io.Writer, name, path string) {

	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(w, "PASS %s: %s\n", name, path)
	} else if os.IsNotExist(err) {
		fmt.Fprintf(w, "WARN %s: %s does not exist yet\n", name, path)
	} else {
		fmt.Fprintf(w, "WARN %s: %v\n", name, err)
	}
}

// redactedURL strips URL components that commonly carry secrets before doctor prints configuration.
func redactedURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "REDACTED"
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String() + " (REDACTED)"
	}
	return raw
}

// yesNo renders a boolean as the lowercase yes/no doctor prints for key
// presence. Doctor only ever says whether a credential exists, never its value.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func printRoleCredentialCheck(w io.Writer, role, provider string, opts commandOptions) {
	if provider == "openai-chatgpt" {
		printChatGPTDoctor(w)
		return
	}
	providerConfig := configuredProvider(provider, opts)
	if providerConfig.UsesCredentialChain() && !providerConfig.CredentialPresent(opts.Secrets.Resolve) {
		fmt.Fprintf(w, "INFO %s provider credential: %s uses a native credential chain; --ping verifies it\n", role, provider)
		return
	}
	if configuredProvider(provider, opts).CredentialPresent(opts.Secrets.Resolve) {
		fmt.Fprintf(w, "PASS %s provider credential: %s present\n", role, provider)
		return
	}
	fmt.Fprintf(w, "WARN %s provider credential: %s absent\n", role, provider)
}

// pingTarget is one distinct model to probe and the roles that use it.
type pingTarget struct {
	roles  []string
	router adapter.ProviderRouter
}

// pingTargets probes the models a run would really use: the default role,
// then plan and smol only when they resolve to a different model.
func pingTargets(roles adapter.RoleRouters) []pingTarget {
	var targets []pingTarget
	for _, role := range []struct {
		name   string
		router adapter.ProviderRouter
	}{{"default", roles.Default}, {"plan", roles.Plan}, {"smol", roles.Smol}} {
		merged := false
		for i := range targets {
			if sameModel(targets[i].router.Model, role.router.Model) {
				targets[i].roles = append(targets[i].roles, role.name)
				merged = true
				break
			}
		}
		if !merged {
			targets = append(targets, pingTarget{roles: []string{role.name}, router: role.router})
		}
	}
	return targets
}

func sameModel(a, b types.Model) bool {
	return a.Provider == b.Provider && a.ID == b.ID && a.BaseURL == b.BaseURL && a.API == b.API
}

// pingModels sends one tiny request to each configured model and prints the
// model used, plus the provider's HTTP status and message on failure.
func pingModels(stdout io.Writer, opts commandOptions) int {
	roles, err := adapter.ResolveRoles(adapter.RoleRoutingConfig{
		DefaultModel:       opts.Config.Model(),
		PlanModel:          opts.Config.PlanModel(),
		SmolModel:          opts.Config.SmolModel(),
		AnthropicBaseURL:   opts.Config.AnthropicBaseURL(),
		OpenAIBaseURL:      opts.Config.OpenAIBaseURL(),
		MaxTokens:          16,
		SecretLookup:       opts.Secrets.Resolve,
		Providers:          opts.Config.Providers(),
		ChatGPTCredentials: adapter.OpenAIChatGPTCredentials(""),
	})
	fmt.Fprintln(stdout, "ping:")
	if err != nil {
		fmt.Fprintf(stdout, "  error configuration: %v\n", err)
		return 1
	}
	exitCode := 0
	for _, target := range pingTargets(roles) {
		model := target.router.Model
		label := fmt.Sprintf("  %s (%s:%s)", strings.Join(target.roles, "+"), model.Provider, model.ID)
		p := configuredProvider(model.Provider, opts)
		if model.Provider != "openai-chatgpt" && !p.CredentialPresent(opts.Secrets.Resolve) && !p.UsesCredentialChain() {
			fmt.Fprintf(stdout, "%s: error no %s credential; set %s or providers.%s.api_key\n", label, model.Provider, p.APIKeyEnv, model.Provider)
			exitCode = 1
			continue
		}
		started := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		probeOptions := &types.SimpleStreamOptions{}
		if model.Provider != "openai-chatgpt" {
			probeOptions.MaxTokens = 16
		}
		probe := target.router.StreamFn(ctx, model, types.Context{
			Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}},
		}, probeOptions)
		message, err := probe.Result(ctx)
		cancel()
		switch {
		case err != nil:
			classified := retry.Classify(retry.Failure{Message: err.Error()}, time.Now())
			fmt.Fprintf(stdout, "%s: error %s: %s\n", label, classified.Code, providerPingDetail(err.Error(), opts))
			exitCode = 1
		case message.StopReason == types.StopError || message.StopReason == types.StopAborted:
			fmt.Fprintf(stdout, "%s: error %s: %s\n", label, message.ErrorCode, providerPingDetail(message.ErrorMessage, opts))
			exitCode = 1
		default:
			fmt.Fprintf(stdout, "%s: ok %s\n", label, time.Since(started).Round(time.Millisecond))
		}
	}
	return exitCode
}

// pingDetail bounds a provider error (which carries "provider HTTP <status>: <body>")
// and strips every resolvable credential value before doctor prints it.
func pingDetail(message string, secrets config.SecretResolver) string {
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		if value, ok := secrets.Resolve(name); ok {
			message = strings.ReplaceAll(message, value, "[REDACTED]")
		}
	}
	return obs.RedactTruncate(message, 400)
}

func providerPingDetail(message string, opts commandOptions) string {
	providers, _ := adapter.Providers(opts.Config.Providers())
	for _, p := range providers {
		values := []string{p.APIKey, p.Credential(opts.Secrets.Resolve)}
		for _, value := range p.Headers {
			values = append(values, value)
		}
		for _, value := range p.Query {
			values = append(values, value)
		}
		for _, value := range values {
			if value != "" {
				message = strings.ReplaceAll(message, value, "[REDACTED]")
			}
		}
	}
	return pingDetail(message, opts.Secrets)
}
