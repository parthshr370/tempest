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
	// CredentialRoute is the route (credential scope, see [RouteOf]) of the
	// run's primary model: APIKey, NoAuth, Headers and Query were issued for
	// it, and it decides where APIKeyFor's keys belong (see [ProviderKeyApplies]).
	// It names the primary for the whole run, whichever model a request is
	// sent to, so nothing rewrites it while a request moves between models.
	// Zero means the route of the model the request is sent with; a stream
	// function that forwards a request to a different model sets it first.
	CredentialRoute Route `json:"-"`
	// APIKeyFor, when set, resolves the API key for a provider (the agent's
	// GetApiKey). The provider route a request is sent through applies it
	// with [StreamOptions.BindAPIKey], so a backup's key is resolved the same
	// way as its primary's. It may be called from the stream's goroutine.
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

// ProviderKeyApplies reports whether a key that names only a provider (an
// APIKeyFor answer) belongs to candidate in a run whose primary is primary.
// Such a key cannot tell two scopes of one provider apart, so it belongs to
// exactly one scope per provider for the whole run: the primary's scope for
// the primary's provider, and for any other provider the scope its route
// resolves to without an inline endpoint, providerDefault (zero when there is
// none). Every other scope of a provider authenticates with its own route's
// credential. The answer depends on these three routes alone, never on the
// order a run reaches its models.
func ProviderKeyApplies(primary, candidate, providerDefault Route) bool {
	if candidate.Provider == primary.Provider {
		return candidate == primary
	}
	return candidate == providerDefault
}

// BindAPIKey prepares the options' credentials for a request sent to model,
// whose provider's default route is providerDefault (see
// [ProviderKeyApplies]). The credentials issued for CredentialRoute (APIKey,
// NoAuth, Headers, Query) stay only when model is on that route; anywhere
// else they are dropped. APIKeyFor is asked for model's key only where
// [ProviderKeyApplies] places it, and a key it gives replaces APIKey. Without
// a key, model's provider route authenticates with its own credential.
// CredentialRoute is left as it is.
func (o *StreamOptions) BindAPIKey(model Model, providerDefault Route) {
	route := RouteOf(model)
	primary := o.CredentialRoute
	if primary == (Route{}) {
		primary = route
	}
	if route != primary {
		o.APIKey, o.Headers, o.Query, o.NoAuth = "", nil, nil, false
	}
	if o.APIKeyFor == nil || !ProviderKeyApplies(primary, route, providerDefault) {
		return
	}
	if key := o.APIKeyFor(model.Provider); key != "" {
		o.APIKey = key
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
