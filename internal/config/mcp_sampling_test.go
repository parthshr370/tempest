package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveMCPSamplingSettings(t *testing.T) {
	resolve := func(t *testing.T, settings string, env map[string]string) (*ResolvedConfig, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
		return (Config{SettingsPath: path, LookupEnv: func(key string) string { return env[key] }}).Resolve()
	}
	defaults, err := resolve(t, `{"max_tokens":2000}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.MCPSamplingModel() != "" || defaults.MCPSamplingMaxTokens() != 2000 || defaults.MCPSamplingAllowHeadless() {
		t.Fatalf("defaults: model %q cap %d headless %v", defaults.MCPSamplingModel(), defaults.MCPSamplingMaxTokens(), defaults.MCPSamplingAllowHeadless())
	}
	settings := `{"mcp_sampling_model":"openai:gpt-6-luna","mcp_sampling_max_tokens":512,"mcp_sampling_allow_headless":true}`
	fromFile, err := resolve(t, settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fromFile.MCPSamplingModel() != "openai:gpt-6-luna" || fromFile.MCPSamplingMaxTokens() != 512 || !fromFile.MCPSamplingAllowHeadless() {
		t.Fatal("settings file values were not resolved")
	}
	fromEnv, err := resolve(t, settings, map[string]string{"HARNESS_MCP_SAMPLING_MODEL": "anthropic:claude-haiku-4-5", "HARNESS_MCP_SAMPLING_MAX_TOKENS": "64", "HARNESS_MCP_SAMPLING_ALLOW_HEADLESS": "false"})
	if err != nil {
		t.Fatal(err)
	}
	if fromEnv.MCPSamplingModel() != "anthropic:claude-haiku-4-5" || fromEnv.MCPSamplingMaxTokens() != 64 || fromEnv.MCPSamplingAllowHeadless() {
		t.Fatal("environment did not override settings")
	}
	for field, invalid := range map[string]string{
		"mcp_sampling_model":          `{"mcp_sampling_model":"nowhere:model"}`,
		"mcp_sampling_max_tokens":     `{"mcp_sampling_max_tokens":0}`,
		"mcp_sampling_allow_headless": `{"mcp_sampling_allow_headless":"yes"}`,
	} {
		_, err := resolve(t, invalid, nil)
		var configErr *Error
		if field == "mcp_sampling_allow_headless" {
			// A wrong JSON type fails while reading the settings file.
			if err == nil {
				t.Fatalf("%s: invalid value resolved", field)
			}
			continue
		}
		if !errors.As(err, &configErr) || configErr.Field != field {
			t.Fatalf("%s: err = %v", field, err)
		}
	}
}
