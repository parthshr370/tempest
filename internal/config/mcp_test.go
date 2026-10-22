package config

import (
	"os"
	"path/filepath"
	"testing"

	"go.harness.dev/harness/internal/mcpconfig"
)

func TestMCPConfigSources(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":[{"name":"file","transport":"stdio","command":"./fixture","permission":"allow","env":{"TOKEN":"secret"}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	settings := fileSettings{MCPServers: []mcpconfig.Server{{Name: "settings", Transport: "stdio", Command: "fixture"}}}
	fromSettings, err := resolveMCP(StringInput{}, "", settings)
	if err != nil || fromSettings[0].Name != "settings" {
		t.Fatalf("settings: %v %v", fromSettings, err)
	}
	fromEnv, err := resolveMCP(StringInput{}, path, settings)
	if err != nil || fromEnv[0].Name != "file" || fromEnv[0].Command != filepath.Join(directory, "fixture") {
		t.Fatalf("env: %v %v", fromEnv, err)
	}
	disabled, err := resolveMCP(StringInput{Set: true, Value: ""}, path, settings)
	if err != nil || disabled == nil || len(disabled) != 0 {
		t.Fatalf("explicit disable: %v %v", disabled, err)
	}
	settings.MCPConfig = &path
	if _, err = resolveMCP(StringInput{}, "", settings); err == nil {
		t.Fatal("accepted conflicting settings")
	}
	resolved := &ResolvedConfig{mcpServers: fromEnv}
	copy := resolved.MCPServers()
	copy[0].Env["TOKEN"] = "mutated"
	if resolved.MCPServers()[0].Env["TOKEN"] != "secret" {
		t.Fatal("secret map shared")
	}
}
func TestMCPConfigFileStrictness(t *testing.T) {
	for name, contents := range map[string]string{"unknown": `{"mcpServers":[],"other":true}`, "unknown-server": `{"mcpServers":[{"name":"x","transport":"stdio","command":"fixture","other":true}]}`, "null": `{"mcpServers":null}`, "trailing": `{"mcpServers":[]} {}`} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mcp.json")
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := resolveMCP(StringInput{Set: true, Value: path}, "", fileSettings{}); err == nil {
				t.Fatal("accepted malformed file")
			}
		})
	}
}
