package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
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

// UntrustedProviderFiles lists project settings ignored at the credential boundary.
func (c *ResolvedConfig) UntrustedProviderFiles() []string {
	return slices.Clone(c.untrustedProviderFiles)
}

func resolveProviders(settings fileSettings, cwd string, trusted, skipProject bool) (map[string]adapter.ProviderConfig, []string, error) {
	providers := maps.Clone(settings.Providers)
	if providers == nil {
		providers = map[string]adapter.ProviderConfig{}
	}
	if skipProject {
		_, err := adapter.Providers(providers)
		return providers, nil, err
	}
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, nil, err
		}
	}
	root, err := filepath.Abs(cwd)
	if err != nil {
		return nil, nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if errors.Is(err, os.ErrNotExist) {
		_, err = adapter.Providers(providers)
		return providers, nil, err
	}
	if err != nil {
		return nil, nil, err
	}
	for _, path := range settings.TrustedProviderProjects {
		if !filepath.IsAbs(path) {
			continue
		}
		canonical, err := filepath.EvalSymlinks(path)
		if err == nil && canonical == root {
			trusted = true
		}
	}
	var skipped []string
	for _, name := range []string{"settings.json", "settings.local.json"} {
		path := filepath.Join(root, ProjectConfigDirName, name)
		if !trusted {
			if _, err := os.Lstat(path); err == nil {
				skipped = append(skipped, path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, nil, err
			}
			continue
		}
		project, err := loadSettings(path)
		if err != nil {
			return nil, nil, err
		}
		// A trusted project can override user definitions. Untrusted files cannot
		// redirect a built-in endpoint or inherit a user's credential, even no_auth.
		for name, p := range project.Providers {
			providers[name] = p
		}
	}
	if _, err := adapter.Providers(providers); err != nil {
		return nil, nil, fmt.Errorf("providers: %w", err)
	}
	return providers, skipped, nil
}
