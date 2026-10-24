package openai

import (
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/base"
)

// Options configures an OpenAI stream request, embedding the shared StreamOptions.
// ToolChoice is the OpenAI tool_choice value (nil, "auto", "none", or a forced
// tool); ReasoningEffort maps to the reasoning_effort request field.
type Options struct {
	types.StreamOptions
	ToolChoice      any
	ReasoningEffort string
}

const (
	contextSafetyTokens  = 4096
	minMaxTokens         = 1
	defaultOpenAIBaseURL = "https://api.openai.com/v1"
	// cacheRetentionNone is the StreamOptions.CacheRetention value that turns
	// prompt-cache request fields off.
	cacheRetentionNone = "none"
)

// streamTimeouts bounds stream silence for the process lifetime.
var streamTimeouts = base.StreamTimeoutsFromEnv()

// clampMaxTokensToContext lowers maxTokens so it fits within the model's context
// window after the estimated prompt and a safety margin, never below minMaxTokens.
func clampMaxTokensToContext(model types.Model, c types.Context, maxTokens int) int {
	return base.ClampMaxTokens(model, c, maxTokens)
}

// buildBaseOptions derives StreamOptions from the simple options, clamping max
// tokens to the context window and applying apiKey when provided.
func buildBaseOptions(model types.Model, c types.Context, opts *types.SimpleStreamOptions, apiKey string) types.StreamOptions {
	var base types.StreamOptions
	if opts != nil {
		base = opts.StreamOptions
	}
	requestedMaxTokens := model.MaxTokens
	if opts != nil && opts.MaxTokens != 0 {
		requestedMaxTokens = opts.MaxTokens
	}
	base.MaxTokens = clampMaxTokensToContext(model, c, requestedMaxTokens)
	if apiKey != "" {
		base.APIKey = apiKey
	}
	return base
}

func boolPtr(v bool) *bool { return &v }

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
