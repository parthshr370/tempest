package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/adapter"
)

// layerWorkspace is a hermetic set of settings files plus injected
// environment and flags.
type layerWorkspace struct {
	t       *testing.T
	root    string
	user    string
	managed string
	env     map[string]string
	flags   FlagValues
}

func newLayerWorkspace(t *testing.T) *layerWorkspace {
	t.Helper()
	// Project layers carry the canonical workspace path, as on macOS /var is /private/var.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ProjectConfigDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	return &layerWorkspace{
		t:       t,
		root:    root,
		user:    filepath.Join(home, "settings.json"),
		managed: filepath.Join(home, "managed-settings.json"),
		env:     map[string]string{},
	}
}

func (w *layerWorkspace) projectPath() string {
	return filepath.Join(w.root, ProjectConfigDirName, "settings.json")
}

func (w *layerWorkspace) localPath() string {
	return filepath.Join(w.root, ProjectConfigDirName, "settings.local.json")
}

func (w *layerWorkspace) write(path, content string) {
	w.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		w.t.Fatal(err)
	}
}

func (w *layerWorkspace) resolve() (*ResolvedConfig, error) {
	return Config{
		Flags:               w.flags,
		LookupEnv:           func(name string) string { return w.env[name] },
		SettingsPath:        w.user,
		ManagedSettingsPath: w.managed,
		WorkingDir:          w.root,
	}.Resolve()
}

func (w *layerWorkspace) mustResolve() *ResolvedConfig {
	w.t.Helper()
	resolved, err := w.resolve()
	if err != nil {
		w.t.Fatal(err)
	}
	return resolved
}

func settingFor(t *testing.T, resolved *ResolvedConfig, key string) Setting {
	t.Helper()
	for _, setting := range resolved.Settings() {
		if setting.Key == key {
			return setting
		}
	}
	t.Fatalf("no effective setting %q in %+v", key, resolved.Settings())
	return Setting{}
}

func jsonString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		t.Fatalf("value %s is not a string: %v", raw, err)
	}
	return text
}

// precedenceLayer sets "model" in one layer of a workspace.
type precedenceLayer struct {
	name   string
	source func(*layerWorkspace) string
	set    func(w *layerWorkspace, model string, user, project, local *[]string)
}

var precedenceLayers = []precedenceLayer{
	{LayerManaged, func(w *layerWorkspace) string { return w.managed }, func(w *layerWorkspace, model string, _, _, _ *[]string) {
		w.write(w.managed, fmt.Sprintf(`{"defaults":{"model":%q}}`, model))
	}},
	{LayerUser, func(w *layerWorkspace) string { return w.user }, func(_ *layerWorkspace, model string, user, _, _ *[]string) {
		*user = append(*user, fmt.Sprintf(`"model":%q`, model))
	}},
	{LayerProject, (*layerWorkspace).projectPath, func(_ *layerWorkspace, model string, _, project, _ *[]string) {
		*project = append(*project, fmt.Sprintf(`"model":%q`, model))
	}},
	{LayerProjectLocal, (*layerWorkspace).localPath, func(_ *layerWorkspace, model string, _, _, local *[]string) {
		*local = append(*local, fmt.Sprintf(`"model":%q`, model))
	}},
	{LayerProfile, func(w *layerWorkspace) string { return w.user }, func(w *layerWorkspace, model string, user, _, _ *[]string) {
		*user = append(*user, fmt.Sprintf(`"profiles":{"review":{"model":%q}}`, model))
		w.env["HARNESS_PROFILE"] = "review"
	}},
	{LayerEnv, func(*layerWorkspace) string { return "HARNESS_MODEL" }, func(w *layerWorkspace, model string, _, _, _ *[]string) {
		w.env["HARNESS_MODEL"] = model
	}},
	{LayerFlag, func(*layerWorkspace) string { return "-model" }, func(w *layerWorkspace, model string, _, _, _ *[]string) {
		w.flags.Model = StringInput{Value: model, Set: true}
	}},
	{LayerOverride, func(*layerWorkspace) string { return overrideSource }, func(w *layerWorkspace, model string, _, _, _ *[]string) {
		w.flags.Overrides = StringsInput{Values: []string{"model=" + model}, Set: true}
	}},
}

// Every layer alone, and every pair, resolves to the higher layer's value, and
// config show attributes the value to that layer and its file.
func TestLayerPrecedenceEveryPair(t *testing.T) {
	type selection struct{ low, high int }
	var cases []selection
	for high := range precedenceLayers {
		cases = append(cases, selection{-1, high})
		for low := range high {
			cases = append(cases, selection{low, high})
		}
	}
	for _, test := range cases {
		name := precedenceLayers[test.high].name
		if test.low >= 0 {
			name = precedenceLayers[test.low].name + "<" + name
		}
		t.Run(name, func(t *testing.T) {
			w := newLayerWorkspace(t)
			var user, project, local []string
			for _, index := range []int{test.low, test.high} {
				if index >= 0 {
					layer := precedenceLayers[index]
					layer.set(w, "anthropic:"+layer.name, &user, &project, &local)
				}
			}
			for path, members := range map[string][]string{w.user: user, w.projectPath(): project, w.localPath(): local} {
				if len(members) > 0 {
					w.write(path, "{"+strings.Join(members, ",")+"}")
				}
			}
			resolved := w.mustResolve()
			winner := precedenceLayers[test.high]
			if want := "anthropic:" + winner.name; resolved.Model() != want {
				t.Fatalf("model = %q, want %q", resolved.Model(), want)
			}
			shown := settingFor(t, resolved, "model")
			if shown.Layer != winner.name || shown.Source != winner.source(w) || jsonString(t, shown.Value) != resolved.Model() {
				t.Fatalf("config show model = %+v, resolver %q from %s %s", shown, resolved.Model(), winner.name, winner.source(w))
			}
		})
	}
	t.Run("default", func(t *testing.T) {
		resolved := newLayerWorkspace(t).mustResolve()
		if shown := settingFor(t, resolved, "model"); resolved.Model() != defaultModel || shown.Layer != LayerDefault || jsonString(t, shown.Value) != defaultModel {
			t.Fatalf("default model = %q, shown %+v", resolved.Model(), shown)
		}
	})
}

func TestLayerMergeObjectsReplaceArraysAndUnsetWithNull(t *testing.T) {
	w := newLayerWorkspace(t)
	w.write(w.managed, `{"defaults":{"enable_web":true,"bash_timeout":"30s","permission_deny_tools":["managed"]}}`)
	w.write(w.user, `{
		"enable_web": null,
		"max_tokens": 9000,
		"permission_deny_tools": ["user-a", "user-b"],
		"debug_adapters": {"node": {"command": "node", "args": ["user.js"]}},
		"providers": {"gw": {"protocol": "openai-chat", "base_url": "https://user.example/v1", "api_key_env": "GW_KEY"}}
	}`)
	w.write(w.projectPath(), `{"max_tokens": null}`)
	w.flags.Overrides = StringsInput{Set: true, Values: []string{
		"providers.gw.base_url=https://override.example/v1",
		`providers.extra={"protocol":"openai-chat","base_url":"https://extra.example/v1","no_auth":true}`,
		`debug_adapters.node.args=["override.js"]`,
	}}
	resolved := w.mustResolve()

	if got := resolved.PermissionDenyTools(); !reflect.DeepEqual(got, []string{"user-a", "user-b"}) {
		t.Fatalf("array not replaced: %v", got)
	}
	if resolved.MaxTokens() != adapter.DefaultMaxTokens || resolved.MaxTokensConfigured() {
		t.Fatalf("null did not unset max_tokens: %d configured=%t", resolved.MaxTokens(), resolved.MaxTokensConfigured())
	}
	if resolved.EnableWeb() || settingFor(t, resolved, "enable_web").Layer != LayerDefault {
		t.Fatal("user null did not unset the managed default")
	}
	if resolved.BashTimeout().String() != "30s" || settingFor(t, resolved, "bash_timeout").Layer != LayerManaged {
		t.Fatalf("managed default lost: %s", resolved.BashTimeout())
	}
	providers := resolved.Providers()
	if gw := providers["gw"]; gw.BaseURL != "https://override.example/v1" || gw.APIKeyEnv != "GW_KEY" || gw.Protocol != "openai-chat" {
		t.Fatalf("override did not merge into the user entry: %+v", gw)
	}
	if _, ok := providers["extra"]; !ok {
		t.Fatal("override did not add a sibling entry")
	}
	if node := resolved.DebugAdapters()["node"]; node.Command != "node" || !reflect.DeepEqual(node.Args, []string{"override.js"}) {
		t.Fatalf("debug adapter merge: %+v", node)
	}
	if shown := settingFor(t, resolved, "providers.gw.api_key_env"); shown.Layer != LayerUser || shown.Source != w.user {
		t.Fatalf("merged leaf attribution = %+v", shown)
	}
	if shown := settingFor(t, resolved, "providers.gw.base_url"); shown.Layer != LayerOverride {
		t.Fatalf("override leaf attribution = %+v", shown)
	}

	w.flags.Overrides = StringsInput{Set: true, Values: []string{"providers.gw=null"}}
	if _, ok := w.mustResolve().Providers()["gw"]; ok {
		t.Fatal("nested null did not unset the provider entry")
	}

	// Each -c applies on top of the previous one: unset the registry, then add one route.
	w.flags.Overrides = StringsInput{Set: true, Values: []string{"providers=null", `providers.only={"protocol":"openai-chat","base_url":"https://only.example/v1","no_auth":true}`}}
	providers = w.mustResolve().Providers()
	if _, ok := providers["gw"]; ok {
		t.Fatal("a later -c resurrected providers an earlier -c unset")
	}
	if _, ok := providers["only"]; !ok {
		t.Fatal("a later -c was lost")
	}
}

func TestManagedRequirementsPinKeys(t *testing.T) {
	w := newLayerWorkspace(t)
	w.write(w.managed, `{"requirements":{"permission_mode_default":"default","providers":{"openai":{"base_url":"https://proxy.corp/v1"}}}}`)
	resolved := w.mustResolve()
	if resolved.Providers()["openai"].BaseURL != "https://proxy.corp/v1" || resolved.PermissionModeDefault() != "default" {
		t.Fatal("requirement not applied when no layer sets the key")
	}
	if shown := settingFor(t, resolved, "providers.openai.base_url"); shown.Layer != LayerRequirement || shown.Source != w.managed {
		t.Fatalf("requirement attribution = %+v", shown)
	}

	w.write(w.user, `{"permission_mode_default":"default","providers":{"openai":{"api_key_env":"CORP_KEY"}}}`)
	resolved = w.mustResolve()
	if openai := resolved.Providers()["openai"]; openai.BaseURL != "https://proxy.corp/v1" || openai.APIKeyEnv != "CORP_KEY" {
		t.Fatalf("an equal value or an unpinned sibling must be accepted: %+v", openai)
	}

	// Each case sets a pinned key in one higher layer and returns what the
	// error must name besides the key and the managed file.
	violations := []struct {
		name    string
		key     string
		prepare func(*layerWorkspace) []string
	}{
		{"user", "permission_mode_default", func(w *layerWorkspace) []string {
			w.write(w.user, `{"permission_mode_default":"bypass"}`)
			return []string{"user settings", w.user}
		}},
		{"env", "permission_mode_default", func(w *layerWorkspace) []string {
			w.env["HARNESS_PERMISSION_MODE"] = "acceptEdits"
			return []string{"HARNESS_PERMISSION_MODE"}
		}},
		{"flag", "permission_mode_default", func(w *layerWorkspace) []string {
			w.flags.PermissionMode = StringInput{Value: "plan", Set: true}
			return []string{"-permission-mode"}
		}},
		{"override unsets parent", "providers.openai.base_url", func(w *layerWorkspace) []string {
			w.flags.Overrides = StringsInput{Set: true, Values: []string{"providers=null"}}
			return []string{"-c"}
		}},
		{"trusted project", "permission_mode_default", func(w *layerWorkspace) []string {
			w.write(w.projectPath(), `{"permission_mode_default":"bypass"}`)
			w.flags.TrustProjectHooks = BoolInput{Value: true, Set: true}
			return []string{"project settings", w.projectPath()}
		}},
	}
	for _, violation := range violations {
		t.Run(violation.name, func(t *testing.T) {
			w := newLayerWorkspace(t)
			w.write(w.managed, `{"requirements":{"permission_mode_default":"default","providers":{"openai":{"base_url":"https://proxy.corp/v1"}}}}`)
			names := violation.prepare(w)
			_, err := w.resolve()
			var configErr *Error
			if !errors.As(err, &configErr) || configErr.Field != violation.key {
				t.Fatalf("err = %v, want a %s requirement error", err, violation.key)
			}
			for _, name := range append(names, w.managed) {
				if !strings.Contains(err.Error(), name) {
					t.Fatalf("error %q does not name %q", err, name)
				}
			}
		})
	}

	t.Run("untrusted project value is dropped, not a violation", func(t *testing.T) {
		w := newLayerWorkspace(t)
		w.write(w.managed, `{"requirements":{"permission_mode_default":"default"}}`)
		w.write(w.projectPath(), `{"permission_mode_default":"bypass"}`)
		if resolved := w.mustResolve(); resolved.PermissionModeDefault() != "default" {
			t.Fatalf("mode = %q", resolved.PermissionModeDefault())
		}
	})

	t.Run("managed file rejects unknown sections and keys", func(t *testing.T) {
		for _, content := range []string{`{"require":{"model":"x"}}`, `{"requirements":{"modle":"x"}}`, `{"requirements":{"profiles":{}}}`} {
			w := newLayerWorkspace(t)
			w.write(w.managed, content)
			if _, err := w.resolve(); err == nil || !strings.Contains(err.Error(), w.managed) && !strings.Contains(err.Error(), "modle") {
				t.Fatalf("%s: err = %v", content, err)
			}
		}
	})
}

func TestStrictConfigAndUnknownKeyWarnings(t *testing.T) {
	w := newLayerWorkspace(t)
	w.write(w.user, `{"modle":"anthropic:typo","mcp_servers":[{"name":"x","transport":"stdio","command":"server","bogus":1}]}`)
	w.flags.Overrides = StringsInput{Set: true, Values: []string{"enable_webb=true"}}
	resolved := w.mustResolve()
	warnings := strings.Join(resolved.Warnings(), "\n")
	if len(resolved.Warnings()) != 3 || !strings.Contains(warnings, `"modle"`) || !strings.Contains(warnings, w.user) || !strings.Contains(warnings, `"enable_webb"`) || !strings.Contains(warnings, "-c") || !strings.Contains(warnings, `"mcp_servers[0].bogus"`) {
		t.Fatalf("want one warning per unknown key naming its source, got %q", resolved.Warnings())
	}

	for _, strict := range []func(*layerWorkspace){
		func(w *layerWorkspace) { w.flags.StrictConfig = BoolInput{Value: true, Set: true} },
		func(w *layerWorkspace) { w.env["HARNESS_STRICT_CONFIG"] = "true" },
	} {
		w := newLayerWorkspace(t)
		w.write(w.user, `{"modle":"anthropic:typo"}`)
		strict(w)
		_, err := w.resolve()
		var configErr *Error
		if !errors.As(err, &configErr) || configErr.Field != "modle" || !strings.Contains(err.Error(), w.user) {
			t.Fatalf("strict unknown key: %v", err)
		}
	}

	w = newLayerWorkspace(t)
	w.write(w.user, `{"mcp_servers":[{"name":"x","transport":"stdio","command":"server","bogus":1}]}`)
	w.flags.StrictConfig = BoolInput{Value: true, Set: true}
	if _, err := w.resolve(); err == nil || !strings.Contains(err.Error(), "bogus") || !strings.Contains(err.Error(), w.user) {
		t.Fatalf("strict nested unknown field: %v", err)
	}

	// A wrong type is an error in an operator file; an untrusted project file
	// with one is skipped (TestInvalidUntrustedProjectFileIsSkipped).
	for _, strict := range []bool{false, true} {
		w := newLayerWorkspace(t)
		w.write(w.user, `{"max_tokens":"lots"}`)
		w.flags.StrictConfig = BoolInput{Value: strict, Set: true}
		if _, err := w.resolve(); err == nil || !strings.Contains(err.Error(), w.user) || !strings.Contains(err.Error(), "max_tokens") {
			t.Fatalf("strict=%t wrong type: %v", strict, err)
		}
	}

	w = newLayerWorkspace(t)
	w.write(w.user, `{"profiles":{"review":{"modle":"x"}}}`)
	w.flags.Profile = StringInput{Value: "review", Set: true}
	if warnings := w.mustResolve().Warnings(); len(warnings) != 1 || !strings.Contains(warnings[0], `profile "review"`) {
		t.Fatalf("profile unknown key warnings = %q", warnings)
	}
}

func TestProjectTrustGateDropsExecutableAndCredentialSettings(t *testing.T) {
	project := `{
		"model": "anthropic:project",
		"enable_task": true,
		"mcp_servers": [{"name": "repo", "transport": "stdio", "command": "repo-server"}],
		"permission_mode_default": "bypass",
		"providers": {"repo": {"protocol": "openai-chat", "base_url": "https://repo.example/v1", "no_auth": true}},
		"no_hooks": true,
		"trusted_hook_projects": ["/"],
		"profiles": {"review": {"debug_adapters": {"repo": {"command": "repo-adapter"}}, "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "exit 0"}]}]}}}
	}`

	w := newLayerWorkspace(t)
	w.write(w.projectPath(), project)
	w.flags.Profile = StringInput{Value: "review", Set: true}
	resolved := w.mustResolve()
	if resolved.Model() != "anthropic:project" || !resolved.EnableTask() {
		t.Fatal("ungated project settings must apply without trust")
	}
	if len(resolved.MCPServers()) != 0 || resolved.PermissionModeDefault() != defaultPermissionMode || resolved.Hooks().NoHooks {
		t.Fatal("untrusted project widened execution")
	}
	if _, ok := resolved.Providers()["repo"]; ok {
		t.Fatal("untrusted project added a provider")
	}
	if _, ok := resolved.DebugAdapters()["repo"]; ok {
		t.Fatal("untrusted project profile added a debug adapter")
	}
	if !reflect.DeepEqual(resolved.UntrustedProviderFiles(), []string{w.projectPath()}) {
		t.Fatalf("untrusted provider files = %v", resolved.UntrustedProviderFiles())
	}
	warnings := resolved.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("want one trust warning, got %q", warnings)
	}
	for _, key := range []string{"mcp_servers", "permission_mode_default", "providers", "no_hooks", "trusted_hook_projects", "debug_adapters", "hooks", w.projectPath()} {
		if !strings.Contains(warnings[0], key) {
			t.Fatalf("trust warning %q does not name %s", warnings[0], key)
		}
	}

	trustRoot := func(w *layerWorkspace) {
		root, err := filepath.EvalSymlinks(w.root)
		if err != nil {
			t.Fatal(err)
		}
		w.write(w.user, fmt.Sprintf(`{"trusted_hook_projects":[%q]}`, root))
	}
	for name, trust := range map[string]func(*layerWorkspace){
		"flag":      func(w *layerWorkspace) { w.flags.TrustProjectHooks = BoolInput{Value: true, Set: true} },
		"user list": trustRoot,
	} {
		t.Run(name, func(t *testing.T) {
			w := newLayerWorkspace(t)
			w.write(w.projectPath(), project)
			w.flags.Profile = StringInput{Value: "review", Set: true}
			trust(w)
			resolved := w.mustResolve()
			if len(resolved.MCPServers()) != 1 || resolved.PermissionModeDefault() != "bypass" {
				t.Fatal("trusted project executable settings were not applied")
			}
			if _, ok := resolved.DebugAdapters()["repo"]; !ok {
				t.Fatal("trusted project profile was not applied")
			}
			if _, ok := resolved.Providers()["repo"]; ok || resolved.Hooks().NoHooks {
				t.Fatal("hook trust admitted provider or operator-only settings")
			}
			if shown := settingFor(t, resolved, "permission_mode_default"); shown.Layer != LayerProject || shown.Source != w.projectPath() {
				t.Fatalf("attribution = %+v", shown)
			}
		})
	}

	t.Run("model endpoint needs provider trust", func(t *testing.T) {
		w := newLayerWorkspace(t)
		w.write(w.projectPath(), `{"model":"openai:gpt-x@https://repo.example/v1"}`)
		resolved := w.mustResolve()
		if resolved.Model() != defaultModel || len(resolved.UntrustedProviderFiles()) != 1 {
			t.Fatalf("untrusted endpoint model = %q", resolved.Model())
		}
		w.flags.TrustProjectProviders = BoolInput{Value: true, Set: true}
		if resolved := w.mustResolve(); resolved.Model() != "openai:gpt-x@https://repo.example/v1" {
			t.Fatalf("trusted endpoint model = %q", resolved.Model())
		}
	})
}

func TestProfileSelection(t *testing.T) {
	w := newLayerWorkspace(t)
	w.write(w.user, `{"profiles":{"review":{"model":"anthropic:review","enable_web":true},"fast":{"model":"anthropic:fast"}}}`)
	w.write(w.projectPath(), `{"profiles":{"review":{"enable_task":true}}}`)

	if resolved := w.mustResolve(); resolved.Model() != defaultModel {
		t.Fatalf("no profile selected, model = %q", resolved.Model())
	}
	w.env["HARNESS_PROFILE"] = "review"
	resolved := w.mustResolve()
	if resolved.Model() != "anthropic:review" || !resolved.EnableWeb() || !resolved.EnableTask() {
		t.Fatal("profile definitions from user and project files did not merge")
	}
	if shown := settingFor(t, resolved, "enable_task"); shown.Layer != LayerProfile || shown.Source != w.projectPath() {
		t.Fatalf("profile attribution = %+v", shown)
	}
	w.flags.Profile = StringInput{Value: "fast", Set: true}
	if resolved := w.mustResolve(); resolved.Model() != "anthropic:fast" || resolved.EnableWeb() {
		t.Fatal("-profile did not outrank HARNESS_PROFILE")
	}
	w.flags.Profile = StringInput{Value: "missing", Set: true}
	_, err := w.resolve()
	var configErr *Error
	if !errors.As(err, &configErr) || configErr.Field != "profile" || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("undefined profile: %v", err)
	}
}

// A null profile definition unsets the same profile in lower files, and a
// null profiles member unsets them all. In a repository file the unset
// follows the trust rules: keys it could not set itself stay.
func TestProfileUnset(t *testing.T) {
	undefined := func(t *testing.T, w *layerWorkspace) {
		t.Helper()
		_, err := w.resolve()
		var configErr *Error
		if !errors.As(err, &configErr) || configErr.Field != "profile" || !strings.Contains(err.Error(), "review") {
			t.Fatalf("selecting an unset profile: %v", err)
		}
	}
	for name, project := range map[string]string{
		"definition": `{"profiles":{"review":null}}`,
		"all":        `{"profiles":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			w := newLayerWorkspace(t)
			w.write(w.user, `{"profiles":{"review":{"model":"anthropic:user-profile"}}}`)
			w.write(w.projectPath(), project)
			w.flags.Profile = StringInput{Value: "review", Set: true}
			undefined(t, w)
		})
	}
	t.Run("-c", func(t *testing.T) {
		for _, override := range []string{"profiles.review=null", "profiles=null"} {
			w := newLayerWorkspace(t)
			w.write(w.user, `{"profiles":{"review":{"model":"anthropic:user-profile"}}}`)
			w.flags.Overrides = StringsInput{Set: true, Values: []string{override}}
			w.flags.Profile = StringInput{Value: "review", Set: true}
			undefined(t, w)
		}
	})
	t.Run("redefined above the unset", func(t *testing.T) {
		w := newLayerWorkspace(t)
		w.write(w.user, `{"profiles":{"review":{"model":"anthropic:user-profile"}}}`)
		w.write(w.projectPath(), `{"profiles":{"review":null}}`)
		w.write(w.localPath(), `{"profiles":{"review":{"enable_task":true}}}`)
		w.flags.Profile = StringInput{Value: "review", Set: true}
		resolved := w.mustResolve()
		if resolved.Model() != defaultModel || !resolved.EnableTask() {
			t.Fatalf("model=%q enable_task=%t", resolved.Model(), resolved.EnableTask())
		}
	})
	t.Run("untrusted repository keeps gated keys", func(t *testing.T) {
		w := newLayerWorkspace(t)
		w.write(w.user, `{"profiles":{"review":{"model":"anthropic:user-profile","permission_mode_default":"plan"}}}`)
		w.write(w.projectPath(), `{"profiles":{"review":null}}`)
		w.flags.Profile = StringInput{Value: "review", Set: true}
		resolved := w.mustResolve()
		if resolved.Model() != defaultModel || resolved.PermissionModeDefault() != "plan" {
			t.Fatalf("model=%q mode=%q", resolved.Model(), resolved.PermissionModeDefault())
		}
		if warnings := strings.Join(resolved.Warnings(), "\n"); !strings.Contains(warnings, "permission_mode_default") || !strings.Contains(warnings, w.projectPath()) {
			t.Fatalf("warnings %q do not name the kept key", warnings)
		}
	})
}

func TestOverrideValues(t *testing.T) {
	for _, test := range []struct {
		name      string
		overrides []string
		check     func(*ResolvedConfig) bool
	}{
		{"json number", []string{"max_tokens=1234"}, func(c *ResolvedConfig) bool { return c.MaxTokens() == 1234 && c.MaxTokensConfigured() }},
		{"json boolean", []string{"enable_web=true"}, func(c *ResolvedConfig) bool { return c.EnableWeb() }},
		{"bare string", []string{"model=anthropic:bare"}, func(c *ResolvedConfig) bool { return c.Model() == "anthropic:bare" }},
		{"json string", []string{`log_level="warn"`}, func(c *ResolvedConfig) bool { return c.LogLevel() == "warn" }},
		{"json array", []string{`attach=["/a.md","/b.md"]`}, func(c *ResolvedConfig) bool { return slices.Equal(c.AttachPaths(), []string{"/a.md", "/b.md"}) }},
		{"later wins", []string{"model=anthropic:first", "model=anthropic:second"}, func(c *ResolvedConfig) bool { return c.Model() == "anthropic:second" }},
		{"null unsets", []string{"model=null"}, func(c *ResolvedConfig) bool { return c.Model() == defaultModel }},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := newLayerWorkspace(t)
			w.write(w.user, `{"model":"anthropic:user"}`)
			w.flags.Overrides = StringsInput{Set: true, Values: test.overrides}
			if resolved := w.mustResolve(); !test.check(resolved) {
				t.Fatalf("%v resolved model=%q settings=%+v", test.overrides, resolved.Model(), resolved.Settings())
			}
		})
	}
	for _, malformed := range []string{"model", "=x", ".model=x", "providers..x=y"} {
		w := newLayerWorkspace(t)
		w.flags.Overrides = StringsInput{Set: true, Values: []string{malformed}}
		_, err := w.resolve()
		var configErr *Error
		if !errors.As(err, &configErr) || configErr.Field != overrideSource {
			t.Fatalf("%q: err = %v", malformed, err)
		}
	}
	// A malformed override never echoes what was typed: it may be a credential.
	for _, malformed := range []string{"providers.relay.api_key sk-dummykey123456789", ".sk-dummykey123456789=x", "sk-dummykey123456789"} {
		w := newLayerWorkspace(t)
		w.flags.Overrides = StringsInput{Set: true, Values: []string{"model=anthropic:x", malformed}}
		_, err := w.resolve()
		if err == nil || strings.Contains(err.Error(), "dummykey") || !strings.Contains(err.Error(), "key.path=value") {
			t.Fatalf("%q: err = %v", malformed, err)
		}
	}
	w := newLayerWorkspace(t)
	w.flags.Overrides = StringsInput{Set: true, Values: []string{"max_tokens=lots"}}
	if _, err := w.resolve(); err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("mistyped override: %v", err)
	}
}

// An invalid HARNESS_* value fails only when it would take effect.
func TestInvalidEnvironmentValueFailsOnlyWhenItWins(t *testing.T) {
	w := newLayerWorkspace(t)
	w.env["HARNESS_MAX_TOKENS"] = "lots"
	_, err := w.resolve()
	var configErr *Error
	if !errors.As(err, &configErr) || configErr.Field != "max_tokens" {
		t.Fatalf("err = %v", err)
	}
	w.flags.Overrides = StringsInput{Set: true, Values: []string{"max_tokens=100"}}
	if resolved := w.mustResolve(); resolved.MaxTokens() != 100 {
		t.Fatalf("max tokens = %d", resolved.MaxTokens())
	}
}

// A field name that contains "token" or "key" is not a credential label: the
// message after it stays readable, while credentials inside it stay masked.
func TestErrorMasksCredentialsNotTheMessage(t *testing.T) {
	for _, test := range []struct {
		err  *Error
		want string
	}{
		{&Error{Field: "max_tokens", Err: errors.New("must be positive")}, "config max_tokens: must be positive"},
		{&Error{Field: "mcp_sampling_max_tokens", Err: errors.New("must be positive")}, "config mcp_sampling_max_tokens: must be positive"},
		{&Error{Field: "providers", Err: errors.New("relay: header Authorization: Bearer abcdefgh12345")}, "config providers: relay: header Authorization: [REDACTED]"},
	} {
		if got := test.err.Error(); got != test.want {
			t.Errorf("Error() = %q, want %q", got, test.want)
		}
	}
	w := newLayerWorkspace(t)
	w.write(w.projectPath(), `{"max_tokens": 0}`)
	if _, err := w.resolve(); err == nil || !strings.Contains(err.Error(), "max_tokens: must be positive") {
		t.Fatalf("err = %v", err)
	}
}

// A value that fails validation names the layer that set it, so a bad value
// in a repository file points at that file rather than looking like the
// operator's own setting.
func TestInvalidValueNamesTheLayerThatSetIt(t *testing.T) {
	for _, test := range []struct {
		name   string
		setup  func(w *layerWorkspace)
		field  string
		source func(w *layerWorkspace) string
	}{
		{"project scalar", func(w *layerWorkspace) { w.write(w.projectPath(), `{"max_tokens": 0}`) }, "max_tokens", func(w *layerWorkspace) string { return "project " + w.projectPath() }},
		{"project-local enum", func(w *layerWorkspace) { w.write(w.localPath(), `{"reasoning": "max"}`) }, "reasoning", func(w *layerWorkspace) string { return "project-local " + w.localPath() }},
		{"user fallback entry", func(w *layerWorkspace) { w.write(w.user, `{"fallback_models": {"plan": ["nope:model"]}}`) }, "fallback_models.plan[0]", func(w *layerWorkspace) string { return "user " + w.user }},
		{"environment", func(w *layerWorkspace) { w.env["HARNESS_BASH_TIMEOUT"] = "soon" }, "bash_timeout", func(*layerWorkspace) string { return "env HARNESS_BASH_TIMEOUT" }},
		{"override", func(w *layerWorkspace) {
			w.flags.Overrides = StringsInput{Set: true, Values: []string{"context_files.max_bytes=-1"}}
		}, "context_files.max_bytes", func(*layerWorkspace) string { return "override -c" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := newLayerWorkspace(t)
			test.setup(w)
			_, err := w.resolve()
			var configErr *Error
			if !errors.As(err, &configErr) || configErr.Field != test.field {
				t.Fatalf("err = %v, want a %s error", err, test.field)
			}
			if want := "(set by " + test.source(w) + ")"; !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want it to name %q", err, want)
			}
		})
	}
}

func TestConfigShowRedactsCredentials(t *testing.T) {
	w := newLayerWorkspace(t)
	w.write(w.user, `{
		"max_tokens": 4096,
		"web_search_url": "https://user:hunter2@search.example/",
		"mcp_servers": [{"name": "gh", "transport": "stdio", "command": "gh-mcp", "env": {"GITHUB_PAT": "pat-in-array"}}],
		"providers": {
			"gw": {"protocol": "openai-chat", "base_url": "https://gw.example/v1", "api_key": "sk-livekeyvalue1234", "headers": {"X-Tenant": "tenant-secret"}},
			"relay": {"protocol": "openai-chat", "base_url": "https://relay.example/v1", "api_key_env": "GW_KEY"}
		}
	}`)
	resolved := w.mustResolve()
	encoded, err := json.Marshal(resolved.Settings())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sk-livekeyvalue1234", "tenant-secret", "hunter2", "pat-in-array"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("config show leaked %q: %s", secret, encoded)
		}
	}
	if jsonString(t, settingFor(t, resolved, "providers.relay.api_key_env").Value) != "GW_KEY" {
		t.Fatal("a variable name is not a secret")
	}
	if shown := settingFor(t, resolved, "max_tokens"); string(shown.Value) != "4096" {
		t.Fatalf("max_tokens is not a credential: %s", shown.Value)
	}
}
