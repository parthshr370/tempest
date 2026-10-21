package catalog

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"
)

// TestEmbeddedCatalogParses guards the invariant that a runtime call can
// never fail: the embedded JSON must unmarshal and carry real content. The
// accessors swallow parse errors by design, so this test is the only place a
// malformed models.json surfaces.
func TestEmbeddedCatalogParses(t *testing.T) {
	var decoded file
	if err := json.Unmarshal(embedded, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.DateOnly, decoded.Provenance.AsOf); err != nil {
		t.Fatalf("invalid provenance as_of: %v", err)
	}
	if u, err := url.ParseRequestURI(decoded.Provenance.GeneratedFrom); err != nil || u.Scheme != "https" {
		t.Fatal("missing provenance generated_from URL")
	}
	var raw struct {
		Models []struct {
			Cost map[string]*float64 `json:"cost"`
		} `json:"models"`
	}
	if err := json.Unmarshal(embedded, &raw); err != nil {
		t.Fatal(err)
	}
	for i, m := range decoded.Models {
		if u, err := url.ParseRequestURI(m.Source); err != nil || u.Scheme != "https" {
			t.Fatalf("%s: missing source URL", m.ID)
		}
		if _, err := time.Parse(time.DateOnly, m.VerifiedAt); err != nil {
			t.Fatalf("%s: invalid verified_at: %v", m.ID, err)
		}
		for _, component := range []string{"input", "output", "cacheRead", "cacheWrite"} {
			if raw.Models[i].Cost[component] == nil {
				t.Fatalf("%s: missing %s price", m.ID, component)
			}
		}
	}
	if AsOf() == "" {
		t.Fatal("catalog as_of stamp is empty")
	}
	providers := Providers()
	if len(providers) == 0 {
		t.Fatal("catalog lists no providers")
	}
	models := Models()
	if len(models) == 0 {
		t.Fatal("catalog lists no models")
	}
	known := make(map[string]bool, len(providers))
	for _, p := range providers {
		if p.ID == "" || p.API == "" || p.KeyEnv == "" {
			t.Fatalf("provider %+v missing id, api, or key_env", p)
		}
		// OpenRouter intentionally shares the openai-completions wire, so api
		// strings may repeat; provider ids and model ids stay unique.
		known[p.ID] = true
	}
	ids := make(map[string]bool, len(models))
	for _, m := range models {
		if !known[m.Provider] {
			t.Fatalf("model %s references unknown provider %s", m.ID, m.Provider)
		}
		key := m.Provider + "/" + m.ID
		if ids[key] {
			t.Fatalf("duplicate model id %s", key)
		}
		ids[key] = true
		if m.ContextWindow <= 0 || m.MaxTokens <= 0 {
			t.Fatalf("model %s has non-positive limits: window=%d max_tokens=%d", m.ID, m.ContextWindow, m.MaxTokens)
		}
		c := m.Cost
		for name, price := range map[string]float64{"input": c.Input, "output": c.Output, "cacheRead": c.CacheRead, "cacheWrite": c.CacheWrite} {
			if price < 0 {
				t.Fatalf("model %s has negative %s price: %v", m.ID, name, price)
			}
		}
	}
}

func TestLookup(t *testing.T) {
	m, ok := Lookup("google", "gemini-3.8-flash")
	if !ok {
		t.Fatal("gemini-2.5-flash missing from catalog")
	}
	if m.ContextWindow <= 0 || m.Cost.Output <= 0 {
		t.Fatalf("catalog entry incomplete: %+v", m)
	}
	if _, ok := Lookup("anthropic", "gpt-5"); ok {
		t.Fatal("gpt-5 must not resolve under anthropic")
	}
	if _, ok := Lookup("mistral", "large"); ok {
		t.Fatal("unknown provider resolved")
	}
}
