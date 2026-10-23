package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/cloudcmd"
	"go.harness.dev/harness/internal/lsp"
)

// writeFile writes content into dir and returns its path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// resolveWith resolves configuration from flags, an environment map and an
// optional settings document.
func resolveWith(t *testing.T, flags FlagValues, env map[string]string, settings string) (*ResolvedConfig, error) {
	t.Helper()
	settingsPath := filepath.Join(t.TempDir(), "missing-settings.json")
	if settings != "" {
		settingsPath = writeFile(t, t.TempDir(), "settings.json", settings)
	}
	return Config{Flags: flags, SettingsPath: settingsPath, LookupEnv: func(key string) string { return env[key] }}.Resolve()
}

func serverNames(cfg lsp.Config) string {
	names := make([]string, 0, len(cfg.Servers))
	for _, server := range cfg.Servers {
		names = append(names, server.Name)
	}
	return strings.Join(names, ",")
}

func TestLSPEnablementPrecedence(t *testing.T) {
	broken := writeFile(t, t.TempDir(), "broken.json", `{"servers":[{"name":"cobol","command":"x"}]}`)
	for _, test := range []struct {
		name     string
		flags    FlagValues
		env      map[string]string
		settings string
		want     bool
	}{
		{name: "default off"},
		{name: "settings on", settings: `{"enable_lsp":true}`, want: true},
		{name: "environment beats settings", settings: `{"enable_lsp":true}`, env: map[string]string{"HARNESS_ENABLE_LSP": "false"}},
		{name: "environment on", env: map[string]string{"HARNESS_ENABLE_LSP": "1"}, want: true},
		{name: "explicit flag false beats environment", flags: FlagValues{EnableLSP: BoolInput{Value: false, Set: true}}, env: map[string]string{"HARNESS_ENABLE_LSP": "true"}},
		// A disabled configuration never reads the server file, so an
		// operator can always switch a broken file off.
		{name: "flag false ignores a broken server file", flags: FlagValues{EnableLSP: BoolInput{Value: false, Set: true}, LSPConfig: StringInput{Value: broken, Set: true}}, settings: `{"enable_lsp":true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolved, err := resolveWith(t, test.flags, test.env, test.settings)
			if err != nil {
				t.Fatal(err)
			}
			if got := resolved.LSP().Enabled; got != test.want {
				t.Fatalf("enabled = %v, want %v", got, test.want)
			}
		})
	}
	if _, err := resolveWith(t, FlagValues{}, map[string]string{"HARNESS_ENABLE_LSP": "maybe"}, ""); err == nil {
		t.Fatal("accepted a non-boolean HARNESS_ENABLE_LSP")
	}
}

func TestLSPServerFileSelection(t *testing.T) {
	dir := t.TempDir()
	goOnly := writeFile(t, dir, "go.json", `{"servers":[{"name":"go","command":"./bin/gopls","args":["-remote=auto"],"env":{"GOFLAGS":"-mod=mod"},"settings":{"gopls":{"staticcheck":true}}}]}`)
	none := writeFile(t, dir, "none.json", `{"servers":[]}`)
	envFile := writeFile(t, dir, "env.json", `{"servers":[{"name":"yaml","command":"/opt/yaml-language-server","args":["--stdio"]}]}`)
	enabled := FlagValues{EnableLSP: BoolInput{Value: true, Set: true}}
	withFile := func(path string) FlagValues {
		flags := enabled
		flags.LSPConfig = StringInput{Value: path, Set: true}
		return flags
	}

	resolved, err := resolveWith(t, enabled, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := serverNames(resolved.LSP()); got != "go,yaml,terraform,helm" {
		t.Fatalf("absent file servers = %s, want the built-in routes", got)
	}
	resolved, err = resolveWith(t, withFile(""), map[string]string{"HARNESS_LSP_CONFIG": envFile}, "")
	if err != nil || serverNames(resolved.LSP()) != "go,yaml,terraform,helm" {
		t.Fatalf("explicit empty -lsp-config must select the built-ins over the environment: %v %v", serverNames(resolved.LSP()), err)
	}
	resolved, err = resolveWith(t, enabled, map[string]string{"HARNESS_LSP_CONFIG": envFile}, `{"lsp_config":"`+goOnly+`"}`)
	if err != nil || serverNames(resolved.LSP()) != "yaml" {
		t.Fatalf("environment file must beat settings: %v %v", serverNames(resolved.LSP()), err)
	}
	resolved, err = resolveWith(t, withFile(none), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if servers := resolved.LSP().Servers; servers == nil || len(servers) != 0 {
		t.Fatalf("an explicit empty list means no servers, got %#v", servers)
	}

	resolved, err = resolveWith(t, withFile(goOnly), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := resolved.LSP()
	if serverNames(cfg) != "go" {
		t.Fatalf("a supplied list replaces the defaults, got %s", serverNames(cfg))
	}
	server := cfg.Servers[0]
	if server.Command != filepath.Join(dir, "bin", "gopls") {
		t.Fatalf("relative executable resolves against the file: %q", server.Command)
	}
	// The accessor returns a detached copy: secret-bearing env stays put.
	server.Env["GOFLAGS"] = "mutated"
	server.Args[0] = "mutated"
	again := resolved.LSP().Servers[0]
	if again.Env["GOFLAGS"] != "-mod=mod" || again.Args[0] != "-remote=auto" {
		t.Fatalf("LSP() shares mutable state: %#v", again)
	}
}

func TestLSPServerFileRejectsMalformedConfigs(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"unknown top-level field": `{"servers":[],"extra":true}`,
		"unknown server field":    `{"servers":[{"name":"go","command":"gopls","shell":true}]}`,
		"missing servers":         `{}`,
		"null servers":            `{"servers":null}`,
		"trailing data":           `{"servers":[]} {}`,
		"unknown route":           `{"servers":[{"name":"python","command":"pylsp"}]}`,
		"duplicate name":          `{"servers":[{"name":"go","command":"gopls"},{"name":"go","command":"gopls"}]}`,
		"settings not an object":  `{"servers":[{"name":"go","command":"gopls","settings":[1]}]}`,
		"oversized":               `{"servers":[` + strings.Repeat(" ", maxOperatorFileBytes) + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t, dir, strings.ReplaceAll(name, " ", "-")+".json", content)
			_, err := resolveWith(t, FlagValues{EnableLSP: BoolInput{Value: true, Set: true}, LSPConfig: StringInput{Value: path, Set: true}}, nil, "")
			var configErr *Error
			if !errors.As(err, &configErr) || configErr.Field != "lsp_config" {
				t.Fatalf("got %v, want an lsp_config error", err)
			}
		})
	}
	_, err := resolveWith(t, FlagValues{EnableLSP: BoolInput{Value: true, Set: true}, LSPConfig: StringInput{Value: filepath.Join(dir, "absent.json"), Set: true}}, nil, "")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a selected but missing file must fail: %v", err)
	}
}

func TestCloudPolicySelection(t *testing.T) {
	dir := t.TempDir()
	policy := writeFile(t, dir, "policy.json", `{"rules":[{"tool":"kubectl","context":"production","action":"deny"},{"tool":"helm","context":"kind-dev","action":"allow_write"}]}`)
	other := writeFile(t, dir, "other.json", `{"rules":[{"tool":"kubectl","context":"staging","action":"deny"}]}`)

	resolved, err := resolveWith(t, FlagValues{}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if rules := resolved.CloudRules(); rules == nil || len(rules) != 0 {
		t.Fatalf("no policy means no rules, not nil: %#v", rules)
	}
	resolved, err = resolveWith(t, FlagValues{CloudPolicy: StringInput{Value: policy, Set: true}}, map[string]string{"HARNESS_CLOUD_POLICY": other}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []cloudcmd.Rule{{Tool: "helm", Context: "kind-dev", Action: "allow_write"}, {Tool: "kubectl", Context: "production", Action: "deny"}}
	rules := resolved.CloudRules()
	if len(rules) != len(want) || rules[0] != want[0] || rules[1] != want[1] {
		t.Fatalf("flag file rules = %#v, want normalized %#v", rules, want)
	}
	rules[0].Action = "deny"
	if resolved.CloudRules()[0].Action != "allow_write" {
		t.Fatal("CloudRules() shares its backing array")
	}
	resolved, err = resolveWith(t, FlagValues{}, map[string]string{"HARNESS_CLOUD_POLICY": other}, `{"cloud_policy":"`+policy+`"}`)
	if err != nil || len(resolved.CloudRules()) != 1 || resolved.CloudRules()[0].Context != "staging" {
		t.Fatalf("environment must beat settings: %#v %v", resolved.CloudRules(), err)
	}
	resolved, err = resolveWith(t, FlagValues{CloudPolicy: StringInput{Value: "", Set: true}}, map[string]string{"HARNESS_CLOUD_POLICY": other}, "")
	if err != nil || len(resolved.CloudRules()) != 0 {
		t.Fatalf("explicit empty -cloud-policy selects no rules: %#v %v", resolved.CloudRules(), err)
	}

	var tooMany strings.Builder
	tooMany.WriteString(`{"rules":[`)
	for i := range cloudcmd.MaxRules + 1 {
		if i > 0 {
			tooMany.WriteString(",")
		}
		fmt.Fprintf(&tooMany, `{"tool":"kubectl","context":"c%d","action":"deny"}`, i)
	}
	tooMany.WriteString("]}")
	for name, content := range map[string]string{
		"pattern context": `{"rules":[{"tool":"kubectl","context":"prod-*","action":"deny"}]}`,
		"terraform grant": `{"rules":[{"tool":"terraform","context":"dev","action":"allow_write"}]}`,
		"unknown field":   `{"rules":[{"tool":"kubectl","context":"dev","action":"deny","namespace":"x"}]}`,
		"null rules":      `{"rules":null}`,
		"too many rules":  tooMany.String(),
	} {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t, dir, strings.ReplaceAll(name, " ", "-")+".json", content)
			_, err := resolveWith(t, FlagValues{CloudPolicy: StringInput{Value: path, Set: true}}, nil, "")
			var configErr *Error
			if !errors.As(err, &configErr) || configErr.Field != "cloud_policy" {
				t.Fatalf("got %v, want a cloud_policy error", err)
			}
		})
	}
}
