package adapter

import (
	"context"
	"fmt"
	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/catalog"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/anthropic"
	"go.harness.dev/harness/internal/provider/base"
	"go.harness.dev/harness/internal/provider/google"
	"go.harness.dev/harness/internal/provider/openai"
	"go.harness.dev/harness/internal/retry"
	"os"
	"strconv"
	"strings"
	"time"
)

// AnthropicMessagesAPI is the provider API id for the Anthropic Messages protocol.
const AnthropicMessagesAPI = "anthropic-messages"

// OpenAICompletionsAPI is the provider API id for the OpenAI chat-completions
// protocol.
const OpenAICompletionsAPI = "openai-completions"

// GoogleGenerativeAPI is the provider API id for the Google Generative AI protocol.
const GoogleGenerativeAPI = "google-generative-ai"

// defaultMaxTokens is the model output-token cap. 4096 (the old value) truncated
// a plan turn's PRD + mandatory App Mockup and would truncate a large build
// page.tsx (stop_reason "length"). 32000 fits the Sonnet-4 / Haiku-4.5 output
// limit (<=64000) and stays inside the 200k context window after clamping.
const defaultMaxTokens = 32000

// DefaultMaxTokens is the output-token cap applied when configuration omits one.
const DefaultMaxTokens = defaultMaxTokens

// resolveMaxTokens returns the model output-token cap, overridable via
// HARNESS_MAX_TOKENS (positive int), else defaultMaxTokens.
func resolveMaxTokens(env func(string) string) int {
	if env != nil {
		if v := strings.TrimSpace(env("HARNESS_MAX_TOKENS")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return n
			}
		}
	}
	return defaultMaxTokens
}

// RoutingConfig holds the environmental inputs for building a provider router.
// All fields are optional; missing values fall back to env vars or defaults.
type RoutingConfig struct {
	ModelID          string
	BaseURL          string
	AnthropicBaseURL string
	OpenAIBaseURL    string
	APIKey           string
	AuthToken        string
	MaxTokens        int
	Env              func(string) string
	BlobReader       types.BlobReader
	Retry            *retry.Config
}

// ProviderRouter bundles the resolved provider registry, the default model,
// and the stream function that wires the agent loop to the base.
type ProviderRouter struct {
	Registry *base.Registry
	Model    types.Model
	StreamFn agent.StreamFn
}

// NewProviderRouter resolves the provider configuration from the given config
// and environment, then wires the agent loop to the resolved provider through
// the registry. The model string selects the route (see resolveRoute):
// "anthropic" (the default, unchanged) streams via the Anthropic Messages
// protocol; "openai:model@url" uses an explicit OpenAI-compatible endpoint.
// An unsupported provider or an API with no registered stream is a
// deterministic configuration error.
func NewProviderRouter(cfg RoutingConfig) (ProviderRouter, error) {
	env := cfg.Env
	if env == nil {
		env = os.Getenv
	}
	if err := ValidateModelSpec(firstNonEmpty(cfg.ModelID, env("ANTHROPIC_DEFAULT_SONNET_MODEL"), "claude-opus-4-8")); err != nil {
		return ProviderRouter{}, err
	}
	rt, err := resolveRoute(cfg, env)
	if err != nil {
		return ProviderRouter{}, err
	}
	registry := base.NewRegistry()
	registry.Register(AnthropicMessagesAPI, base.Provider{ID: "anthropic", Models: []types.Model{rt.Model}, StreamSimple: anthropic.StreamSimple})
	registry.Register(OpenAICompletionsAPI, base.Provider{ID: "openai", Models: []types.Model{rt.Model}, StreamSimple: openai.StreamSimple})
	registry.Register(GoogleGenerativeAPI, base.Provider{ID: "google", Models: []types.Model{rt.Model}, StreamSimple: google.StreamSimple})
	p, ok := registry.Resolve(rt.API)
	if !ok || p.StreamSimple == nil {
		return ProviderRouter{}, fmt.Errorf("no provider registered for model API %q", rt.API)
	}
	r := ProviderRouter{Registry: registry, Model: rt.Model}
	r = r.withStream(p.StreamSimple, rt.APIKey, rt.AuthToken, cfg.BlobReader)
	if cfg.Retry != nil {
		retrier, err := retry.New(retry.StreamFunc(r.StreamFn), *cfg.Retry)
		if err != nil {
			return ProviderRouter{}, fmt.Errorf("retry policy: %w", err)
		}
		r.StreamFn = agent.StreamFn(retrier.Stream)
	}
	return r, nil
}

// withStream binds the resolved provider's StreamSimple to StreamFn, filling the
// API key (or an authorization bearer header from authToken) and blob reader
// that the caller left unset. A nil streamSimple can only arise from programmer
// error since NewProviderRouter validates it, so it surfaces as a terminal error
// event rather than a panic.
func (r ProviderRouter) withStream(streamSimple base.SimpleStreamFunc, apiKey, authToken string, blobReader types.BlobReader) ProviderRouter {
	r.StreamFn = func(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
		if streamSimple == nil {
			return erroredStream(model, fmt.Errorf("provider stream not configured for model API %q", model.API))
		}
		resolved := cloneSimpleOptions(opts)
		if blobReader != nil {
			resolved.BlobReader = blobReader
		}
		if strings.TrimSpace(resolved.APIKey) == "" && apiKey != "" {
			resolved.APIKey = apiKey
		}
		if strings.TrimSpace(resolved.APIKey) == "" && authToken != "" && !hasHeader(resolved.Headers, "authorization") {
			if resolved.Headers == nil {
				resolved.Headers = map[string]string{}
			}
			resolved.Headers["authorization"] = "Bearer " + authToken
		}
		return streamSimple(ctx, model, c, resolved)
	}
	return r
}

// erroredStream returns an AssistantStream carrying a single terminal error
// event, so a misconfigured route surfaces deterministically through the normal
// streaming path instead of panicking on a nil StreamSimple.
func erroredStream(model types.Model, err error) *stream.AssistantStream {
	s := stream.NewAssistantStream(model.API, model.Provider, model.ID)
	s.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopError, Err: &types.AssistantMessage{
		Content:      []types.ContentBlock{},
		API:          model.API,
		Provider:     model.Provider,
		Model:        model.ID,
		StopReason:   types.StopError,
		ErrorMessage: err.Error(),
		Timestamp:    time.Now().UnixMilli(),
	}})
	s.End()
	return s
}

// ResolveProviderForModel returns the provider for a bare model name. The
// embedded catalog decides by exact id; id-shape prefixes cover unknown ids;
// slash-prefixed ids ("openai/gpt-4o") keep the Anthropic proxy route the
// router has always given them, as does anything unrecognized.
func ResolveProviderForModel(modelID string) string {
	trimmed := strings.TrimSpace(strings.ToLower(modelID))
	for _, prov := range []string{"anthropic", "openai", "google"} {
		if _, ok := catalog.Lookup(prov, trimmed); ok {
			return prov
		}
	}
	switch {
	case strings.HasPrefix(trimmed, "gemini-"):
		return "google"
	case strings.HasPrefix(trimmed, "gpt-"), strings.HasPrefix(trimmed, "o1"), strings.HasPrefix(trimmed, "o3"), strings.HasPrefix(trimmed, "o4"):
		return "openai"
	case strings.HasPrefix(trimmed, "claude-"):
		return "anthropic"
	}
	return "anthropic"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// cloneSimpleOptions keeps per-stream credential and header defaults from mutating caller-owned options.
func cloneSimpleOptions(opts *types.SimpleStreamOptions) *types.SimpleStreamOptions {
	if opts == nil {
		return &types.SimpleStreamOptions{}
	}
	clone := *opts
	if opts.Headers != nil {
		clone.Headers = map[string]string{}
		for key, value := range opts.Headers {
			clone.Headers[key] = value
		}
	}
	return &clone
}

// hasHeader treats header names case-insensitively and ignores blank values before adding authorization.
func hasHeader(headers map[string]string, name string) bool {
	for key, value := range headers {
		if strings.EqualFold(key, name) && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}
