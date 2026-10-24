package google

import (
	"go.harness.dev/harness/internal/engine/estimate"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/base"
)

// Options configures a Gemini stream, extending the shared StreamOptions with
// Google-specific controls: ToolChoice selects the function-calling mode
// ("AUTO", "NONE", "ANY") and Thinking configures reasoning output (nil = default).
type Options struct {
	types.StreamOptions
	ToolChoice any
	Thinking   *thinkingConfig
}

// thinkingConfig controls Gemini reasoning output. Enabled turns reasoning on for
// reasoning-capable models; Level (a named level) takes precedence over
// BudgetTokens, which otherwise caps the reasoning token budget.
type thinkingConfig struct {
	Enabled      bool
	BudgetTokens int
	Level        string
}

const (
	contextSafetyTokens  = 4096
	minMaxTokens         = 1
	defaultGoogleBaseURL = "https://generativelanguage.googleapis.com"
)

// streamTimeouts bounds stream silence for the process lifetime.
var streamTimeouts = base.StreamTimeoutsFromEnv()

// clampMaxTokensToContext lowers maxTokens so the request plus a safety margin
// fits inside the model's context window, never dropping below minMaxTokens.
func clampMaxTokensToContext(model types.Model, c types.Context, maxTokens int) int {
	if model.ContextWindow <= 0 {
		return maxInt(minMaxTokens, maxTokens)
	}
	available := model.ContextWindow - estimate.EstimateContextTokens(c).Tokens - contextSafetyTokens
	return minInt(maxTokens, maxInt(minMaxTokens, available))
}

// buildBaseOptions derives StreamOptions from simple options, clamping the
// requested (or model-default) max tokens and applying the API key.
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
