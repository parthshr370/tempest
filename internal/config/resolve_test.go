package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestResolvePrecedence(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{
		"model":"anthropic:settings",
		"max_tokens":9000,
		"enable_task":true,
		"plan":true,
		"attach":["/settings-note.md"],
		"enable_web":true,
		"agent_dir":"/settings-agent",
		"log_level":"warn"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		flags FlagValues
		env   map[string]string
		path  string
		check func(*ResolvedConfig) string
		want  string
	}{
		{name: "model default", path: filepath.Join(t.TempDir(), "missing.json"), check: func(c *ResolvedConfig) string { return c.Model() }, want: defaultModel},
		{name: "model settings", path: settingsPath, check: func(c *ResolvedConfig) string { return c.Model() }, want: "anthropic:settings"},
		{name: "model environment", path: settingsPath, env: map[string]string{"HARNESS_MODEL": "anthropic:environment"}, check: func(c *ResolvedConfig) string { return c.Model() }, want: "anthropic:environment"},
		{name: "model flag", path: settingsPath, flags: FlagValues{Model: StringInput{Value: "anthropic:flag", Set: true}}, env: map[string]string{"HARNESS_MODEL": "anthropic:environment"}, check: func(c *ResolvedConfig) string { return c.Model() }, want: "anthropic:flag"},
		{name: "integer settings", path: settingsPath, check: func(c *ResolvedConfig) string { return strconv.Itoa(c.MaxTokens()) }, want: "9000"},
		{name: "integer environment", path: settingsPath, env: map[string]string{"HARNESS_MAX_TOKENS": "8000"}, check: func(c *ResolvedConfig) string { return strconv.Itoa(c.MaxTokens()) }, want: "8000"},
		{name: "integer flag", path: settingsPath, flags: FlagValues{MaxTokens: IntInput{Value: 7000, Set: true}}, env: map[string]string{"HARNESS_MAX_TOKENS": "8000"}, check: func(c *ResolvedConfig) string { return strconv.Itoa(c.MaxTokens()) }, want: "7000"},
		{name: "boolean settings", path: settingsPath, check: func(c *ResolvedConfig) string { return strconv.FormatBool(c.EnableWeb()) }, want: "true"},
		{name: "boolean environment false", path: settingsPath, env: map[string]string{"HARNESS_ENABLE_WEB": "false"}, check: func(c *ResolvedConfig) string { return strconv.FormatBool(c.EnableWeb()) }, want: "false"},
		{name: "boolean flag false", path: settingsPath, flags: FlagValues{EnableWeb: BoolInput{Value: false, Set: true}}, env: map[string]string{"HARNESS_ENABLE_WEB": "true"}, check: func(c *ResolvedConfig) string { return strconv.FormatBool(c.EnableWeb()) }, want: "false"},
		{name: "path settings", path: settingsPath, check: func(c *ResolvedConfig) string { return c.AgentDir() }, want: "/settings-agent"},
		{name: "path environment", path: settingsPath, env: map[string]string{EnvAgentDir: "/environment-agent"}, check: func(c *ResolvedConfig) string { return c.AgentDir() }, want: "/environment-agent"},
		{name: "path flag", path: settingsPath, flags: FlagValues{AgentDir: StringInput{Value: "/flag-agent", Set: true}}, env: map[string]string{EnvAgentDir: "/environment-agent"}, check: func(c *ResolvedConfig) string { return c.AgentDir() }, want: "/flag-agent"},
		{name: "debug alias", path: settingsPath, env: map[string]string{"HARNESS_DEBUG": "1"}, check: func(c *ResolvedConfig) string { return c.LogLevel() }, want: "debug"},
		{name: "log level environment wins debug alias", path: settingsPath, env: map[string]string{"HARNESS_LOG_LEVEL": "error", "HARNESS_DEBUG": "1"}, check: func(c *ResolvedConfig) string { return c.LogLevel() }, want: "error"},
		{name: "log level flag wins debug alias", path: settingsPath, flags: FlagValues{LogLevel: StringInput{Value: "error", Set: true}}, env: map[string]string{"HARNESS_DEBUG": "1"}, check: func(c *ResolvedConfig) string { return c.LogLevel() }, want: "error"},
		{name: "enable task settings", path: settingsPath, check: func(c *ResolvedConfig) string { return strconv.FormatBool(c.EnableTask()) }, want: "true"},
		{name: "enable task environment false", path: settingsPath, env: map[string]string{"HARNESS_ENABLE_TASK": "false"}, check: func(c *ResolvedConfig) string { return strconv.FormatBool(c.EnableTask()) }, want: "false"},
		{name: "enable task flag false", path: settingsPath, flags: FlagValues{EnableTask: BoolInput{Value: false, Set: true}}, env: map[string]string{"HARNESS_ENABLE_TASK": "true"}, check: func(c *ResolvedConfig) string { return strconv.FormatBool(c.EnableTask()) }, want: "false"},
		{name: "plan settings", path: settingsPath, check: func(c *ResolvedConfig) string { return strconv.FormatBool(c.Plan()) }, want: "true"},
		{name: "plan environment false", path: settingsPath, env: map[string]string{"HARNESS_PLAN": "false"}, check: func(c *ResolvedConfig) string { return strconv.FormatBool(c.Plan()) }, want: "false"},
		{name: "plan flag false", path: settingsPath, flags: FlagValues{Plan: BoolInput{Value: false, Set: true}}, env: map[string]string{"HARNESS_PLAN": "true"}, check: func(c *ResolvedConfig) string { return strconv.FormatBool(c.Plan()) }, want: "false"},
		{name: "attach settings", path: settingsPath, check: func(c *ResolvedConfig) string { return strings.Join(c.AttachPaths(), ",") }, want: "/settings-note.md"},
		{name: "attach environment", path: settingsPath, env: map[string]string{"HARNESS_ATTACH": "/env-a.md,/env-b.md"}, check: func(c *ResolvedConfig) string { return strings.Join(c.AttachPaths(), ",") }, want: "/env-a.md,/env-b.md"},
		{name: "attach flag", path: settingsPath, flags: FlagValues{Attach: StringsInput{Values: []string{"/flag-a.md"}, Set: true}}, env: map[string]string{"HARNESS_ATTACH": "/env-a.md"}, check: func(c *ResolvedConfig) string { return strings.Join(c.AttachPaths(), ",") }, want: "/flag-a.md"},
		{name: "attach default", path: filepath.Join(t.TempDir(), "missing.json"), check: func(c *ResolvedConfig) string { return strings.Join(c.AttachPaths(), ",") }, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolved, err := Config{
				Flags:        test.flags,
				SettingsPath: test.path,
				LookupEnv:    func(name string) string { return test.env[name] },
			}.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			if got := test.check(resolved); got != test.want {
				t.Fatalf("resolved value = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPermissionToolListsResolveWithPrecedenceAndDefensiveCopies(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{
		"permission_allow_tools":["settings-allow"],
		"permission_deny_tools":["settings-deny"]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		flags     FlagValues
		env       map[string]string
		wantAllow []string
		wantDeny  []string
	}{
		{name: "settings", wantAllow: []string{"settings-allow"}, wantDeny: []string{"settings-deny"}},
		{name: "environment", env: map[string]string{
			"HARNESS_PERMISSION_ALLOW_TOOLS": "environment-allow-one, environment-allow-two",
			"HARNESS_PERMISSION_DENY_TOOLS":  "environment-deny-one, environment-deny-two",
		}, wantAllow: []string{"environment-allow-one", "environment-allow-two"}, wantDeny: []string{"environment-deny-one", "environment-deny-two"}},
		{name: "flags", flags: FlagValues{
			PermissionAllowTools: StringsInput{Values: []string{"flag-allow"}, Set: true},
			PermissionDenyTools:  StringsInput{Values: []string{"flag-deny"}, Set: true},
		}, env: map[string]string{
			"HARNESS_PERMISSION_ALLOW_TOOLS": "environment-allow",
			"HARNESS_PERMISSION_DENY_TOOLS":  "environment-deny",
		}, wantAllow: []string{"flag-allow"}, wantDeny: []string{"flag-deny"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolved, err := Config{
				Flags:        test.flags,
				SettingsPath: settingsPath,
				LookupEnv:    func(name string) string { return test.env[name] },
			}.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			if got := resolved.PermissionAllowTools(); !reflect.DeepEqual(got, test.wantAllow) {
				t.Fatalf("allow tools = %#v; want %#v", got, test.wantAllow)
			}
			if got := resolved.PermissionDenyTools(); !reflect.DeepEqual(got, test.wantDeny) {
				t.Fatalf("deny tools = %#v; want %#v", got, test.wantDeny)
			}
		})
	}

	resolved, err := Config{SettingsPath: settingsPath, LookupEnv: func(string) string { return "" }}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	allow := resolved.PermissionAllowTools()
	deny := resolved.PermissionDenyTools()
	allow[0] = "mutated"
	deny[0] = "mutated"
	if got := resolved.PermissionAllowTools()[0]; got != "settings-allow" {
		t.Fatalf("PermissionAllowTools leaked mutable slice: %q", got)
	}
	if got := resolved.PermissionDenyTools()[0]; got != "settings-deny" {
		t.Fatalf("PermissionDenyTools leaked mutable slice: %q", got)
	}
}

func TestResolveUsesOnlyInjectedEnvironment(t *testing.T) {
	t.Setenv(EnvAgentDir, "/process-agent-dir")
	resolved, err := Config{
		SettingsPath: filepath.Join(t.TempDir(), "missing.json"),
		LookupEnv:    func(string) string { return "" },
	}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resolved.AgentDir(), defaultAgentDir(); got != want {
		t.Fatalf("agent directory = %q, want %q", got, want)
	}
}

func TestResolvedConfigAccessorsExposeImmutableSnapshot(t *testing.T) {
	resolved, err := Config{SettingsPath: filepath.Join(t.TempDir(), "missing.json"), LookupEnv: func(string) string { return "" }}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Model() == "" || resolved.SettingsPath() == "" {
		t.Fatalf("accessors returned incomplete config: model=%q settings=%q", resolved.Model(), resolved.SettingsPath())
	}
	typ := reflect.TypeOf(*resolved)
	for i := range typ.NumField() {
		if field := typ.Field(i); field.PkgPath == "" {
			t.Fatalf("ResolvedConfig field %q is exported", field.Name)
		}
	}
}

func TestResolveValidationErrors(t *testing.T) {
	tests := []struct {
		name  string
		flags FlagValues
	}{
		{name: "unknown provider", flags: FlagValues{Model: StringInput{Value: "other:model", Set: true}}},
		{name: "missing model", flags: FlagValues{Model: StringInput{Value: "openai:", Set: true}}},
		{name: "conflicting credentials", flags: FlagValues{AnthropicAPIKey: StringInput{Value: "one", Set: true}, AnthropicAuthToken: StringInput{Value: "two", Set: true}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Config{Flags: test.flags, SettingsPath: filepath.Join(t.TempDir(), "missing.json"), LookupEnv: func(string) string { return "" }}.Resolve()
			if err == nil {
				t.Fatal("Resolve unexpectedly succeeded")
			}
			var configErr *Error
			if !errors.As(err, &configErr) {
				t.Fatalf("error %T does not preserve *Error: %v", err, err)
			}
		})
	}
}

func TestSkillPathsResolutionAndDefensiveCopy(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"skill_paths":["settings-one","settings-two"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := Config{SettingsPath: settingsPath, LookupEnv: func(string) string { return "" }}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resolved.SkillPaths(), []string{"settings-one", "settings-two"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("settings skill paths = %#v, want %#v", got, want)
	}
	paths := resolved.SkillPaths()
	paths[0] = "mutated"
	if got := resolved.SkillPaths()[0]; got != "settings-one" {
		t.Fatalf("SkillPaths leaked mutable slice: %q", got)
	}
	resolved, err = Config{Flags: FlagValues{SkillPaths: StringsInput{Values: []string{"flag-one", "flag-two"}, Set: true}}, SettingsPath: settingsPath, LookupEnv: func(string) string { return "" }}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resolved.SkillPaths(), []string{"flag-one", "flag-two"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("flag skill paths = %#v, want %#v", got, want)
	}
	if _, err := (Config{Flags: FlagValues{SkillPaths: StringsInput{Values: []string{""}, Set: true}}, SettingsPath: settingsPath, LookupEnv: func(string) string { return "" }}).Resolve(); err == nil {
		t.Fatal("empty -skill equivalent succeeded")
	}
}
