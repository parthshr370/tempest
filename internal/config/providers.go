package config

import (
	"maps"
	"slices"

	"go.harness.dev/harness/internal/adapter"
)

// Providers returns endpoint settings for runtime routing. Do not log this map:
// api_key, headers and query may contain credentials.
func (c *ResolvedConfig) Providers() map[string]adapter.ProviderConfig {
	result := make(map[string]adapter.ProviderConfig, len(c.providers))
	for name, p := range c.providers {
		p.Headers = maps.Clone(p.Headers)
		p.Query = maps.Clone(p.Query)
		p.Models = slices.Clone(p.Models)
		result[name] = p
	}
	return result
}

// UntrustedProviderFiles lists project settings files whose provider settings
// the trust gate ignored.
func (c *ResolvedConfig) UntrustedProviderFiles() []string {
	return slices.Clone(c.untrustedProviderFiles)
}

// resolveProviders validates the merged endpoint registry. Project entries
// reach it only through the provider trust gate in resolveLayers.
func resolveProviders(settings fileSettings) (map[string]adapter.ProviderConfig, error) {
	providers := maps.Clone(settings.Providers)
	if providers == nil {
		providers = map[string]adapter.ProviderConfig{}
	}
	if _, err := adapter.Providers(providers); err != nil {
		return nil, err
	}
	return providers, nil
}
