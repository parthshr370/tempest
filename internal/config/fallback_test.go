package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFallbackSettings(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFallbackModelsPrecedenceAndRoleInheritance(t *testing.T) {
	settings := writeFallbackSettings(t, `{"fallback_models":{"default":["openai:settings-a","google:settings-b"],"smol":[]}}`)
	planOwn := writeFallbackSettings(t, `{"fallback_models":{"default":["openai:settings-a"],"plan":["anthropic:plan-backup"]}}`)
	flag := StringsInput{Values: []string{"openai:flag-a", " anthropic:flag-b "}, Set: true}
	for _, test := range []struct {
		name               string
		flags              FlagValues
		env                map[string]string
		path               string
		defaultChain, plan string
		smol               string
	}{
		{name: "unset", path: filepath.Join(t.TempDir(), "missing.json")},
		{name: "settings; plan inherits, explicit empty smol turns it off", path: settings, defaultChain: "openai:settings-a,google:settings-b", plan: "openai:settings-a,google:settings-b"},
		{name: "plan entry of its own", path: planOwn, defaultChain: "openai:settings-a", plan: "anthropic:plan-backup", smol: "openai:settings-a"},
		{name: "environment beats settings", path: settings, env: map[string]string{"HARNESS_FALLBACK_MODELS": "openai:env-a, openai:env-b"}, defaultChain: "openai:env-a,openai:env-b", plan: "openai:env-a,openai:env-b"},
		{name: "flag sets only the default role; settings' own plan chain stays", path: planOwn, flags: FlagValues{FallbackModels: flag}, defaultChain: "openai:flag-a,anthropic:flag-b", plan: "anthropic:plan-backup", smol: "openai:flag-a,anthropic:flag-b"},
		{name: "-c override beats the flag", path: settings, flags: FlagValues{FallbackModels: flag, Overrides: StringsInput{Values: []string{`fallback_models.default=["openai:c-a"]`}, Set: true}}, defaultChain: "openai:c-a", plan: "openai:c-a"},
		{name: "flag beats environment", path: settings, flags: FlagValues{FallbackModels: flag}, env: map[string]string{"HARNESS_FALLBACK_MODELS": "openai:env-a"}, defaultChain: "openai:flag-a,anthropic:flag-b", plan: "openai:flag-a,anthropic:flag-b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolved, err := Config{Flags: test.flags, SettingsPath: test.path, LookupEnv: func(name string) string { return test.env[name] }}.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			for role, want := range map[string]string{RoleDefault: test.defaultChain, RolePlan: test.plan, RoleSmol: test.smol} {
				if got := strings.Join(resolved.FallbackModels(role), ","); got != want {
					t.Errorf("%s chain = %q, want %q", role, got, want)
				}
			}
		})
	}
}

func TestFallbackModelsRejectsUnknownRolesAndUnroutableModels(t *testing.T) {
	for _, test := range []struct {
		name, settings, field string
	}{
		{"unknown role", `{"fallback_models":{"review":["openai:x"]}}`, "fallback_models"},
		{"unknown provider", `{"fallback_models":{"default":["nope:model"]}}`, "fallback_models.default[0]"},
		{"empty spec", `{"fallback_models":{"plan":["openai:x",""]}}`, "fallback_models.plan[1]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Config{SettingsPath: writeFallbackSettings(t, test.settings), LookupEnv: func(string) string { return "" }}.Resolve()
			var configErr *Error
			if !errors.As(err, &configErr) || configErr.Field != test.field {
				t.Fatalf("error = %v, want field %s", err, test.field)
			}
		})
	}
}
