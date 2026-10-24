package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHooksSettingsAndDisablePrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	data := `{"disableAllHooks":true,"trusted_hook_projects":["/trusted"],"hooks":{"PreToolUse":[{"matcher":"write","hooks":[{"type":"command","command":"exit 2"}]}]}}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{SettingsPath: path, LookupEnv: func(string) string { return "" }}
	resolved, err := c.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Hooks().NoHooks || len(resolved.Hooks().Hooks["PreToolUse"]) != 1 {
		t.Fatalf("hooks=%+v", resolved.Hooks())
	}
	c.LookupEnv = func(key string) string {
		if key == "HARNESS_NO_HOOKS" {
			return "false"
		}
		return ""
	}
	resolved, err = c.Resolve()
	if err != nil || resolved.Hooks().NoHooks {
		t.Fatalf("env precedence %v %v", resolved, err)
	}
	c.Flags.NoHooks = BoolInput{Set: true, Value: true}
	c.Flags.TrustProjectHooks = BoolInput{Set: true, Value: true}
	resolved, err = c.Resolve()
	if err != nil || !resolved.Hooks().NoHooks || !resolved.Hooks().TrustProjectHooks {
		t.Fatalf("flag precedence %v %v", resolved, err)
	}
	snapshot := resolved.Hooks()
	snapshot.Hooks["PreToolUse"][0].Hooks[0].Command = "changed"
	if resolved.Hooks().Hooks["PreToolUse"][0].Hooks[0].Command != "exit 2" {
		t.Fatal("caller mutated resolved hooks")
	}
}
