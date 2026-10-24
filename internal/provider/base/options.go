package base

import (
	"crypto/sha256"
	"encoding/hex"

	"go.harness.dev/harness/internal/engine/estimate"
	"go.harness.dev/harness/internal/engine/types"
)

// PromptCacheKey keeps session keys within OpenAI's 64-character limit.
func PromptCacheKey(sessionID string) string {
	if len(sessionID) <= 64 {
		return sessionID
	}
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:])
}

// NormalizeToolCallID keeps portable IDs intact and hashes IDs the target can't accept.
func NormalizeToolCallID(id string, _ types.Model, _ types.AssistantMessage) string {
	valid := len(id) > 0 && len(id) <= 64
	for i := 0; valid && i < len(id); i++ {
		b := id[i]
		valid = b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-'
	}
	if valid {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// ClampMaxTokens leaves room for the prompt without exceeding the model's output limit.
func ClampMaxTokens(model types.Model, c types.Context, requested int) int {
	if requested <= 0 {
		requested = model.MaxTokens
	}
	if model.MaxTokens > 0 && requested > model.MaxTokens {
		requested = model.MaxTokens
	}
	if model.ContextWindow > 0 {
		available := max(1, model.ContextWindow-estimate.EstimateContextTokens(c).Tokens-4096)
		if requested <= 0 || requested > available {
			requested = available
		}
	}
	return max(1, requested)
}
