package adapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/retry"
)

func TestResolveRolesUsesFallbacksAndFullRoutes(t *testing.T) {
	roles, err := ResolveRoles(RoleRoutingConfig{
		DefaultModel:     "openai:gpt-default@https://default.example",
		PlanModel:        "anthropic:plan-model@https://plan.example",
		AnthropicBaseURL: "https://anthropic.example",
		OpenAIBaseURL:    "https://openai.example",
		MaxTokens:        1234,
		SecretLookup: func(string) (string, bool) {
			return "", false
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if roles.Default.Model.Provider != "openai" || roles.Default.Model.BaseURL != "https://default.example" {
		t.Fatalf("default route = %+v", roles.Default.Model)
	}
	if roles.Plan.Model.Provider != "anthropic" || roles.Plan.Model.BaseURL != "https://plan.example" {
		t.Fatalf("plan route = %+v", roles.Plan.Model)
	}
	if roles.Smol.Model.ID != roles.Default.Model.ID || roles.Smol.Model.Provider != "openai" {
		t.Fatalf("smol fallback = %+v, default = %+v", roles.Smol.Model, roles.Default.Model)
	}
	if roles.Default.Model.MaxTokens != 1234 || roles.Plan.Model.MaxTokens != 1234 {
		t.Fatalf("max tokens were not threaded: default=%d plan=%d", roles.Default.Model.MaxTokens, roles.Plan.Model.MaxTokens)
	}
}

func TestRoleCredentialsPerProvider(t *testing.T) {
	lookup := func(key string) (string, bool) {
		switch key {
		case "GEMINI_API_KEY":
			return "gem-key", true
		case "OPENAI_API_KEY":
			return "oai-key", true
		case "ANTHROPIC_API_KEY":
			return "ant-key", true
		}
		return "", false
	}
	tests := map[string][2]string{
		"google:gemini-2.5-flash": {"gem-key", ""},
		"gemini-2.5-flash":        {"gem-key", ""},
		"openai:gpt-5":            {"oai-key", ""},
		"claude-opus-4-8":         {"ant-key", ""},
	}
	for model, want := range tests {
		got, _ := roleCredentials(model, lookup)
		if got != want[0] {
			t.Fatalf("roleCredentials(%q) key = %q, want %q", model, got, want[0])
		}
	}
	// GOOGLE_API_KEY is the google fallback when GEMINI_API_KEY is absent.
	fallback, _ := roleCredentials("google:gemini-2.5-flash", func(k string) (string, bool) {
		if k == "GOOGLE_API_KEY" {
			return "gcp-key", true
		}
		return "", false
	})
	if fallback != "gcp-key" {
		t.Fatalf("google fallback key = %q", fallback)
	}
}
func TestResolveRolesPropagatesRetryToEveryRealRouter(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt := requests.Add(1)
		if attempt%2 == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeMinimalAnthropicStream(w)
	}))
	defer server.Close()

	sleeper := &adapterRetrySleeper{}
	policy := retry.DefaultConfig()
	policy.Clock = adapterRetryClock{now: time.Unix(1_700_000_000, 0)}
	policy.Sleeper = sleeper
	policy.Random = adapterRetryRandom(1)
	spec := "anthropic:claude-test@" + server.URL
	roles, err := ResolveRoles(RoleRoutingConfig{
		DefaultModel: spec,
		PlanModel:    spec,
		SmolModel:    spec,
		Retry:        &policy,
		SecretLookup: func(string) (string, bool) { return "test", true },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, router := range []ProviderRouter{roles.Default, roles.Plan, roles.Smol} {
		final := drain(t, router.StreamFn(context.Background(), router.Model, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hello")}}}, nil))
		if final.StopReason != types.StopStop {
			t.Fatalf("role stream final=%+v", final)
		}
	}
	if delays := sleeper.Delays(); requests.Load() != 6 || len(delays) != 3 {
		t.Fatalf("retry was not propagated to every real router: requests=%d delays=%v", requests.Load(), delays)
	}
}
