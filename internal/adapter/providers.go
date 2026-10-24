package adapter

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"go.harness.dev/harness/internal/engine/types"
)

// ProviderConfig describes an endpoint independently of its wire protocol.
// Credentials are bound to stream options, never to the persisted model.
type ProviderConfig struct {
	Name          string              `json:"-"`
	Protocol      string              `json:"protocol,omitempty"`
	BaseURL       string              `json:"base_url,omitempty"`
	APIKeyEnv     string              `json:"api_key_env,omitempty"`
	APIKey        string              `json:"api_key,omitempty"`
	AuthHeader    string              `json:"auth_header,omitempty"`
	AuthQuery     string              `json:"auth_query,omitempty"`
	AuthPrefix    *string             `json:"auth_prefix,omitempty"`
	NoAuth        *bool               `json:"no_auth,omitempty"`
	Headers       map[string]string   `json:"headers,omitempty"`
	Query         map[string]string   `json:"query,omitempty"`
	Compatibility *types.OpenAICompat `json:"compatibility,omitempty"`
	Models        []types.Model       `json:"models,omitempty"`
	modelFields   map[string]map[string]bool
}

// BuiltinProviders returns owned endpoint presets. Model metadata stays in catalog.
func BuiltinProviders() map[string]ProviderConfig {
	chat := func(url, key string) ProviderConfig {
		return ProviderConfig{Protocol: "openai-chat", BaseURL: url, APIKeyEnv: key, Compatibility: &types.OpenAICompat{MaxTokensField: "max_tokens", SupportsPromptCacheKey: new(false)}}
	}
	providers := map[string]ProviderConfig{
		"anthropic":        {Protocol: "anthropic", BaseURL: "https://api.anthropic.com", APIKeyEnv: "ANTHROPIC_API_KEY"},
		"openai":           {Protocol: "openai-chat", BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY", Compatibility: &types.OpenAICompat{SupportsPromptCacheKey: new(true)}},
		"google":           {Protocol: "google", BaseURL: "https://generativelanguage.googleapis.com", APIKeyEnv: "GEMINI_API_KEY"},
		"openrouter":       {Protocol: "openai-chat", BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY", Compatibility: &types.OpenAICompat{ReasoningFormat: "openrouter", SupportsPromptCacheKey: new(true), SupportsSessionID: new(true), CacheControlFormat: "anthropic"}},
		"groq":             chat("https://api.groq.com/openai/v1", "GROQ_API_KEY"),
		"mistral":          chat("https://api.mistral.ai/v1", "MISTRAL_API_KEY"),
		"xai":              chat("https://api.x.ai/v1", "XAI_API_KEY"),
		"deepseek":         chat("https://api.deepseek.com", "DEEPSEEK_API_KEY"),
		"together":         chat("https://api.together.ai/v1", "TOGETHER_API_KEY"),
		"fireworks":        chat("https://api.fireworks.ai/inference/v1", "FIREWORKS_API_KEY"),
		"cerebras":         chat("https://api.cerebras.ai/v1", "CEREBRAS_API_KEY"),
		"perplexity":       {Protocol: "openai-responses", BaseURL: "https://api.perplexity.ai/v1", APIKeyEnv: "PERPLEXITY_API_KEY", Compatibility: &types.OpenAICompat{SupportsPromptCacheKey: new(false), SupportsEncryptedReasoning: new(false)}},
		"ollama":           chat("http://localhost:11434/v1", ""),
		"lmstudio":         chat("http://localhost:1234/v1", ""),
		"vllm":             chat("http://localhost:8000/v1", ""),
		"llamacpp":         chat("http://localhost:8080/v1", ""),
		"azure":            {Protocol: "openai-chat", APIKeyEnv: "AZURE_OPENAI_API_KEY", AuthHeader: "api-key", AuthPrefix: new(""), Query: map[string]string{"api-version": "2025-04-01-preview"}, Compatibility: &types.OpenAICompat{SupportsPromptCacheKey: new(false)}},
		"openai-responses": {Protocol: "openai-responses", BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
		"openai-chatgpt":   {Protocol: "openai-responses", BaseURL: "https://api.openai.com/v1"},
		"bedrock":          {Protocol: "bedrock", APIKeyEnv: "AWS_BEARER_TOKEN_BEDROCK"},
		"vertex":           {Protocol: "vertex", APIKeyEnv: "GOOGLE_CLOUD_API_KEY"},
	}
	for _, name := range []string{"ollama", "lmstudio", "vllm", "llamacpp"} {
		p := providers[name]
		p.NoAuth = new(true)
		providers[name] = p
	}
	providers["mistral"].Compatibility.RequiresMistralToolIDs = new(true)
	providers["mistral"].Compatibility.SupportsUsageInStreaming = new(false)
	providers["cerebras"].Compatibility.SupportsUsageInStreaming = new(false)
	providers["ollama"].Compatibility.SupportsToolChoice = new(false)
	providers["deepseek"].Compatibility.ReasoningContentField = "reasoning_content"
	for name, p := range providers {
		p.Name = name
		providers[name] = p
	}
	return providers
}

// Providers merges trusted settings with presets. A settings entry may override
// one preset field; a new name needs a protocol and endpoint.
func Providers(overrides map[string]ProviderConfig) (map[string]ProviderConfig, error) {
	providers := BuiltinProviders()
	for name, override := range overrides {
		if name == "openai-chatgpt" {
			return nil, fmt.Errorf("openai-chatgpt uses its signed-in credential and public Responses endpoint; provider overrides are not allowed")
		}
		if name == "" || name != strings.ToLower(name) || strings.ContainsAny(name, ":@/ \t\r\n") {
			return nil, fmt.Errorf("invalid provider name %q", name)
		}
		p := providers[name]
		if override.Protocol != "" {
			p.Protocol = override.Protocol
		}
		if override.BaseURL != "" {
			p.BaseURL = override.BaseURL
		}
		if override.APIKeyEnv != "" {
			p.APIKeyEnv = override.APIKeyEnv
		}
		if override.APIKey != "" {
			p.APIKey = override.APIKey
		}
		if override.AuthHeader != "" {
			p.AuthHeader = override.AuthHeader
		}
		if override.AuthQuery != "" {
			p.AuthQuery = override.AuthQuery
		}
		if override.AuthPrefix != nil {
			p.AuthPrefix = override.AuthPrefix
		}
		if override.NoAuth != nil {
			p.NoAuth = override.NoAuth
		}
		if override.Headers != nil {
			p.Headers = maps.Clone(override.Headers)
		}
		if override.Query != nil {
			p.Query = maps.Clone(override.Query)
		}
		if override.Compatibility != nil {
			p.Compatibility = mergeOpenAICompat(p.Compatibility, override.Compatibility)
		}
		if override.Models != nil {
			p.Models = slices.Clone(override.Models)
			p.modelFields = override.modelFields
		}
		p.Name = name
		if p.AuthPrefix != nil && p.AuthHeader == "" {
			p.AuthHeader = "Authorization"
		}
		if protocolAPI(p.Protocol) == "" {
			return nil, fmt.Errorf("provider %q has unsupported protocol %q", name, p.Protocol)
		}
		if p.BaseURL != "" {
			if err := ValidateBaseURL(p.BaseURL); err != nil {
				return nil, fmt.Errorf("provider %q base_url: %w", name, err)
			}
		} else if p.Protocol != "bedrock" && p.Protocol != "vertex" && name != "azure" {
			return nil, fmt.Errorf("provider %q requires base_url", name)
		}
		for key, value := range p.Headers {
			if !validHeaderName(key) || strings.ContainsAny(value, "\r\n") {
				return nil, fmt.Errorf("provider %q has invalid header", name)
			}
		}
		if p.AuthHeader != "" && !validHeaderName(p.AuthHeader) {
			return nil, fmt.Errorf("provider %q has invalid auth_header", name)
		}
		if p.Compatibility != nil {
			c := p.Compatibility
			if c.MaxTokensField != "" && c.MaxTokensField != "max_tokens" && c.MaxTokensField != "max_completion_tokens" {
				return nil, fmt.Errorf("provider %q has invalid maxTokensField", name)
			}
			if c.ReasoningFormat != "" && c.ReasoningFormat != "openai" && c.ReasoningFormat != "openrouter" && c.ReasoningFormat != "none" {
				return nil, fmt.Errorf("provider %q has invalid reasoningFormat", name)
			}
			if c.ReasoningContentField != "" && c.ReasoningContentField != "reasoning_content" && c.ReasoningContentField != "reasoning" && c.ReasoningContentField != "reasoning_text" {
				return nil, fmt.Errorf("provider %q has invalid reasoningContentField", name)
			}
		}
		for _, m := range p.Models {
			if m.ID == "" || m.ContextWindow < 0 || m.MaxTokens < 0 {
				return nil, fmt.Errorf("provider %q has invalid model limits or id", name)
			}
		}
		providers[name] = p
	}
	return providers, nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
			return false
		}
	}
	return true
}

func protocolAPI(protocol string) string {
	switch protocol {
	case "anthropic":
		return AnthropicMessagesAPI
	case "openai-chat":
		return OpenAICompletionsAPI
	case "google":
		return GoogleGenerativeAPI
	case "openai-responses":
		return "openai-responses"
	case "bedrock":
		return "bedrock-converse-stream"
	case "vertex":
		return "google-vertex"
	}
	return ""
}

// API returns the stream API identifier for this endpoint's protocol.
func (p ProviderConfig) API() string { return protocolAPI(p.Protocol) }

// Credential resolves the provider's key without exposing it in diagnostics.
func (p ProviderConfig) Credential(lookup func(string) (string, bool)) string {
	if lookup != nil {
		if v, present := lookup(p.APIKeyEnv); p.APIKeyEnv != "" && present && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		if p.Name == "google" {
			if v, present := lookup("GOOGLE_API_KEY"); present && strings.TrimSpace(v) != "" {
				return v
			}
		}
		if p.Name == "anthropic" {
			if v, present := lookup("ANTHROPIC_OAUTH_TOKEN"); present && strings.TrimSpace(v) != "" {
				return v
			}
		}
	}
	return strings.TrimSpace(p.APIKey)
}

// CredentialPresent reports whether the endpoint has credentials or allows no auth.
func (p ProviderConfig) CredentialPresent(lookup func(string) (string, bool)) bool {
	if p.NoAuth != nil && *p.NoAuth {
		return true
	}
	if p.Credential(lookup) != "" {
		return true
	}
	if p.Name == "anthropic" && lookup != nil {
		v, _ := lookup("ANTHROPIC_AUTH_TOKEN")
		if v != "" {
			return true
		}
	}
	if p.AuthHeader != "" && hasHeader(p.Headers, p.AuthHeader) {
		return true
	}
	for key, value := range p.Headers {
		if strings.TrimSpace(value) != "" && (strings.EqualFold(key, "authorization") || strings.EqualFold(key, "x-api-key") || strings.EqualFold(key, "api-key")) {
			return true
		}
	}
	return p.Query["key"] != "" || p.Query["api-key"] != "" || p.AuthQuery != "" && p.Query[p.AuthQuery] != ""
}

// UnmarshalJSON keeps unspecified model prices unknown rather than free.
func (p *ProviderConfig) UnmarshalJSON(raw []byte) error {
	type config ProviderConfig
	var decoded config
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	var models struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &models); err != nil {
		return err
	}
	decoded.modelFields = make(map[string]map[string]bool, len(models.Models))
	for i, model := range models.Models {
		fields := make(map[string]bool, len(model))
		for field := range model {
			fields[field] = true
		}
		decoded.modelFields[decoded.Models[i].ID] = fields
		if !fields["cost"] {
			decoded.Models[i].Cost.Unknown = true
		}
	}
	*p = ProviderConfig(decoded)
	return nil
}

func (p ProviderConfig) credentialError() error {
	if p.APIKeyEnv != "" {
		return fmt.Errorf("provider %q: missing API key; set %s or providers.%s.api_key", p.Name, p.APIKeyEnv, p.Name)
	}
	return fmt.Errorf("provider %q: missing API key; set providers.%s.api_key or no_auth", p.Name, p.Name)
}

func putHeader(headers map[string]string, key, value string) {
	for old := range headers {
		if strings.EqualFold(old, key) {
			delete(headers, old)
		}
	}
	headers[http.CanonicalHeaderKey(key)] = value
}

func mergeOpenAICompat(defaults, override *types.OpenAICompat) *types.OpenAICompat {
	if defaults == nil && override == nil {
		return nil
	}
	merged := types.OpenAICompat{}
	if defaults != nil {
		merged = *defaults
	}
	if override == nil {
		return &merged
	}
	if override.MaxTokensField != "" {
		merged.MaxTokensField = override.MaxTokensField
	}
	if override.ReasoningFormat != "" {
		merged.ReasoningFormat = override.ReasoningFormat
	}
	if override.ReasoningContentField != "" {
		merged.ReasoningContentField = override.ReasoningContentField
	}
	if override.ReasoningDisableMode != "" {
		merged.ReasoningDisableMode = override.ReasoningDisableMode
	}
	if override.CacheControlFormat != "" {
		merged.CacheControlFormat = override.CacheControlFormat
	}
	if override.SupportsPromptCacheKey != nil {
		merged.SupportsPromptCacheKey = override.SupportsPromptCacheKey
	}
	if override.SupportsEncryptedReasoning != nil {
		merged.SupportsEncryptedReasoning = override.SupportsEncryptedReasoning
	}
	if override.SupportsSessionID != nil {
		merged.SupportsSessionID = override.SupportsSessionID
	}
	if override.SupportsUsageInStreaming != nil {
		merged.SupportsUsageInStreaming = override.SupportsUsageInStreaming
	}
	if override.SupportsToolChoice != nil {
		merged.SupportsToolChoice = override.SupportsToolChoice
	}
	if override.RequiresMistralToolIDs != nil {
		merged.RequiresMistralToolIDs = override.RequiresMistralToolIDs
	}
	return &merged
}

// UsesCredentialChain reports whether native credentials can replace an API key.
func (p ProviderConfig) UsesCredentialChain() bool {
	return p.Protocol == "bedrock" || p.Protocol == "vertex"
}
