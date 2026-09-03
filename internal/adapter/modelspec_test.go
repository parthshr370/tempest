package adapter

import "testing"

func TestParseModelSpec(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		provider string
		modelID  string
		baseURL  string
	}{
		{"bare model", "claude-opus-4-8", "", "claude-opus-4-8", ""},
		{"slash model no provider", "anthropic/claude-opus-4-8", "", "anthropic/claude-opus-4-8", ""},
		{"provider prefix", "anthropic:claude-sonnet-4", "anthropic", "claude-sonnet-4", ""},
		{"openai with base url", "openai:gpt-4o@https://api.openai.com/v1", "openai", "gpt-4o", "https://api.openai.com/v1"},
		{"empty", "", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseModelSpec(tc.raw)
			if got.Provider != tc.provider || got.ModelID != tc.modelID || got.BaseURL != tc.baseURL {
				t.Fatalf("parseModelSpec(%q) = %+v", tc.raw, got)
			}
		})
	}
}

func TestModelIDFromSpec(t *testing.T) {
	tests := map[string]string{
		"claude-opus-4-8":           "claude-opus-4-8",
		"anthropic:claude-sonnet-4": "claude-sonnet-4",
		"anthropic/claude-opus-4-8": "anthropic/claude-opus-4-8",
	}
	for raw, want := range tests {
		if got := ModelIDFromSpec(raw); got != want {
			t.Fatalf("ModelIDFromSpec(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestResolveRouteDefaultsToAnthropic(t *testing.T) {
	rt, err := resolveRoute(RoutingConfig{}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("resolveRoute: %v", err)
	}
	if rt.API != AnthropicMessagesAPI || rt.Model.Provider != "anthropic" || rt.Model.ID != "claude-opus-4-8" {
		t.Fatalf("route = %+v", rt)
	}
}

func TestResolveRouteUnknownProviderErrors(t *testing.T) {
	if _, err := resolveRoute(RoutingConfig{ModelID: "mistral:large"}, func(string) string { return "" }); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestValidateModelSpecAcceptsGoogle(t *testing.T) {
	for _, raw := range []string{"google:gemini-2.5-flash", "google:gemini-2.5-pro@https://ex/"} {
		if err := ValidateModelSpec(raw); err != nil {
			t.Fatalf("ValidateModelSpec(%q) = %v", raw, err)
		}
	}
	err := ValidateModelSpec("mistral:large")
	if err == nil || err.Error() != "unsupported model provider; use anthropic, openai, or google" {
		t.Fatalf("unknown provider error = %v", err)
	}
}

func TestResolveRouteGoogle(t *testing.T) {
	rt, err := resolveRoute(RoutingConfig{ModelID: "google:gemini-2.5-flash", APIKey: "key"}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("resolveRoute: %v", err)
	}
	if rt.API != GoogleGenerativeAPI || rt.Model.Provider != "google" || rt.APIKey != "key" {
		t.Fatalf("route = %+v", rt)
	}
	// Catalog-known id: context window and cost come from the catalog.
	if rt.Model.ContextWindow != 1048576 || rt.Model.Cost.Input != 0.3 {
		t.Fatalf("catalog entry not applied: %+v", rt.Model)
	}
	// Unknown google id keeps the hardcoded fallback window.
	unknown, err := resolveRoute(RoutingConfig{ModelID: "google:gemini-x"}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("resolveRoute unknown: %v", err)
	}
	if unknown.Model.ContextWindow != 1048576 || unknown.Model.Cost.Input != 0 {
		t.Fatalf("unknown google fallback = %+v", unknown.Model)
	}
}

func TestResolveRouteCatalogDefaults(t *testing.T) {
	// Known anthropic id with no MaxTokens override: catalog max tokens wins.
	rt, err := resolveRoute(RoutingConfig{ModelID: "claude-haiku-4-5"}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("resolveRoute: %v", err)
	}
	if rt.Model.MaxTokens != 64000 || rt.Model.ContextWindow != 200000 || rt.Model.Cost.Output != 5.0 {
		t.Fatalf("catalog defaults = %+v", rt.Model)
	}
	// HARNESS_MAX_TOKENS beats the catalog.
	override, err := resolveRoute(RoutingConfig{ModelID: "claude-haiku-4-5"}, func(k string) string {
		if k == "HARNESS_MAX_TOKENS" {
			return "1024"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("resolveRoute override: %v", err)
	}
	if override.Model.MaxTokens != 1024 {
		t.Fatalf("env override lost: %+v", override.Model)
	}
	// Unknown id keeps the package default.
	fallback, err := resolveRoute(RoutingConfig{ModelID: "claude-unknown"}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("resolveRoute fallback: %v", err)
	}
	if fallback.Model.MaxTokens != DefaultMaxTokens {
		t.Fatalf("default max tokens = %d", fallback.Model.MaxTokens)
	}
}

func TestResolveProviderForModel(t *testing.T) {
	tests := map[string]string{
		"claude-opus-4-8":       "anthropic",
		"gpt-5":                 "openai",
		"o3":                    "openai",
		"gemini-2.5-flash":      "google",
		"google/gemini-2.5-pro": "anthropic",
		"openai/gpt-4o":         "anthropic",
		"totally-unknown":       "anthropic",
	}
	for raw, want := range tests {
		if got := ResolveProviderForModel(raw); got != want {
			t.Fatalf("ResolveProviderForModel(%q) = %q, want %q", raw, got, want)
		}
	}
}
