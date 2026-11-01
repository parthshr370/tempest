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
	CredentialRoute Route  `json:"-"`
	DisableEnvAuth  bool   `json:"-"`
	Transport       string `json:"transport,omitempty"`
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

// SimpleStreamOptions is the higher-level option bag whose reasoning level is
// mapped to provider-specific thinking config by streamSimple.
type SimpleStreamOptions struct {
	StreamOptions
	Reasoning       string         `json:"reasoning,omitempty"` // minimal|low|medium|high|xhigh
	ThinkingBudgets map[string]int `json:"thinkingBudgets,omitempty"`
	BlobReader      BlobReader     `json:"-"`
}
