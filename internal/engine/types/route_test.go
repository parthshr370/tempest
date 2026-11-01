package types

import "testing"

// A credential is scoped to the endpoint it was issued for. Its scheme and
// host are compared as DNS does, ignoring case; its path is kept exact. On
// Azure the scope is the resource: the deployment or v1 path a route adds
// below it only selects a model. Any other path stays part of the scope, since
// it may itself separate tenants.
func TestRouteOfScopesACredentialToItsEndpoint(t *testing.T) {
	for _, tc := range []struct {
		provider, baseURL, scope string
	}{
		{"openai", "https://api.example/v1", "https://api.example/v1"},
		{"openai", "HTTPS://API.Example/V1", "https://api.example/V1"},
		{"openai", "https://API.example", "https://api.example"},
		{"openai", "https://api.example/openai/deployments/a", "https://api.example/openai/deployments/a"},
		{"azure", "https://resource.example/openai/deployments/a", "https://resource.example"},
		{"azure", "https://RESOURCE.example/openai/deployments/a", "https://resource.example"},
		{"azure", "https://resource.example/proxy/openai/deployments/b", "https://resource.example/proxy"},
		{"azure", "https://resource.example/openai/v1", "https://resource.example"},
		{"azure", "https://resource.example/openai/deployments/a/extra", "https://resource.example/openai/deployments/a/extra"},
		{"azure", "https://resource.example/tenant", "https://resource.example/tenant"},
		{"azure", "https://resource.example/Tenant", "https://resource.example/Tenant"},
		{"bedrock", "https://bedrock-runtime.us-east-1.amazonaws.com", "https://bedrock-runtime.us-east-1.amazonaws.com"},
		{"bedrock", "", ""},
	} {
		got := RouteOf(Model{Provider: tc.provider, BaseURL: tc.baseURL})
		if got != (Route{Provider: tc.provider, Scope: tc.scope}) {
			t.Errorf("RouteOf(%s %s) = %+v, want scope %s", tc.provider, tc.baseURL, got, tc.scope)
		}
	}
}

// A key that names only a provider belongs to one scope of each provider:
// the primary's for the primary's provider, the provider's default route for
// any other.
func TestProviderKeyAppliesAtOneScopePerProvider(t *testing.T) {
	primary := Route{Provider: "anthropic", Scope: "https://primary.example"}
	openAIDefault := Route{Provider: "openai", Scope: "https://openai.example/v1"}
	for _, tc := range []struct {
		name            string
		candidate       Route
		providerDefault Route
		want            bool
	}{
		{"the primary", primary, Route{Provider: "anthropic", Scope: "https://api.anthropic.com"}, true},
		{"the primary's provider at its default", Route{Provider: "anthropic", Scope: "https://api.anthropic.com"}, Route{Provider: "anthropic", Scope: "https://api.anthropic.com"}, false},
		{"the primary's provider elsewhere", Route{Provider: "anthropic", Scope: "https://other.example"}, Route{}, false},
		{"another provider at its default", openAIDefault, openAIDefault, true},
		{"another provider elsewhere", Route{Provider: "openai", Scope: "https://proxy.example/v1"}, openAIDefault, false},
		{"another provider without a default", Route{Provider: "openai", Scope: "https://proxy.example/v1"}, Route{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProviderKeyApplies(primary, tc.candidate, tc.providerDefault); got != tc.want {
				t.Fatalf("ProviderKeyApplies = %v, want %v", got, tc.want)
			}
		})
	}
}

// The credentials on the options were issued for CredentialRoute, the run's
// primary, and stay only on it. APIKeyFor is asked only where
// ProviderKeyApplies places its key, and a key it gives replaces APIKey.
// CredentialRoute never changes, so binding again for another model starts
// from the same primary.
func TestBindAPIKeyKeepsCredentialsWithTheirScope(t *testing.T) {
	primary := Model{Provider: "openai", BaseURL: "https://primary.example"}
	anthropicDefault := Route{Provider: "anthropic", Scope: "https://api.anthropic.com"}
	for _, tc := range []struct {
		name        string
		target      Model
		resolvedKey string
		wantKey     string
		wantDropped bool
		wantAsked   bool
	}{
		{"the primary", primary, "resolved-key", "resolved-key", false, true},
		{"the primary, no key", primary, "", "original-key", false, true},
		{"the primary, host case", Model{Provider: "openai", BaseURL: "https://PRIMARY.example"}, "resolved-key", "resolved-key", false, true},
		{"other provider at its default", Model{Provider: "anthropic", BaseURL: "https://api.anthropic.com"}, "resolved-key", "resolved-key", true, true},
		{"other provider at its default, no key", Model{Provider: "anthropic", BaseURL: "https://api.anthropic.com"}, "", "", true, true},
		{"other provider elsewhere", Model{Provider: "anthropic", BaseURL: "https://backup.example"}, "resolved-key", "", true, false},
		{"same provider, other endpoint", Model{Provider: "openai", BaseURL: "https://backup.example"}, "resolved-key", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := false
			options := StreamOptions{
				CredentialRoute: RouteOf(primary),
				APIKey:          "original-key",
				NoAuth:          true,
				Headers:         map[string]string{"X-Request": "original-header"},
				Query:           map[string]string{"parameter": "original-query"},
				APIKeyFor: func(provider string) string {
					asked = true
					if provider != tc.target.Provider {
						t.Errorf("asked for %q, want %q", provider, tc.target.Provider)
					}
					return tc.resolvedKey
				},
			}
			options.BindAPIKey(tc.target, anthropicDefault)
			if asked != tc.wantAsked {
				t.Fatalf("APIKeyFor asked = %v, want %v", asked, tc.wantAsked)
			}
			if options.APIKey != tc.wantKey || options.CredentialRoute != RouteOf(primary) {
				t.Fatalf("key %q with route %+v, want %q with the primary's", options.APIKey, options.CredentialRoute, tc.wantKey)
			}
			dropped := options.Headers == nil && options.Query == nil && !options.NoAuth
			kept := options.Headers["X-Request"] == "original-header" && options.Query["parameter"] == "original-query" && options.NoAuth
			if tc.wantDropped && !dropped || !tc.wantDropped && !kept {
				t.Fatalf("request metadata dropped = %v, want %v", dropped, tc.wantDropped)
			}
		})
	}
}

// Credentials with no route were given for the model the request is sent to.
func TestBindAPIKeyTreatsUnroutedCredentialsAsTheModels(t *testing.T) {
	model := Model{Provider: "azure", BaseURL: "https://resource.example/openai/deployments/a"}
	options := StreamOptions{APIKey: "original-key", Headers: map[string]string{"X-Request": "original-header"}, APIKeyFor: func(string) string { return "resolved-key" }}
	options.BindAPIKey(model, Route{})
	if options.APIKey != "resolved-key" || options.Headers["X-Request"] != "original-header" || options.CredentialRoute != (Route{}) {
		t.Fatalf("key %q, headers %v, route %+v", options.APIKey, options.Headers, options.CredentialRoute)
	}
}
