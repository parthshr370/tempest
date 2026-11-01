package config

import (
	"fmt"
	"slices"
	"strings"

	"go.harness.dev/harness/internal/adapter"
)

// Model roles that take a fallback chain.
const (
	RoleDefault = "default"
	RolePlan    = "plan"
	RoleSmol    = "smol"
)

// fallbackRoles lists the roles a fallback_models settings object may name.
var fallbackRoles = []string{RoleDefault, RolePlan, RoleSmol}

// resolveFallbackModels resolves each role's backup model chain from the
// merged fallback_models object. -fallback-model (repeatable) and
// HARNESS_FALLBACK_MODELS (comma separated) set fallback_models.default in
// their layers (settingInputs). A role with no entry of its own inherits the
// default role's chain; an explicit empty list turns fallback off for it.
func resolveFallbackModels(settings map[string][]string) (map[string][]string, error) {
	for role := range settings {
		if !slices.Contains(fallbackRoles, role) {
			return nil, &Error{Field: "fallback_models", Err: fmt.Errorf("unknown role %q (valid roles: %s)", role, strings.Join(fallbackRoles, ", "))}
		}
	}
	chains := map[string][]string{RoleDefault: trimModels(settings[RoleDefault])}
	for _, role := range []string{RolePlan, RoleSmol} {
		chain, ok := settings[role]
		if !ok {
			chain = chains[RoleDefault]
		}
		chains[role] = trimModels(chain)
	}
	return chains, nil
}

func trimModels(models []string) []string {
	trimmed := make([]string, 0, len(models))
	for _, model := range models {
		trimmed = append(trimmed, strings.TrimSpace(model))
	}
	return trimmed
}

// validateFallbackModels rejects a backup spec the router could not route.
func validateFallbackModels(c *ResolvedConfig) error {
	for _, role := range fallbackRoles {
		for i, model := range c.fallbackModels[role] {
			field := fmt.Sprintf("fallback_models.%s[%d]", role, i)
			if err := adapter.ValidateModelSpecWithProviders(model, c.providers); err != nil {
				return &Error{Field: field, Err: err}
			}
			if c.maxTokensConfigured && strings.HasPrefix(model, "openai-chatgpt:") {
				return &Error{Field: "max_tokens", Err: fmt.Errorf("%s: ChatGPT plan usage does not support explicit output-token caps; use turn or wall-time limits", field)}
			}
		}
	}
	return nil
}

// FallbackModels returns the backup model specs for a role ("default",
// "plan" or "smol"), tried in order when the role's model is unavailable.
func (c *ResolvedConfig) FallbackModels(role string) []string {
	return slices.Clone(c.fallbackModels[role])
}
