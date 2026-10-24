// Package adapter turns resolved configuration into ready-to-call provider
// routers, one per model role.
//
// The CLI resolves config into model specs like "claude-sonnet-5" or
// "openai:gpt-5.1@https://host/v1" and hands them here. We parse the provider
// out of each spec, look up the right credential, and bind a stream function
// against the provider layer. [ResolveRoles] does this for all three roles (the
// default agent, the read-only plan model, and the small smol helper), so each
// role can point at a different provider while sharing one retry policy and
// token budget. An unknown provider is a hard error, never a silent fallback.
package adapter

import (
	"context"
	"fmt"
	"strings"

	"go.harness.dev/harness/internal/retry"
)

// RoleRoutingConfig contains the resolved model settings shared by the three
// agent roles. SecretLookup keeps credential material out of durable config.
type RoleRoutingConfig struct {
	DefaultModel       string
	PlanModel          string
	SmolModel          string
	AnthropicBaseURL   string
	OpenAIBaseURL      string
	MaxTokens          int
	SecretLookup       func(string) (string, bool)
	Retry              *retry.Config
	Providers          map[string]ProviderConfig
	ChatGPTCredentials func(context.Context) (RequestCredential, error)
}

// RoleRouters contains a fully routed provider for each agent role.
type RoleRouters struct {
	Default ProviderRouter
	Plan    ProviderRouter
	Smol    ProviderRouter
}

// ResolveRoles routes the default, plan, and smol role models through the
// provider router. Empty plan and smol models inherit the default model.
func ResolveRoles(cfg RoleRoutingConfig) (RoleRouters, error) {
	defaultModel := strings.TrimSpace(cfg.DefaultModel)
	planModel := firstNonEmpty(strings.TrimSpace(cfg.PlanModel), defaultModel)
	smolModel := firstNonEmpty(strings.TrimSpace(cfg.SmolModel), defaultModel)

	defaultRouter, err := newRoleRouter(defaultModel, cfg)
	if err != nil {
		return RoleRouters{}, err
	}
	planRouter, err := newRoleRouter(planModel, cfg)
	if err != nil {
		return RoleRouters{}, err
	}
	smolRouter, err := newRoleRouter(smolModel, cfg)
	if err != nil {
		return RoleRouters{}, err
	}
	return RoleRouters{Default: defaultRouter, Plan: planRouter, Smol: smolRouter}, nil
}

// newRoleRouter resolves one role with the credentials that match its provider prefix.
func newRoleRouter(model string, cfg RoleRoutingConfig) (ProviderRouter, error) {
	providers, err := Providers(cfg.Providers)
	if err != nil {
		return ProviderRouter{}, err
	}
	spec := parseModelSpec(model)
	provider := spec.Provider
	if provider == "" {
		provider = "anthropic"
	}
	p, ok := providers[provider]
	if !ok {
		return ProviderRouter{}, fmt.Errorf("unsupported model provider %q", provider)
	}
	apiKey := p.Credential(cfg.SecretLookup)
	authToken := ""
	if provider == "anthropic" && cfg.SecretLookup != nil {
		authToken, _ = cfg.SecretLookup("ANTHROPIC_AUTH_TOKEN")
	}
	maxTokens := cfg.MaxTokens
	if provider == "openai-chatgpt" {
		maxTokens = 0
	}
	return NewProviderRouter(RoutingConfig{
		ModelID:            model,
		AnthropicBaseURL:   cfg.AnthropicBaseURL,
		OpenAIBaseURL:      cfg.OpenAIBaseURL,
		APIKey:             apiKey,
		AuthToken:          authToken,
		MaxTokens:          maxTokens,
		Providers:          cfg.Providers,
		ChatGPTCredentials: cfg.ChatGPTCredentials,
		// Environment sources are resolved upstream: config folds
		// HARNESS_* and the vendor base-URL variables into cfg in
		// precedence order, and SecretLookup supplies credentials. The
		// router must not read the process environment behind them.
		Env:   func(string) string { return "" },
		Retry: cfg.Retry,
	})
}
