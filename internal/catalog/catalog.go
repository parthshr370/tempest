// Package catalog holds the embedded model catalog: the providers tempest
// knows about and the models each one serves, with context windows, output
// limits, and per-million pricing. The data ships in the binary (go:embed) so
// doctor and the adapter router answer without network access or a config
// file. Catalog entries are defaults, not authority: an unknown model id still
// routes, it just keeps the adapter's hardcoded fallbacks.
package catalog

import (
	_ "embed"
	"encoding/json"
	"sync"

	"go.harness.dev/harness/internal/engine/types"
)

// embedded holds models.json, parsed once by load.
//
//go:embed models.json
var embedded []byte

// Provider describes a model vendor: which wire API it speaks, which
// environment variable holds its credential, and its default endpoint.
type Provider struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	API     string `json:"api"`
	KeyEnv  string `json:"key_env"`
	BaseURL string `json:"base_url"`
}

// Model is one catalog entry: where the model lives (Provider+ID), what it
// costs, and the limits the adapter uses when routing to it.
type Model struct {
	Provider      string          `json:"provider"`
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	ContextWindow int             `json:"context_window"`
	MaxTokens     int             `json:"max_tokens"`
	Cost          types.ModelCost `json:"cost"`
	Capabilities  []string        `json:"capabilities"`
}

// file mirrors models.json on disk. asOf records the pricing date so future
// readers know how stale the numbers are.
type file struct {
	AsOf      string     `json:"as_of"`
	Providers []Provider `json:"providers"`
	Models    []Model    `json:"models"`
}

var (
	once    sync.Once
	data    file
	loadErr error
)

// load parses the embedded JSON exactly once. A malformed file must never
// fail a runtime call, so the error is retained for tests only and every
// accessor degrades to an empty catalog.
func load() {
	once.Do(func() {
		loadErr = json.Unmarshal(embedded, &data)
		if loadErr != nil {
			data = file{}
		}
	})
}

// AsOf reports the pricing snapshot date stamped in models.json.
func AsOf() string {
	load()
	return data.AsOf
}

// Providers returns every provider in the catalog, in file order.
func Providers() []Provider {
	load()
	return data.Providers
}

// Models returns every model in the catalog, in file order.
func Models() []Model {
	load()
	return data.Models
}

// Lookup finds one model by provider id and model id. The second result is
// false when either the provider or the model id is unknown.
func Lookup(provider, modelID string) (Model, bool) {
	for _, m := range Models() {
		if m.Provider == provider && m.ID == modelID {
			return m, true
		}
	}
	return Model{}, false
}
