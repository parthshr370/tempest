package catalog

import "testing"

// TestEmbeddedCatalogParses guards the invariant that a runtime call can
// never fail: the embedded JSON must unmarshal and carry real content. The
// accessors swallow parse errors by design, so this test is the only place a
// malformed models.json surfaces.
func TestEmbeddedCatalogParses(t *testing.T) {
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
	seenAPI := make(map[string]string, len(providers))
	for _, p := range providers {
		if p.ID == "" || p.API == "" || p.KeyEnv == "" {
			t.Fatalf("provider %+v missing id, api, or key_env", p)
		}
		if other, dup := seenAPI[p.API]; dup {
			t.Fatalf("providers %s and %s share api %s", other, p.ID, p.API)
		}
		seenAPI[p.API] = p.ID
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
	m, ok := Lookup("google", "gemini-2.5-flash")
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
