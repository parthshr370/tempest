// Package base is the streaming seam between the agent loop and concrete API
// implementations (Anthropic, faux, ...). It is deliberately minimal: no
// dynamic model refresh, no multi-provider dispatch, and no auth strategies.
package base

import (
	"context"
	"net/url"
	"os"
	"strings"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
)

// StreamFunc is the low-level streaming contract. It never reports errors out
// of band — failures are encoded as an `error` event in the returned stream.
// Cancellation is via ctx.
type StreamFunc func(ctx context.Context, model types.Model, c types.Context, opts *types.StreamOptions) *stream.AssistantStream

// SimpleStreamFunc maps a reasoning level to provider thinking config before
// delegating.
type SimpleStreamFunc func(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream

// Provider bundles an API implementation with its model list.
type Provider struct {
	ID           string
	Models       []types.Model
	Stream       StreamFunc
	StreamSimple SimpleStreamFunc
}

// Registry resolves a Provider by a model's API identifier.
type Registry struct {
	byAPI map[string]Provider
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{byAPI: map[string]Provider{}} }

// Register maps an API id (e.g. "anthropic-messages") to its Provider. A later
// Register for the same api replaces the earlier one.
func (r *Registry) Register(api string, p Provider) { r.byAPI[api] = p }

// Resolve returns the Provider registered for the given model API id.
func (r *Registry) Resolve(api string) (Provider, bool) {
	p, ok := r.byAPI[api]
	return p, ok
}

// envKeyByProvider maps a provider id to its API-key environment variables.
// Unlisted providers fall back to a derived <PROVIDER>_API_KEY. Extend as
// providers land.
var envKeysByProvider = map[string][]string{
	"anthropic": {"ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_API_KEY"},
	"google":    {"GEMINI_API_KEY"},
}

// getEnvAPIKey returns the API key for a provider from its known environment
// variable. The deriveEnvVar fallback returns any <PROVIDER>_API_KEY that is
// set. Add an explicit entry or credential guard before adding an OAuth-only
// provider.
func getEnvAPIKey(provider string) string {
	if envVars, ok := envKeysByProvider[provider]; ok {
		for _, envVar := range envVars {
			if value := os.Getenv(envVar); value != "" {
				return value
			}
		}
		return ""
	}
	return os.Getenv(deriveEnvVar(provider))
}

// deriveEnvVar builds a fallback env var name: uppercase the provider id, map any
// non-alphanumeric rune to '_', and suffix "_API_KEY" (e.g. "openai" → OPENAI_API_KEY).
func deriveEnvVar(provider string) string {
	var b strings.Builder
	for _, r := range provider {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - ('a' - 'A'))
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	b.WriteString("_API_KEY")
	return b.String()
}

// WithEnvAPIKey returns options whose APIKey is filled from the provider's
// environment variable when the caller left it blank. A whitespace-only key
// counts as absent, and the function is non-mutating: it returns a shallow copy
// when it fills a key and the original pointer otherwise. A nil opts yields a
// fresh one.
func WithEnvAPIKey(opts *types.StreamOptions, provider string) *types.StreamOptions {
	if opts == nil {
		opts = &types.StreamOptions{}
	}
	if opts.DisableEnvAuth || opts.NoAuth {
		return opts
	}
	if strings.TrimSpace(opts.APIKey) != "" {
		return opts // explicit key: untouched
	}
	key := getEnvAPIKey(provider)
	if key == "" {
		return opts // nothing to fill: untouched
	}
	clone := *opts // shallow copy
	clone.APIKey = key
	return &clone
}

// RequestURL adds configured query parameters after protocol endpoint expansion.
func RequestURL(raw string, query map[string]string) string {
	if len(query) == 0 {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	values := u.Query()
	for key, value := range query {
		values.Set(key, value)
	}
	u.RawQuery = values.Encode()
	return u.String()
}
