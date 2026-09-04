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
	"strings"

	"go.harness.dev/harness/internal/retry"
)

// RoleRoutingConfig contains the resolved model settings shared by the three
// agent roles. SecretLookup keeps credential material out of durable config.
type RoleRoutingConfig struct {
	DefaultModel     string
	PlanModel        string
	SmolModel        string
	AnthropicBaseURL string
	OpenAIBaseURL    string
	MaxTokens        int
	SecretLookup     func(string) (string, bool)
	Retry            *retry.Config
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
	apiKey, authToken := roleCredentials(model, cfg.SecretLookup)
	return NewProviderRouter(RoutingConfig{
		ModelID:          model,
		AnthropicBaseURL: cfg.AnthropicBaseURL,
		OpenAIBaseURL:    cfg.OpenAIBaseURL,
		APIKey:           apiKey,
		AuthToken:        authToken,
		MaxTokens:        cfg.MaxTokens,
		Env:              func(string) string { return "" },
		Retry:            cfg.Retry,
	})
}

// roleCredentials keeps each provider's key route separate: OpenAI takes an
// API key, Anthropic a key-or-token pair, Google a Gemini key with the
// Google Cloud key as fallback. Resolution goes through
// ResolveProviderForModel so bare model ids ("gemini-2.5-flash") route like
// prefixed ones ("google:gemini-2.5-flash").
func roleCredentials(model string, lookup func(string) (string, bool)) (string, string) {
	if lookup == nil {
		return "", ""
	}
	spec := parseModelSpec(model)
	provider := spec.Provider
	if provider == "" {
		provider = ResolveProviderForModel(spec.ModelID)
	}
	switch provider {
	case "openai":
		key, _ := lookup("OPENAI_API_KEY")
		return key, ""
	case "openrouter":
		key, _ := lookup("OPENROUTER_API_KEY")
		return key, ""
	case "google":
		if key, _ := lookup("GEMINI_API_KEY"); key != "" {
			return key, ""
		}
		key, _ := lookup("GOOGLE_API_KEY")
		return key, ""
	default:
		apiKey, _ := lookup("ANTHROPIC_API_KEY")
		authToken, _ := lookup("ANTHROPIC_AUTH_TOKEN")
		return apiKey, authToken
	}
}
