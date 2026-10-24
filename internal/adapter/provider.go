package adapter

import (
	"context"
	"fmt"
	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/obs"
	"go.harness.dev/harness/internal/provider/anthropic"
	"go.harness.dev/harness/internal/provider/base"
	"go.harness.dev/harness/internal/provider/bedrock"
	"go.harness.dev/harness/internal/provider/google"
	"go.harness.dev/harness/internal/provider/openai"
	"go.harness.dev/harness/internal/provider/responses"
	"go.harness.dev/harness/internal/provider/vertex"
	"go.harness.dev/harness/internal/retry"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
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
	// MaxTokensDefaulted keeps inherited harness defaults distinct from user caps.
	MaxTokensDefaulted bool
	Env                func(string) string
	BlobReader         types.BlobReader
	Retry              *retry.Config
	Providers          map[string]ProviderConfig
	// ChatGPTCredentials is explicit so SDK users never inherit ambient sign-in.
	ChatGPTCredentials func(context.Context) (RequestCredential, error)
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
	if err := ValidateModelSpecWithProviders(firstNonEmpty(cfg.ModelID, env("ANTHROPIC_DEFAULT_SONNET_MODEL"), "claude-opus-4-8"), cfg.Providers); err != nil {
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
	registry.Register("openai-responses", base.Provider{ID: "openai-responses", StreamSimple: responses.StreamSimple})
	registry.Register("bedrock-converse-stream", base.Provider{ID: "bedrock", StreamSimple: bedrock.StreamSimple})
	registry.Register("google-vertex", base.Provider{ID: "vertex", StreamSimple: vertex.StreamSimple})
	p, ok := registry.Resolve(rt.API)
	if !ok || p.StreamSimple == nil {
		return ProviderRouter{}, fmt.Errorf("no provider registered for model API %q", rt.API)
	}
	r := ProviderRouter{Registry: registry, Model: rt.Model}
	r = r.withStream(p.StreamSimple, rt, cfg.BlobReader)
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
func (r ProviderRouter) withStream(streamSimple base.SimpleStreamFunc, rt route, blobReader types.BlobReader) ProviderRouter {
	var reasoningWarning sync.Once
	r.StreamFn = func(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
		if streamSimple == nil {
			return erroredStream(model, fmt.Errorf("provider stream not configured for model API %q", model.API))
		}
		resolved := cloneSimpleOptions(opts)
		if rt.CredentialSource != nil {
			if model.Provider != "openai-chatgpt" || model.API != rt.Model.API || model.BaseURL != rt.Model.BaseURL || len(model.Headers) != 0 || resolved.APIKey != "" || len(resolved.Headers) != 0 || len(resolved.Query) != 0 || resolved.NoAuth {
				return erroredStream(model, fmt.Errorf("openai-chatgpt does not allow credential, endpoint or header overrides"))
			}
			credential, err := rt.CredentialSource(ctx)
			if err != nil {
				return erroredStream(model, err)
			}
			if err := validateChatGPTCredential(credential); err != nil {
				return erroredStream(model, err)
			}
			model.BaseURL = credential.BaseURL
			resolved.APIKey = credential.AccessToken
		}
		if rt.CredentialSource != nil {
			// Internal caps drive compaction and overflow recovery, not plan requests.
			resolved.MaxTokens = 0
			resolved.Temperature = nil
		} else {
			requested := model.MaxTokens
			if resolved.MaxTokens > 0 {
				requested = resolved.MaxTokens
			}
			resolved.MaxTokens = base.ClampMaxTokens(model, c, requested)
		}
		resolved.DisableEnvAuth = !rt.Provider.UsesCredentialChain()
		resolved.NoAuth = rt.Provider.NoAuth != nil && *rt.Provider.NoAuth
		if resolved.Headers == nil && (len(rt.Provider.Headers) > 0 || rt.AuthToken != "" || rt.Provider.AuthHeader != "") {
			resolved.Headers = map[string]string{}
		}
		for key, value := range rt.Provider.Headers {
			if !hasHeader(resolved.Headers, key) {
				putHeader(resolved.Headers, key, value)
			}
		}
		if resolved.Query == nil && (len(rt.Provider.Query) > 0 || rt.Provider.AuthQuery != "") {
			resolved.Query = map[string]string{}
		}
		for key, value := range rt.Provider.Query {
			if _, ok := resolved.Query[key]; !ok {
				resolved.Query[key] = value
			}
		}
		if resolved.Reasoning != "" && !model.Reasoning {
			// A reasoning field the model doesn't support is rejected by
			// strict endpoints, so it never reaches the wire.
			reasoningWarning.Do(func() {
				slog.Warn("reasoning effort ignored: the model has no reasoning support in the catalog", "provider", model.Provider, "model", model.ID, "reasoning", resolved.Reasoning)
			})
			resolved.Reasoning = ""
		}
		if blobReader != nil {
			resolved.BlobReader = blobReader
		}
		if strings.TrimSpace(resolved.APIKey) == "" {
			resolved.APIKey = rt.APIKey
		}
		if resolved.APIKey == "" && rt.AuthToken != "" && !hasHeader(resolved.Headers, "authorization") {
			putHeader(resolved.Headers, "authorization", "Bearer "+rt.AuthToken)
		}
		auth := rt.Provider
		requestKey := resolved.APIKey
		auth.APIKey = resolved.APIKey
		auth.Headers = resolved.Headers
		auth.Query = resolved.Query
		auth.NoAuth = new(resolved.NoAuth)
		if !auth.CredentialPresent(nil) && !rt.Provider.UsesCredentialChain() {
			return erroredStream(model, rt.Provider.credentialError())
		}
		if rt.Provider.AuthQuery != "" && resolved.APIKey != "" {
			resolved.Query[rt.Provider.AuthQuery] = resolved.APIKey
			resolved.APIKey = ""
		}
		if rt.Provider.AuthHeader != "" && resolved.APIKey != "" {
			prefix := "Bearer "
			if rt.Provider.AuthPrefix != nil {
				prefix = *rt.Provider.AuthPrefix
			}
			putHeader(resolved.Headers, rt.Provider.AuthHeader, prefix+resolved.APIKey)
			resolved.APIKey = ""
		}
		if rt.Provider.AuthHeader != "" || rt.Provider.AuthQuery != "" || resolved.Query["key"] != "" || resolved.Query["api-key"] != "" {
			resolved.NoAuth = true
		}
		values := []string{rt.APIKey, rt.AuthToken, rt.Provider.APIKey, requestKey}
		for key, value := range resolved.Headers {
			values = append(values, value)
			if strings.EqualFold(key, "authorization") || strings.EqualFold(key, rt.Provider.AuthHeader) {
				prefix := "Bearer "
				if rt.Provider.AuthPrefix != nil {
					prefix = *rt.Provider.AuthPrefix
				}
				if prefix != "" {
					values = append(values, strings.TrimPrefix(value, prefix))
				}
			}
		}
		for _, value := range resolved.Query {
			values = append(values, value)
		}
		source := streamSimple(ctx, model, c, resolved)
		clean := stream.NewAssistantStream(model.API, model.Provider, model.ID)
		go func() {
			defer clean.End()
			for event := range source.Events() {
				if event.Err != nil {
					message := *event.Err
					message.ErrorMessage = obs.RedactValues(message.ErrorMessage, values...)
					message.ErrorDetails = redactErrorDetails(message.ErrorDetails, values)
					event.Err = &message
				}
				if event.Message != nil && event.Message.ErrorMessage != "" {
					message := *event.Message
					message.ErrorMessage = obs.RedactValues(message.ErrorMessage, values...)
					message.ErrorDetails = redactErrorDetails(message.ErrorDetails, values)
					event.Message = &message
				}
				clean.Push(event)
			}
		}()
		return clean
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

func redactErrorDetails(details map[string]any, values []string) map[string]any {
	if details == nil {
		return nil
	}
	out := make(map[string]any, len(details))
	for key, value := range details {
		out[obs.RedactValues(key, values...)] = redactErrorValue(value, values)
	}
	return out
}

func redactErrorValue(value any, values []string) any {
	switch v := value.(type) {
	case string:
		return obs.RedactValues(v, values...)
	case map[string]any:
		return redactErrorDetails(v, values)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactErrorValue(item, values)
		}
		return out
	default:
		return value
	}
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
	if opts.Query != nil {
		clone.Query = make(map[string]string, len(opts.Query))
		for key, value := range opts.Query {
			clone.Query[key] = value
		}
	}
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
