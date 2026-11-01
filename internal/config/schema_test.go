package config

import (
	"encoding"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/adapter"
)

// The pruner knows a type's members from its fields. A type that decodes
// itself must decode exactly those fields, or the pruner could keep a member
// the decoder reads under another name; each such type is listed here after
// checking its UnmarshalJSON.
var fieldDecoders = map[reflect.Type]string{
	reflect.TypeFor[adapter.ProviderConfig](): "decodes its own fields and reads models[].cost presence",
}

func TestSettingsTypesDecodeTheirFields(t *testing.T) {
	jsonDecoder := reflect.TypeFor[json.Unmarshaler]()
	textDecoder := reflect.TypeFor[encoding.TextUnmarshaler]()
	seen := map[reflect.Type]bool{}
	var walk func(schema reflect.Type, path string)
	walk = func(schema reflect.Type, path string) {
		for schema.Kind() == reflect.Pointer {
			schema = schema.Elem()
		}
		if seen[schema] || schema == rawMessageType {
			return
		}
		seen[schema] = true
		decodes := reflect.PointerTo(schema).Implements(jsonDecoder) || reflect.PointerTo(schema).Implements(textDecoder)
		if _, listed := fieldDecoders[schema]; decodes && !listed {
			t.Errorf("%s (%s) decodes itself; check its member names match its fields, then list it in fieldDecoders", schema, path)
		}
		switch schema.Kind() {
		case reflect.Struct:
			for name, field := range schemaFields(schema) {
				walk(field, joinSettingPath(path, name))
			}
		case reflect.Map, reflect.Slice, reflect.Array:
			walk(schema.Elem(), path+"[]")
		case reflect.Interface:
			t.Errorf("%s holds free-form JSON the pruner cannot check", path)
		}
	}
	walk(settingsSchema, "")
}

// Unknown members nested inside settings objects, including types with their
// own JSON decoding such as provider entries, get the same treatment as
// unknown top-level keys: a warning and removal, an error in strict mode,
// and always an error under managed requirements.
func TestNestedUnknownSettings(t *testing.T) {
	typo := "providers.openai.bsae_url=https://wrong.example"

	w := newLayerWorkspace(t)
	w.flags.Overrides = StringsInput{Set: true, Values: []string{typo}}
	resolved := w.mustResolve()
	if warnings := strings.Join(resolved.Warnings(), "\n"); !strings.Contains(warnings, "providers.openai.bsae_url") || !strings.Contains(warnings, "-c") {
		t.Fatalf("warnings %q do not name the nested unknown key and its source", warnings)
	}
	for _, setting := range resolved.Settings() {
		if strings.Contains(setting.Key, "bsae_url") {
			t.Fatalf("config show lists the ignored key %q", setting.Key)
		}
	}

	w.flags.StrictConfig = BoolInput{Value: true, Set: true}
	_, err := w.resolve()
	var configErr *Error
	if !errors.As(err, &configErr) || configErr.Field != "providers.openai.bsae_url" || !strings.Contains(err.Error(), "-c") {
		t.Fatalf("strict nested unknown: %v", err)
	}

	w = newLayerWorkspace(t)
	w.write(w.managed, `{"requirements":{"providers":{"openai":{"bsae_url":"https://proxy.corp/v1"}}}}`)
	if _, err := w.resolve(); !errors.As(err, &configErr) || !strings.Contains(err.Error(), "bsae_url") || !strings.Contains(err.Error(), w.managed) {
		t.Fatalf("managed nested unknown requirement: %v", err)
	}

	w = newLayerWorkspace(t)
	w.write(w.user, `{"providers":{"gw":{"protocol":"openai-chat","base_url":"https://gw.example/v1","no_auth":true,
		"compatibility":{"supportsPromptCacheKeyy":true},
		"models":[{"id":"m","contextWindw":1000,"cost":{"inptu":1}}]}}}`)
	warnings := strings.Join(w.mustResolve().Warnings(), "\n")
	for _, path := range []string{"providers.gw.compatibility.supportsPromptCacheKeyy", "providers.gw.models[0].contextWindw", "providers.gw.models[0].cost.inptu"} {
		if !strings.Contains(warnings, path) {
			t.Fatalf("warnings %q do not name %s", warnings, path)
		}
	}
}

func TestConfigShowRedactsCaseVariantContainers(t *testing.T) {
	w := newLayerWorkspace(t)
	w.write(w.user, `{
		"providers":{"relay":{"protocol":"openai-chat","base_url":"https://relay.example/v1","no_auth":true,"Headers":{"X-Custom":"p1-header-credential-dummy"}}},
		"mcp_servers":[{"name":"verify","transport":"stdio","command":"true","Env":{"GITHUB_PAT":"p1-env-credential-dummy"}}]
	}`)
	resolved := w.mustResolve()
	encoded, err := json.Marshal(resolved.Settings())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"p1-header-credential-dummy", "p1-env-credential-dummy"} {
		if strings.Contains(string(encoded), secret) || strings.Contains(strings.Join(resolved.Warnings(), "\n"), secret) {
			t.Fatalf("config show leaked %q: %s", secret, encoded)
		}
	}
	// The redactor fails closed on its own, whatever reaches it.
	for _, path := range [][]string{
		{"providers", "relay", "Headers", "X-Custom"},
		{"mcp_servers", arrayItemSegment, "ENV", "GITHUB_PAT"},
		{"providers", "relay", "Query", "sig"},
	} {
		if !isSecretPath(path) {
			t.Fatalf("%v is not treated as a credential", path)
		}
	}
}
