package adapter

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"
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

// parseModelSpec keeps version suffixes in model IDs. Only @http:// or
// @https:// starts an endpoint override.
func parseModelSpec(raw string) modelSpec {
	raw = strings.TrimSpace(raw)
	baseURL := ""
	if j := modelBaseURLIndex(raw); j >= 0 {
		raw, baseURL = raw[:j], raw[j+1:]
	}
	provider, modelID := "", raw
	if i := strings.IndexByte(raw, ':'); i >= 0 {
		provider, modelID = raw[:i], raw[i+1:]
	}
	return modelSpec{
		Provider: strings.ToLower(strings.TrimSpace(provider)),
		ModelID:  strings.TrimSpace(modelID),
		BaseURL:  strings.TrimSpace(baseURL),
	}
}

func modelBaseURLIndex(raw string) int {
	for i := range len(raw) {
		if raw[i] == '@' && (strings.HasPrefix(raw[i+1:], "http://") || strings.HasPrefix(raw[i+1:], "https://")) {
			return i
		}
	}
	return -1
}

// HasModelBaseURL reports whether a route overrides the host's endpoint.
func HasModelBaseURL(raw string) bool { return modelBaseURLIndex(raw) >= 0 }

// ModelIDFromSpec returns the bare model id of a model string, dropping any
// "provider:" prefix and "@baseURL" suffix. Callers that only override the model
// id use this helper so the shared route contributes just the model id.
func ModelIDFromSpec(raw string) string {
	return parseModelSpec(raw).ModelID
}

// ValidateModelSpec validates a route using the built-in provider registry.
func ValidateModelSpec(raw string) error { return ValidateModelSpecWithProviders(raw, nil) }

// ValidateModelSpecWithProviders also accepts trusted user-defined providers.
func ValidateModelSpecWithProviders(raw string, providers map[string]ProviderConfig) error {
	spec := parseModelSpec(raw)
	if strings.TrimSpace(raw) == "" || spec.ModelID == "" {
		return fmt.Errorf("model spec must include a model id")
	}
	registry, err := Providers(providers)
	if err != nil {
		return err
	}
	if spec.Provider != "" {
		if _, ok := registry[spec.Provider]; !ok {
			return fmt.Errorf("unsupported model provider %q; configure it in providers", spec.Provider)
		}
	}
	if spec.Provider == "openai-chatgpt" && spec.BaseURL != "" {
		return fmt.Errorf("openai-chatgpt does not allow endpoint overrides")
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
	Model            types.Model
	API              string
	APIKey           string
	AuthToken        string
	Provider         ProviderConfig
	CredentialSource func(context.Context) (RequestCredential, error)
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
	registry, err := Providers(cfg.Providers)
	if err != nil {
		return route{}, err
	}
	p, ok := registry[prov]
	if !ok {
		return route{}, fmt.Errorf("unsupported model provider %q; configure it in providers", prov)
	}
	if prov == "openai-chatgpt" {
		if cfg.BaseURL != "" || cfg.APIKey != "" || cfg.AuthToken != "" {
			return route{}, fmt.Errorf("openai-chatgpt cannot use API keys or base URL overrides; remove the conflicting setting")
		}
		if cfg.MaxTokens > 0 && !cfg.MaxTokensDefaulted || envMaxTokens(env) > 0 {
			return route{}, fmt.Errorf("ChatGPT plan usage does not support explicit output-token caps; use turn or wall-time limits")
		}
		if cfg.ChatGPTCredentials == nil {
			return route{}, fmt.Errorf("openai-chatgpt requires an explicit credential source; use OpenAIChatGPTCredentials or run tempest login openai in the CLI")
		}
	}
	contextWindow := 128000
	switch p.Protocol {
	case "anthropic":
		contextWindow = 200000
	case "google", "vertex":
		contextWindow = 1048576
	}
	model := types.Model{ID: spec.ModelID, Name: spec.ModelID, Provider: prov, API: protocolAPI(p.Protocol), MaxTokens: maxTokens, ContextWindow: contextWindow, Input: []string{"text"}, Cost: types.ModelCost{Unknown: true}}
	model.BaseURL = firstNonEmpty(spec.BaseURL, cfg.BaseURL, p.BaseURL)
	switch prov {
	case "anthropic":
		model.BaseURL = firstNonEmpty(spec.BaseURL, cfg.AnthropicBaseURL, cfg.BaseURL, env("HARNESS_ANTHROPIC_BASE_URL"), env("ANTHROPIC_BASE_URL"), p.BaseURL)
		model.ContextWindow = 200000
	case "openai":
		model.BaseURL = firstNonEmpty(spec.BaseURL, cfg.OpenAIBaseURL, cfg.BaseURL, env("OPENAI_BASE_URL"), p.BaseURL)
		model.ContextWindow = 128000
	case "google":
		model.BaseURL = firstNonEmpty(spec.BaseURL, cfg.BaseURL, env("GEMINI_BASE_URL"), p.BaseURL)
		model.ContextWindow = 1048576
	case "openrouter":
		model.BaseURL = firstNonEmpty(spec.BaseURL, cfg.BaseURL, env("OPENROUTER_BASE_URL"), p.BaseURL)
		model.ContextWindow = 200000
	}
	if prov == "azure" {
		endpoint := strings.TrimRight(firstNonEmpty(model.BaseURL, env("AZURE_OPENAI_ENDPOINT")), "/")
		if endpoint == "" {
			return route{}, fmt.Errorf("provider azure requires base_url or AZURE_OPENAI_ENDPOINT")
		}
		model.BaseURL = endpoint
		if !strings.HasSuffix(endpoint, "/openai/v1") && !strings.Contains(endpoint, "/openai/deployments/") {
			model.BaseURL = endpoint + "/openai/deployments/" + url.PathEscape(spec.ModelID)
		}
		if strings.HasSuffix(endpoint, "/openai/v1") {
			p.Query = maps.Clone(p.Query)
			delete(p.Query, "api-version")
		}
	}
	if cm, ok := routeCatalogModel(prov, p.Protocol, spec.ModelID); ok {
		model.ContextWindow = cm.ContextWindow
		model.Cost = cm.Cost
		model.OpenAICompat = cm.OpenAICompat
		model.Compat = cm.AnthropicCompat
		model.Reasoning = slices.Contains(cm.Capabilities, "thinking")
		model.Input = []string{"text"}
		if slices.Contains(cm.Capabilities, "images") {
			model.Input = append(model.Input, "image")
		}
		if cm.MaxTokens > 0 && (model.MaxTokens <= 0 || model.MaxTokens > cm.MaxTokens) {
			model.MaxTokens = cm.MaxTokens
		}
	}
	for _, configured := range p.Models {
		if configured.ID == model.ID {
			if configured.Name != "" {
				model.Name = configured.Name
			}
			if configured.ContextWindow > 0 {
				model.ContextWindow = configured.ContextWindow
			}
			if configured.Reasoning || p.modelFields[configured.ID]["reasoning"] {
				model.Reasoning = configured.Reasoning
			}
			if configured.Input != nil {
				model.Input = slices.Clone(configured.Input)
			}
			if p.modelFields[configured.ID]["cost"] || p.modelFields[configured.ID] == nil && configured.Cost != (types.ModelCost{}) {
				model.Cost = configured.Cost
			}
			if configured.Compat != nil {
				model.Compat = configured.Compat
			}
			if configured.OpenAICompat != nil {
				model.OpenAICompat = mergeOpenAICompat(model.OpenAICompat, configured.OpenAICompat)
			}
			if configured.MaxTokens > 0 && (model.MaxTokens <= 0 || model.MaxTokens > configured.MaxTokens) {
				model.MaxTokens = configured.MaxTokens
			}
			break
		}
	}
	model.OpenAICompat = mergeOpenAICompat(p.Compatibility, model.OpenAICompat)
	// Unknown compatible endpoints don't inherit OpenAI-only cache fields.
	endpoint, _ := url.Parse(model.BaseURL)
	if prov == "openai" && (endpoint == nil || endpoint.Hostname() != "api.openai.com") && model.OpenAICompat != nil {
		if explicit := cfg.Providers[prov].Compatibility; explicit == nil || explicit.SupportsPromptCacheKey == nil {
			model.OpenAICompat.SupportsPromptCacheKey = new(false)
		}
	}
	if model.MaxTokens <= 0 {
		model.MaxTokens = defaultMaxTokens
	}
	if model.ContextWindow > 0 {
		model.MaxTokens = min(model.MaxTokens, max(1, model.ContextWindow-4096))
	}
	key := firstNonEmpty(cfg.APIKey, p.Credential(func(name string) (string, bool) { v := env(name); return v, v != "" }))
	token := cfg.AuthToken
	if prov == "anthropic" {
		if cfg.Providers[prov].APIKeyEnv == "" {
			key = firstNonEmpty(cfg.APIKey, env("ANTHROPIC_OAUTH_TOKEN"), key)
		}
		token = firstNonEmpty(token, env("ANTHROPIC_AUTH_TOKEN"))
	}
	if prov == "openai-chatgpt" {
		model.Cost = types.ModelCost{Unknown: true}
		return route{Model: model, API: model.API, Provider: p, CredentialSource: cfg.ChatGPTCredentials}, nil
	}
	return route{Model: model, API: model.API, APIKey: key, AuthToken: token, Provider: p}, nil
}

func routeCatalogModel(provider, protocol, id string) (catalog.Model, bool) {
	if m, ok := catalog.Lookup(provider, id); ok {
		return m, true
	}
	switch protocol {
	case "openai-responses":
		if provider != "perplexity" {
			return catalog.Lookup("openai", id)
		}
	case "vertex":
		modelID := id
		if i := strings.LastIndex(modelID, "/models/"); i >= 0 {
			modelID = modelID[i+len("/models/"):]
		}
		modelID, _, _ = strings.Cut(modelID, "@")
		family := "google"
		if strings.HasPrefix(modelID, "claude-") {
			family = "anthropic"
		}
		return catalog.Lookup(family, modelID)
	case "bedrock":
		// Regional profiles and ARNs keep their wire ID, but share model limits.
		if i := strings.Index(id, "anthropic."); i >= 0 {
			modelID := id[i+len("anthropic."):]
			modelID, _, _ = strings.Cut(modelID, ":")
			for _, m := range catalog.Models() {
				if m.Provider == "anthropic" && (modelID == m.ID || strings.HasPrefix(modelID, m.ID+"-")) {
					return m, true
				}
			}
		}
	}
	return catalog.Model{}, false
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
