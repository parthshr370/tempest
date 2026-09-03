package adapter

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"go.harness.dev/harness/internal/catalog"
	"go.harness.dev/harness/internal/engine/types"
)

// modelSpec is a parsed model string of the form "provider:model" or
// "provider:model@baseURL". A bare model with no leading "provider:" leaves
// Provider empty, so the router applies the Anthropic default and existing
// wiring is unchanged.
type modelSpec struct {
	Provider string
	ModelID  string
	BaseURL  string
}

// parseModelSpec splits a model string into provider, model id, and an optional
// @baseURL. The provider is the text before the first ':'; the base URL is the
// text after the first '@' in the remainder (mirrors gitagent's parseModelString
// / createCustomModel). All fields are trimmed; provider is lowercased. A bare
// model id with no ':' (e.g. "claude-opus-4-8" or "anthropic/claude-opus-4-8")
// yields an empty Provider.
func parseModelSpec(raw string) modelSpec {
	raw = strings.TrimSpace(raw)
	provider, rest := "", raw
	if i := strings.IndexByte(raw, ':'); i >= 0 {
		provider, rest = raw[:i], raw[i+1:]
	}
	modelID, baseURL := rest, ""
	if j := strings.IndexByte(rest, '@'); j >= 0 {
		modelID, baseURL = rest[:j], rest[j+1:]
	}
	return modelSpec{
		Provider: strings.ToLower(strings.TrimSpace(provider)),
		ModelID:  strings.TrimSpace(modelID),
		BaseURL:  strings.TrimSpace(baseURL),
	}
}

// ModelIDFromSpec returns the bare model id of a model string, dropping any
// "provider:" prefix and "@baseURL" suffix. Callers that only override the model
// id use this helper so the shared route contributes just the model id.
func ModelIDFromSpec(raw string) string {
	return parseModelSpec(raw).ModelID
}

// ValidateModelSpec rejects malformed or unsupported model routes before a
// session starts. A bare model name routes through the Anthropic adapter.
func ValidateModelSpec(raw string) error {
	spec := parseModelSpec(raw)
	if strings.TrimSpace(raw) == "" || spec.ModelID == "" {
		return fmt.Errorf("model spec must include a model id")
	}
	if spec.Provider != "" && spec.Provider != "anthropic" && spec.Provider != "openai" && spec.Provider != "google" {
		return fmt.Errorf("unsupported model provider; use anthropic, openai, or google")
	}
	if spec.BaseURL != "" {
		if err := ValidateBaseURL(spec.BaseURL); err != nil {
			return fmt.Errorf("model spec base URL is invalid: %w", err)
		}
	}
	return nil
}

// ValidateBaseURL rejects malformed endpoints and endpoints containing userinfo.
func ValidateBaseURL(raw string) error {
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("must be an absolute HTTP URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("must not contain credentials or URL fragments")
	}
	return nil
}

// route is a fully resolved provider route: the model to stream, the registry
// API id to dispatch through, and the credential material.
type route struct {
	Model     types.Model
	API       string
	APIKey    string
	AuthToken string
}

// resolveRoute turns the routing config + environment into a concrete provider
// route. Anthropic is the default when the model carries no (or an "anthropic")
// provider prefix. "openai" uses an explicit or configured compatible endpoint.
// "google" speaks the Generative AI protocol with a GEMINI_API_KEY (or
// GOOGLE_API_KEY) credential. When the model id hits the embedded catalog, the
// catalog's context window and cost win and its output cap becomes the default
// max tokens; the hardcoded values below stay as fallbacks for unknown ids.
// An unknown provider is a deterministic configuration error rather than a
// silent Anthropic fallback.
func resolveRoute(cfg RoutingConfig, env func(string) string) (route, error) {
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = envMaxTokens(env)
	}
	raw := firstNonEmpty(cfg.ModelID, env("ANTHROPIC_DEFAULT_SONNET_MODEL"), "claude-opus-4-8")
	spec := parseModelSpec(raw)
	prov := spec.Provider
	if prov == "" {
		prov = "anthropic"
	}
	model := types.Model{ID: spec.ModelID, Name: spec.ModelID, MaxTokens: maxTokens}
	var api, apiKey, authToken string
	switch prov {
	case "anthropic":
		api = AnthropicMessagesAPI
		model.Provider = "anthropic"
		model.BaseURL = firstNonEmpty(spec.BaseURL, cfg.AnthropicBaseURL, cfg.BaseURL, env("HARNESS_ANTHROPIC_BASE_URL"), env("ANTHROPIC_BASE_URL"))
		model.ContextWindow = 200000
		apiKey = firstNonEmpty(cfg.APIKey, env("ANTHROPIC_OAUTH_TOKEN"), env("ANTHROPIC_API_KEY"))
		authToken = firstNonEmpty(cfg.AuthToken, env("ANTHROPIC_AUTH_TOKEN"))
	case "openai":
		api = OpenAICompletionsAPI
		model.Provider = "openai"
		model.BaseURL = firstNonEmpty(spec.BaseURL, cfg.OpenAIBaseURL, cfg.BaseURL, env("OPENAI_BASE_URL"))
		model.ContextWindow = 128000
		apiKey = firstNonEmpty(cfg.APIKey, env("OPENAI_API_KEY"))
	case "google":
		api = GoogleGenerativeAPI
		model.Provider = "google"
		model.BaseURL = firstNonEmpty(spec.BaseURL, cfg.BaseURL, env("GEMINI_BASE_URL"))
		model.ContextWindow = 1048576
		apiKey = firstNonEmpty(cfg.APIKey, env("GEMINI_API_KEY"), env("GOOGLE_API_KEY"))
	default:
		return route{}, fmt.Errorf("unsupported model provider; use anthropic, openai, or google")
	}
	model.API = api
	// Catalog defaults override the hardcoded fallbacks for known model ids.
	// An explicit cfg.MaxTokens or HARNESS_MAX_TOKENS still beats the catalog.
	if cm, ok := catalog.Lookup(prov, spec.ModelID); ok {
		if cm.ContextWindow > 0 {
			model.ContextWindow = cm.ContextWindow
		}
		model.Cost = cm.Cost
		if maxTokens <= 0 && cm.MaxTokens > 0 {
			model.MaxTokens = cm.MaxTokens
		}
	}
	if model.MaxTokens <= 0 {
		model.MaxTokens = defaultMaxTokens
	}
	return route{Model: model, API: api, APIKey: apiKey, AuthToken: authToken}, nil
}

// envMaxTokens returns the HARNESS_MAX_TOKENS override, or 0 when unset or
// invalid so the caller falls back to the catalog or defaultMaxTokens.
func envMaxTokens(env func(string) string) int {
	if env == nil {
		return 0
	}
	v := strings.TrimSpace(env("HARNESS_MAX_TOKENS"))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
