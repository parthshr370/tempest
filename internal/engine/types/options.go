package types

import (
	"context"
	"encoding/json"
)

// ProviderResponse is the response metadata passed to StreamOptions.OnResponse.
type ProviderResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
}

// ThinkingConfig controls extended-thinking behavior for a request.
type ThinkingConfig struct {
	Enabled      bool   `json:"enabled,omitempty"`
	BudgetTokens int    `json:"budgetTokens,omitempty"`
	Level        string `json:"level,omitempty"` // off|low|medium|high
}

// StreamOptions are per-request knobs shared across providers (02-map-ai-agent.md
// A15). AbortSignal is intentionally absent — cancellation is via context.Context
// threaded through the provider/loop call sites. This struct will grow as the
// provider seam (A5) and Anthropic client (A7) land.
type StreamOptions struct {
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   int      `json:"maxTokens,omitempty"`
	APIKey      string   `json:"-"`
	// Query carries endpoint parameters without persisting credentials.
	Query  map[string]string `json:"-"`
	NoAuth bool              `json:"-"`
	// CredentialRoute is the route APIKey, NoAuth, Headers and Query were
	// issued for. A provider route that sends the request anywhere else
	// drops all four and authenticates with its own credential, so a
	// per-request credential never reaches another provider or endpoint.
	// Zero means the route of the model the request is sent with; a stream
	// function that forwards a request to a different model sets it first.
	CredentialRoute Route `json:"-"`
	// APIKeyFor, when set, resolves the API key for a provider (the agent's
	// GetApiKey). BindAPIKey applies it for the model a request is sent to,
	// so a stream function that forwards a request to another model (a
	// backup) resolves that model's key the same way its first model did.
	// It may be called from the stream's goroutine.
	APIKeyFor      func(provider string) string `json:"-"`
	DisableEnvAuth bool                         `json:"-"`
	Transport      string                       `json:"transport,omitempty"`
	// The Go seam uses a plain string map; header suppression can be added when a
	// caller needs it.
	Headers  map[string]string `json:"headers,omitempty"`
	Thinking ThinkingConfig    `json:"thinking,omitzero"`
	// SessionID enables session-scoped prompt caching for providers that support
	// it. CacheRetention is the cache preference ("none"|"short"|"long";
	// default "short").
	SessionID      string                              `json:"sessionId,omitempty"`
	CacheRetention string                              `json:"cacheRetention,omitempty"`
	OnPayload      func(any, Model) (any, error)       `json:"-"`
	OnResponse     func(ProviderResponse, Model) error `json:"-"`
	// AdmitFallback, when set, is called by a fallback chain after it
	// announced a switch with an EvFallback event and before the backup
	// model gets the request. The backup is called only when it returns nil;
	// an error ends the stream instead. The agent loop uses it to make the
	// switch durable before the backup can start or spend anything.
	AdmitFallback             func(context.Context, ModelFallback) error `json:"-"`
	TimeoutMs                 int                                        `json:"timeoutMs,omitempty"`
	WebsocketConnectTimeoutMs int                                        `json:"websocketConnectTimeoutMs,omitempty"`
	MaxRetries                int                                        `json:"maxRetries,omitempty"`
	MaxRetryDelayMs           int                                        `json:"maxRetryDelayMs,omitempty"`
	Metadata                  map[string]any                             `json:"metadata,omitempty"`
	Env                       map[string]string                          `json:"env,omitempty"`
	// OutputSchema is a compact JSON Schema the final text answer must
	// satisfy. Providers with native structured output constrain the text
	// response to it; tool calls stay unconstrained. Nil asks for free text.
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}

// BindAPIKey prepares the options' credentials for a request sent to model.
// Credentials without a CredentialRoute were given for model. When APIKeyFor
// resolves a key for model's provider, that key is bound to model's route; if
// the other credentials were bound to another route they are dropped with it,
// since they were never meant for model.
func (o *StreamOptions) BindAPIKey(model Model) {
	route := RouteOf(model)
	if o.CredentialRoute == (Route{}) {
		o.CredentialRoute = route
	}
	if o.APIKeyFor == nil {
		return
	}
	key := o.APIKeyFor(model.Provider)
	if key == "" {
		return
	}
	o.APIKey = key
	if route != o.CredentialRoute {
		o.Headers, o.Query, o.NoAuth = nil, nil, false
		o.CredentialRoute = route
	}
}

// SimpleStreamOptions is the higher-level option bag whose reasoning level is
// mapped to provider-specific thinking config by streamSimple.
type SimpleStreamOptions struct {
	StreamOptions
	Reasoning       string         `json:"reasoning,omitempty"` // minimal|low|medium|high|xhigh
	ThinkingBudgets map[string]int `json:"thinkingBudgets,omitempty"`
	BlobReader      BlobReader     `json:"-"`
}
