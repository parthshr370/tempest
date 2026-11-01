package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"go.harness.dev/harness/internal/config"
)

type configShowOutput struct {
	Settings map[string]shownSetting `json:"settings"`
	Warnings []string                `json:"warnings"`
}

func runConfigShow(t *testing.T, args ...string) (configShowOutput, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"config", "show", "-json"}, args...), &stdout, &stderr)
	var output configShowOutput
	if code == 0 {
		if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
			t.Fatalf("config show output %q: %v", stdout.String(), err)
		}
	}
	return output, code
}

// config show prints exactly the settings tree a run with the same flags
// resolves, with each value's layer and source.
func TestConfigShowMatchesResolver(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	output, code := runConfigShow(t, "-c", "model=anthropic:shown", "-max-tokens", "2048")
	if code != 0 {
		t.Fatalf("config show exit = %d", code)
	}
	resolved, err := (config.Config{Flags: config.FlagValues{
		Overrides: config.StringsInput{Values: []string{"model=anthropic:shown"}, Set: true},
		MaxTokens: config.IntInput{Value: 2048, Set: true},
	}}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	settings := resolved.Settings()
	if len(settings) != len(output.Settings) {
		t.Fatalf("config show keys %v, resolver has %d settings", slices.Sorted(maps.Keys(output.Settings)), len(settings))
	}
	for _, setting := range settings {
		shown, ok := output.Settings[setting.Key]
		if !ok || !bytes.Equal(shown.Value, setting.Value) || shown.Layer != setting.Layer || shown.Source != setting.Source {
			t.Fatalf("%s: shown %+v, resolver %+v", setting.Key, shown, setting)
		}
	}
	if model := output.Settings["model"]; model.Layer != config.LayerOverride || resolved.Model() != "anthropic:shown" {
		t.Fatalf("model = %+v", model)
	}
	if maxTokens := output.Settings["max_tokens"]; maxTokens.Layer != config.LayerFlag || maxTokens.Source != "-max-tokens" || string(maxTokens.Value) != "2048" {
		t.Fatalf("max_tokens = %+v", maxTokens)
	}

	filtered, code := runConfigShow(t, "-key", "max_tokens", "-max-tokens", "2048")
	if code != 0 || len(filtered.Settings) != 1 {
		t.Fatalf("-key max_tokens exit=%d settings=%v", code, filtered.Settings)
	}
	if _, code := runConfigShow(t, "-key", "no_such_setting"); code == 0 {
		t.Fatal("an unknown -key must fail")
	}
}
