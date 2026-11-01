package types

import "testing"

// A credential is scoped to the endpoint it was issued for. On Azure that is
// the resource: the deployment or v1 path a route adds below it only selects a
// model. Any other path stays part of the scope, since it may itself separate
// tenants.
func TestRouteOfScopesACredentialToItsEndpoint(t *testing.T) {
	for _, tc := range []struct {
		provider, baseURL, scope string
	}{
		{"openai", "https://api.example/v1", "https://api.example/v1"},
		{"openai", "https://api.example/openai/deployments/a", "https://api.example/openai/deployments/a"},
		{"azure", "https://resource.example/openai/deployments/a", "https://resource.example"},
		{"azure", "https://resource.example/proxy/openai/deployments/b", "https://resource.example/proxy"},
		{"azure", "https://resource.example/openai/v1", "https://resource.example"},
		{"azure", "https://resource.example/openai/deployments/a/extra", "https://resource.example/openai/deployments/a/extra"},
		{"azure", "https://resource.example/tenant", "https://resource.example/tenant"},
		{"bedrock", "https://bedrock-runtime.us-east-1.amazonaws.com", "https://bedrock-runtime.us-east-1.amazonaws.com"},
	} {
		got := RouteOf(Model{Provider: tc.provider, BaseURL: tc.baseURL})
		if got != (Route{Provider: tc.provider, Scope: tc.scope}) {
			t.Errorf("RouteOf(%s %s) = %+v, want scope %s", tc.provider, tc.baseURL, got, tc.scope)
		}
	}
}

// APIKeyFor answers per provider. Its key for the provider of the route the
// credentials are bound to is that route's key, so it is bound only to that
// route; another provider's key is bound to the model it is asked for. A
// model on the bound provider at another scope is not asked about and keeps
// the old binding, which the provider route then drops in favour of its own
// credential. Request metadata stays with its own route.
func TestBindAPIKeyBindsAProviderKeyOnlyToItsScope(t *testing.T) {
	primary := Model{Provider: "openai", BaseURL: "https://primary.example"}
	for _, tc := range []struct {
		name        string
		target      Model
		resolvedKey string
		wantKey     string
		wantRoute   Route
		wantDropped bool
		wantAsked   bool
	}{
		{"same route", Model{Provider: "openai", BaseURL: "https://primary.example"}, "resolved-key", "resolved-key", RouteOf(primary), false, true},
		{"same route, no key", Model{Provider: "openai", BaseURL: "https://primary.example"}, "", "original-key", RouteOf(primary), false, true},
		{"other provider", Model{Provider: "anthropic", BaseURL: "https://backup.example"}, "resolved-key", "resolved-key", Route{Provider: "anthropic", Scope: "https://backup.example"}, true, true},
		{"other provider, no key", Model{Provider: "anthropic", BaseURL: "https://backup.example"}, "", "original-key", RouteOf(primary), false, true},
		{"same provider, other endpoint", Model{Provider: "openai", BaseURL: "https://backup.example"}, "resolved-key", "original-key", RouteOf(primary), false, false},
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
			options.BindAPIKey(tc.target)
			if asked != tc.wantAsked {
				t.Fatalf("APIKeyFor asked = %v, want %v", asked, tc.wantAsked)
			}
			if options.APIKey != tc.wantKey || options.CredentialRoute != tc.wantRoute {
				t.Fatalf("bound key %q to %+v, want %q to %+v", options.APIKey, options.CredentialRoute, tc.wantKey, tc.wantRoute)
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
func TestBindAPIKeyBindsUnroutedCredentialsToTheModel(t *testing.T) {
	model := Model{Provider: "azure", BaseURL: "https://resource.example/openai/deployments/a"}
	options := StreamOptions{APIKeyFor: func(string) string { return "resolved-key" }}
	options.BindAPIKey(model)
	if options.APIKey != "resolved-key" || options.CredentialRoute != RouteOf(model) {
		t.Fatalf("bound key %q to %+v", options.APIKey, options.CredentialRoute)
	}
}
