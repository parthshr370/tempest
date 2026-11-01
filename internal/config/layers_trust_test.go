package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// sessionStartHook is a hooks value with one SessionStart command.
func sessionStartHook(command string) string {
	return fmt.Sprintf(`{"SessionStart":[{"hooks":[{"type":"command","command":%q}]}]}`, command)
}

// gatedSamples holds, for every key a repository needs trust (or the
// operator) to set, a value that changes the resolved runtime configuration.
var gatedSamples = map[string]string{
	"mcp_config":                  `"repo-mcp.json"`,
	"mcp_servers":                 `[{"name":"repo","transport":"stdio","command":"repo-server"}]`,
	"lsp_config":                  `"repo-lsp.json"`,
	"debug_adapters":              `{"repo":{"command":"repo-adapter"}}`,
	"cloud_policy":                `"repo-policy.json"`,
	"permission_mode_default":     `"bypass"`,
	"permission_allow_tools":      `["bash"]`,
	"permission_deny_tools":       `["bash"]`,
	"plan":                        `true`,
	"attach":                      `["/etc/hosts"]`,
	"agent_dir":                   `"/tmp/repo-agent"`,
	"session_dir":                 `"/tmp/repo-sessions"`,
	"log_file":                    `"/tmp/repo.log"`,
	"faux_script":                 `"/tmp/repo-faux.json"`,
	"skill_paths":                 `["/tmp/repo-private-skills"]`,
	"enable_lsp":                  `true`,
	"enable_web":                  `true`,
	"providers":                   `{"repo":{"protocol":"openai-chat","base_url":"https://repo.example/v1","no_auth":true}}`,
	"anthropic_base_url":          `"https://repo.example"`,
	"openai_base_url":             `"https://repo.example/v1"`,
	"web_search_url":              `"https://repo.example/search"`,
	"model":                       `"openai:m@https://repo.example/v1"`,
	"plan_model":                  `"openai:m@https://repo.example/v1"`,
	"smol_model":                  `"openai:m@https://repo.example/v1"`,
	"fallback_models":             `{"default":["openai:m@https://repo.example/v1"]}`,
	"no_hooks":                    `true`,
	"disableAllHooks":             `true`,
	"trusted_hook_projects":       `["/"]`,
	"workspace_listing_max_files": `1000000`,
	"workspace_listing_max_bytes": `1048576`,
	"trusted_provider_projects":   `["/"]`,
	"environment":                 `"repo"`,
	"environments":                `{"repo":{"argv":["repo-env"]}}`,
	"hooks":                       sessionStartHook("printf repo > marker"),
}

// caseVariants are spellings encoding/json would match to key: other ASCII
// cases and the Unicode folds of s (U+017F) and k (U+212A).
func caseVariants(key string) []string {
	variants := []string{strings.ToUpper(key), strings.ToUpper(key[:1]) + key[1:]}
	if strings.Contains(key, "s") {
		variants = append(variants, strings.Replace(key, "s", "\u017f", 1))
	}
	if strings.Contains(key, "k") {
		variants = append(variants, strings.Replace(key, "k", "\u212a", 1))
	}
	return variants
}

// runtimeView is the part of a resolution the runtime consumes, without the
// per-test user settings path.
func runtimeView(c *ResolvedConfig) ResolvedConfig {
	view := *c
	view.settings, view.warnings, view.untrustedProviderFiles, view.workspace, view.settingsPath = nil, nil, nil, nil, ""
	return view
}

// A key that differs from a settings key only by case or Unicode folding is
// unknown everywhere: it never reaches decoding, whatever layer holds it.
func TestCaseVariantKeysNeverReachDecoding(t *testing.T) {
	keys := slices.Sorted(maps.Keys(gatedSamples))
	for _, setting := range projectSettingTrust {
		if _, ok := gatedSamples[setting.key]; setting.gate != gateNone && !ok {
			t.Fatalf("gated key %q has no sample value", setting.key)
		}
	}
	baseline := newLayerWorkspace(t)
	baseline.flags.Profile = StringInput{Value: "review", Set: true}
	baseline.write(baseline.user, `{"profiles":{"review":{}}}`)
	want := runtimeView(baseline.mustResolve())

	placements := map[string]func(w *layerWorkspace, member string){
		"project":       func(w *layerWorkspace, member string) { w.write(w.projectPath(), "{"+member+"}") },
		"project-local": func(w *layerWorkspace, member string) { w.write(w.localPath(), "{"+member+"}") },
		"project profile": func(w *layerWorkspace, member string) {
			w.write(w.projectPath(), `{"profiles":{"review":{`+member+`}}}`)
		},
		"user": func(w *layerWorkspace, member string) {
			w.write(w.user, `{"profiles":{"review":{}},`+member+`}`)
		},
	}
	for _, key := range keys {
		for _, variant := range caseVariants(key) {
			for placement, place := range placements {
				t.Run(placement+"/"+variant, func(t *testing.T) {
					w := newLayerWorkspace(t)
					w.flags.Profile = StringInput{Value: "review", Set: true}
					if placement != "user" {
						w.write(w.user, `{"profiles":{"review":{}}}`)
					}
					place(w, fmt.Sprintf("%q:%s", variant, gatedSamples[key]))
					resolved, err := w.resolve()
					if err != nil {
						t.Fatalf("resolve: %v", err)
					}
					if got := runtimeView(resolved); !reflect.DeepEqual(got, want) {
						t.Fatalf("%q in %s changed the runtime configuration", variant, placement)
					}
					for _, setting := range resolved.Settings() {
						if strings.EqualFold(strings.SplitN(setting.Key, ".", 2)[0], variant) && setting.Key != key {
							t.Fatalf("config show lists %q", setting.Key)
						}
					}
					if !strings.Contains(strings.Join(resolved.Warnings(), "\n"), variant) {
						t.Fatalf("no unknown-setting warning names %q: %q", variant, resolved.Warnings())
					}
				})
			}
		}
	}

	t.Run("trusted project", func(t *testing.T) {
		w := newLayerWorkspace(t)
		w.flags.TrustProjectHooks = BoolInput{Value: true, Set: true}
		w.flags.TrustProjectProviders = BoolInput{Value: true, Set: true}
		w.write(w.projectPath(), `{"PERMISSION_MODE_DEFAULT":"bypass","Hooks":`+sessionStartHook("exit 0")+`,"providers":{"openai":{"BASE_URL":"https://repo.example/v1"}}}`)
		resolved := w.mustResolve()
		if resolved.PermissionModeDefault() != defaultPermissionMode || len(resolved.Hooks().Hooks) != 0 || resolved.Providers()["openai"].BaseURL != "" {
			t.Fatal("a case variant applied in a trusted project")
		}
	})
}

// projectSettingTrust is the trust a repository file needs before a value of
// each settings key applies. It comes from an audit of every fileSettings
// field for running code, reading local files into the model context,
// writing outside the repository, widening permissions and choosing where
// credentials go. It is written out here, not read from projectGates, so a
// gate deleted from the resolver fails TestEveryRiskyProjectSettingNeedsTrust.
// A key whose gate depends on its value appears once per class.
var projectSettingTrust = []struct {
	key   string
	value string
	gate  trustGate
}{
	// Runs code.
	{"hooks", sessionStartHook("printf repo > marker"), gateExecutable},
	{"mcp_config", `"repo-mcp.json"`, gateExecutable},
	{"mcp_servers", `[{"name":"repo","transport":"stdio","command":"repo-server"}]`, gateExecutable},
	{"lsp_config", `"repo-lsp.json"`, gateExecutable},
	{"enable_lsp", `true`, gateExecutable},
	{"debug_adapters", `{"repo":{"command":"repo-adapter"}}`, gateExecutable},
	{"cloud_policy", `"repo-policy.json"`, gateExecutable},
	{"faux_script", `"/tmp/repo-faux.json"`, gateExecutable},
	// Reads local files into the model context.
	{"attach", `["/outside/private.txt"]`, gateExecutable},
	{"skill_paths", `["/outside/private-skills"]`, gateExecutable},
	{"agent_dir", `"/outside/agent"`, gateExecutable},
	// Writes outside the repository.
	{"session_dir", `"/outside/sessions"`, gateExecutable},
	{"log_file", `"/outside/repo.log"`, gateExecutable},
	// Opens a network channel the model can send repository contents through.
	{"enable_web", `true`, gateExecutable},
	// Widens permissions.
	{"permission_allow_tools", `["bash"]`, gateExecutable},
	{"permission_mode_default", `"bypass"`, gateExecutable},
	{"permission_deny_tools", `null`, gateExecutable},
	{"plan", `false`, gateExecutable},
	// Raises a session-start listing cap above the operator's value.
	{"workspace_listing_max_files", `10001`, gateExecutable},
	{"workspace_listing_max_files", `1000000`, gateExecutable},
	{"workspace_listing_max_bytes", `4097`, gateExecutable},
	{"workspace_listing_max_bytes", `1048576`, gateExecutable},
	// Chooses where credentials go.
	{"providers", `{"repo":{"protocol":"openai-chat","base_url":"https://repo.example/v1","no_auth":true}}`, gateCredentials},
	{"anthropic_base_url", `"https://repo.example"`, gateCredentials},
	{"openai_base_url", `"https://repo.example/v1"`, gateCredentials},
	{"web_search_url", `"https://repo.example/search"`, gateCredentials},
	{"model", `"openai:m@https://repo.example/v1"`, gateCredentials},
	{"plan_model", `"openai:m@https://repo.example/v1"`, gateCredentials},
	{"smol_model", `"openai:m@http://repo.example/v1"`, gateCredentials},
	{"fallback_models", `{"plan":["anthropic:backup","openai:m@https://repo.example/v1"]}`, gateCredentials},
	// Decides trust or replaces the operator's runtime.
	{"no_hooks", `true`, gateOperator},
	{"disableAllHooks", `true`, gateOperator},
	{"trusted_hook_projects", `["/"]`, gateOperator},
	{"trusted_provider_projects", `["/"]`, gateOperator},
	{"environment", `"repo"`, gateOperator},
	{"environments", `{"repo":{"argv":["repo-env"]}}`, gateOperator},
	// Applies without trust.
	{"model", `"anthropic:project"`, gateNone},
	{"plan_model", `"anthropic:project-plan"`, gateNone},
	{"smol_model", `"anthropic:project-smol"`, gateNone},
	{"fallback_models", `{"default":["anthropic:project-backup"],"smol":[]}`, gateNone},
	{"reasoning", `"high"`, gateNone},
	{"plan_reasoning", `"high"`, gateNone},
	{"smol_reasoning", `"off"`, gateNone},
	{"max_tokens", `1234`, gateNone},
	{"bash_timeout", `"7s"`, gateNone},
	{"log_level", `"debug"`, gateNone},
	{"log_file_max_bytes", `4096`, gateNone},
	{"enable_web", `false`, gateNone},
	{"enable_task", `true`, gateNone},
	{"permission_mode_default", `"plan"`, gateNone},
	{"plan", `true`, gateNone},
	{"permission_deny_tools", `["bash"]`, gateNone},
	{"retry_enabled", `false`, gateNone},
	{"retry_max_attempts", `2`, gateNone},
	{"retry_base_delay_ms", `1`, gateNone},
	{"retry_backoff_cap_ms", `9000`, gateNone},
	{"retry_max_delay_ms", `100`, gateNone},
	{"retry_jitter_min", `0`, gateNone},
	{"retry_jitter_max", `1`, gateNone},
	// The session-start listing may be turned off or its caps lowered below
	// the operator's values; the operator's value, true and the default caps
	// here, applies again unchanged.
	{"workspace_listing", `false`, gateNone},
	{"workspace_listing", `true`, gateNone},
	{"workspace_listing_max_files", `1`, gateNone},
	{"workspace_listing_max_files", `10000`, gateNone},
	{"workspace_listing_max_bytes", `1`, gateNone},
	{"workspace_listing_max_bytes", `4096`, gateNone},
}

// Every settings key has an audited trust class, and each repository value
// that needs trust is dropped from the project file, the project-local file
// and a selected project profile with the trust warning, and applies only
// under the trust its class names.
func TestEveryRiskyProjectSettingNeedsTrust(t *testing.T) {
	audited := map[string]bool{}
	for _, setting := range projectSettingTrust {
		audited[setting.key] = true
	}
	schema := reflect.TypeFor[fileSettings]()
	for field := range schema.Fields() {
		key, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if !audited[key] {
			t.Errorf("settings key %q has no audited trust class in projectSettingTrust", key)
		}
		delete(audited, key)
	}
	for key := range audited {
		t.Errorf("projectSettingTrust lists %q, which is not a settings key", key)
	}

	// The operator's settings make a loosening permission value observable.
	const user = `{"profiles":{"review":{}},"permission_deny_tools":["bash"],"plan":true}`
	newWorkspace := func(t *testing.T) *layerWorkspace {
		w := newLayerWorkspace(t)
		w.flags.Profile = StringInput{Value: "review", Set: true}
		w.write(w.user, user)
		return w
	}
	want := runtimeView(newWorkspace(t).mustResolve())
	refusals := map[trustGate]string{
		gateExecutable:  "needs -trust-project-hooks",
		gateCredentials: "needs -trust-project-providers",
		gateOperator:    "operator-only",
	}
	placements := []struct {
		name  string
		file  func(w *layerWorkspace) string
		place func(w *layerWorkspace, member string)
	}{
		{"project", (*layerWorkspace).projectPath, func(w *layerWorkspace, member string) { w.write(w.projectPath(), "{"+member+"}") }},
		{"project-local", (*layerWorkspace).localPath, func(w *layerWorkspace, member string) { w.write(w.localPath(), "{"+member+"}") }},
		{"project profile", (*layerWorkspace).projectPath, func(w *layerWorkspace, member string) {
			w.write(w.projectPath(), `{"profiles":{"review":{`+member+`}}}`)
		}},
	}
	for _, setting := range projectSettingTrust {
		if setting.gate == gateNone {
			t.Run("untrusted applies/"+setting.key+"="+setting.value, func(t *testing.T) {
				w := newLayerWorkspace(t)
				w.write(w.projectPath(), fmt.Sprintf("{%q:%s}", setting.key, setting.value))
				resolved := w.mustResolve()
				if warnings := resolved.Warnings(); len(warnings) != 0 {
					t.Fatalf("warnings = %q", warnings)
				}
				// An object setting shows as its leaves.
				for _, shown := range resolved.Settings() {
					if top, _, _ := strings.Cut(shown.Key, "."); top == setting.key && shown.Layer != LayerProject {
						t.Fatalf("config show = %+v, want the project layer", shown)
					}
				}
				if shown := resolved.Settings(); !slices.ContainsFunc(shown, func(s Setting) bool {
					top, _, _ := strings.Cut(s.Key, ".")
					return top == setting.key
				}) {
					t.Fatalf("config show lists no %q in %+v", setting.key, shown)
				}
			})
			continue
		}
		// disableAllHooks is Claude Code's spelling of no_hooks.
		warned := setting.key
		if warned == "disableAllHooks" {
			warned = "no_hooks"
		}
		for _, placement := range placements {
			gate := setting.gate
			if placement.name == "project profile" && setting.key == "hooks" {
				// A profile replaces hook arrays, which would remove the operator's.
				gate = gateOperator
			}
			refused := warned + " (" + refusals[gate]
			resolveWith := func(t *testing.T, hooksTrust, providersTrust bool) (*layerWorkspace, *ResolvedConfig, error) {
				t.Helper()
				w := newWorkspace(t)
				w.flags.TrustProjectHooks = BoolInput{Value: hooksTrust, Set: true}
				w.flags.TrustProjectProviders = BoolInput{Value: providersTrust, Set: true}
				placement.place(w, fmt.Sprintf("%q:%s", setting.key, setting.value))
				resolved, err := w.resolve()
				return w, resolved, err
			}
			mustResolveWith := func(t *testing.T, hooksTrust, providersTrust bool) (*layerWorkspace, *ResolvedConfig) {
				t.Helper()
				w, resolved, err := resolveWith(t, hooksTrust, providersTrust)
				if err != nil {
					t.Fatalf("resolve: %v", err)
				}
				return w, resolved
			}
			t.Run(placement.name+"/"+setting.key+"="+setting.value, func(t *testing.T) {
				w, resolved := mustResolveWith(t, false, false)
				if got := runtimeView(resolved); !reflect.DeepEqual(got, want) {
					t.Fatalf("an untrusted %s changed the runtime configuration", placement.name)
				}
				warnings := strings.Join(resolved.Warnings(), "\n")
				if !strings.Contains(warnings, "project settings ignored: ") || !strings.Contains(warnings, placement.file(w)+": "+refused) {
					t.Fatalf("warnings %q do not name %s in %s", warnings, refused, placement.file(w))
				}
				for _, shown := range resolved.Settings() {
					if top, _, _ := strings.Cut(shown.Key, "."); top == warned && shown.Source == placement.file(w) {
						t.Fatalf("config show lists the dropped %+v", shown)
					}
				}
				// The other trust, or both for an operator-only key, does not admit it.
				_, resolved = mustResolveWith(t, gate != gateExecutable, gate != gateCredentials)
				if warnings := strings.Join(resolved.Warnings(), "\n"); !strings.Contains(warnings, refused) {
					t.Fatalf("warnings %q under the wrong trust do not name %s", warnings, refused)
				}
				if gate == gateOperator {
					return
				}
				// An admitted value applies, or fails its own validation, as a
				// trusted path to a missing file does.
				_, resolved, err := resolveWith(t, gate == gateExecutable, gate == gateCredentials)
				var configErr *Error
				if err != nil && (!errors.As(err, &configErr) || configErr.Field != setting.key) {
					t.Fatalf("trusted resolve: %v", err)
				}
				if err == nil && strings.Contains(strings.Join(resolved.Warnings(), "\n"), warned+" (") {
					t.Fatalf("trusted %s still dropped: %q", setting.key, resolved.Warnings())
				}
			})
		}
	}
}

// An untrusted repository cannot attach a file outside it to the session:
// the attachment tool would hand its contents to the model. That holds for
// the project file, a selected project profile, and a project file that is a
// symlink to a file outside the repository.
func TestProjectAttachNeedsTrust(t *testing.T) {
	private := filepath.Join(t.TempDir(), "private.txt")
	if err := os.WriteFile(private, []byte("P1_ATTACHMENT_PRIVATE_SENTINEL"), 0o600); err != nil {
		t.Fatal(err)
	}
	attach := fmt.Sprintf(`{"attach":[%q]}`, private)
	for name, place := range map[string]func(t *testing.T, w *layerWorkspace){
		"project file": func(_ *testing.T, w *layerWorkspace) { w.write(w.projectPath(), attach) },
		"project profile": func(_ *testing.T, w *layerWorkspace) {
			w.write(w.projectPath(), `{"profiles":{"review":`+attach+`}}`)
		},
		"symlinked project file": func(t *testing.T, w *layerWorkspace) {
			outside := filepath.Join(t.TempDir(), "settings.json")
			w.write(outside, attach)
			if err := os.Symlink(outside, w.projectPath()); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newLayerWorkspace(t)
			w.flags.Profile = StringInput{Value: "review", Set: true}
			w.write(w.user, `{"profiles":{"review":{}}}`)
			place(t, w)
			resolved := w.mustResolve()
			if paths := resolved.AttachPaths(); len(paths) != 0 {
				t.Fatalf("untrusted project attached %v", paths)
			}
			warnings := strings.Join(resolved.Warnings(), "\n")
			if !strings.Contains(warnings, "project settings ignored: ") || !strings.Contains(warnings, w.projectPath()+": attach (needs -trust-project-hooks") {
				t.Fatalf("warnings %q do not name attach in %s", warnings, w.projectPath())
			}
			w.flags.TrustProjectHooks = BoolInput{Value: true, Set: true}
			if paths := w.mustResolve().AttachPaths(); !slices.Equal(paths, []string{private}) {
				t.Fatalf("trusted project attach = %v", paths)
			}
		})
	}
}

func TestProjectSkillPathsAndLSPNeedTrust(t *testing.T) {
	outside := t.TempDir()
	w := newLayerWorkspace(t)
	w.write(w.projectPath(), fmt.Sprintf(`{"skill_paths":[%q],"enable_lsp":true}`, outside))
	resolved := w.mustResolve()
	if len(resolved.SkillPaths()) != 0 || resolved.LSP().Enabled {
		t.Fatalf("untrusted project set skill_paths=%v enable_lsp=%t", resolved.SkillPaths(), resolved.LSP().Enabled)
	}
	warnings := strings.Join(resolved.Warnings(), "\n")
	for _, key := range []string{"skill_paths", "enable_lsp", w.projectPath()} {
		if !strings.Contains(warnings, key) {
			t.Fatalf("trust warning %q does not name %s", warnings, key)
		}
	}
	w.flags.TrustProjectHooks = BoolInput{Value: true, Set: true}
	resolved = w.mustResolve()
	if !slices.Equal(resolved.SkillPaths(), []string{outside}) || !resolved.LSP().Enabled {
		t.Fatal("trusted project skill_paths or enable_lsp not applied")
	}
}

// An untrusted repository may turn the session-start listing off or lower
// its caps, but never above the operator's values: not by a larger number,
// not by enabling a listing the operator turned off, and not by null or an
// invalid value that would fall back to the built-in default. Trust lifts
// the ceiling.
func TestProjectListingCapsCannotRaiseTheOperatorsValues(t *testing.T) {
	const operator = `{"workspace_listing":false,"workspace_listing_max_files":500,"workspace_listing_max_bytes":2048}`
	operatorPolicy := WorkspaceListing{Disabled: true, MaxFiles: 500, MaxBytes: 2048}
	for _, testCase := range []struct {
		name    string
		project string
		want    WorkspaceListing
		dropped []string
	}{
		{"raise", `{"workspace_listing":true,"workspace_listing_max_files":600,"workspace_listing_max_bytes":3000}`, operatorPolicy, []string{"workspace_listing", "workspace_listing_max_files", "workspace_listing_max_bytes"}},
		{"lower", `{"workspace_listing":false,"workspace_listing_max_files":200,"workspace_listing_max_bytes":1024}`, WorkspaceListing{Disabled: true, MaxFiles: 200, MaxBytes: 1024}, nil},
		{"equal", `{"workspace_listing_max_files":500,"workspace_listing_max_bytes":2048}`, operatorPolicy, nil},
		{"null resets to the default", `{"workspace_listing_max_files":null,"workspace_listing_max_bytes":null}`, operatorPolicy, []string{"workspace_listing_max_files", "workspace_listing_max_bytes"}},
		{"invalid", `{"workspace_listing_max_files":0,"workspace_listing_max_bytes":-1}`, operatorPolicy, []string{"workspace_listing_max_files", "workspace_listing_max_bytes"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			w := newLayerWorkspace(t)
			w.write(w.user, operator)
			w.write(w.projectPath(), testCase.project)
			resolved := w.mustResolve()
			if got := resolved.WorkspaceListing(); got != testCase.want {
				t.Fatalf("untrusted listing policy = %+v, want %+v", got, testCase.want)
			}
			warnings := strings.Join(resolved.Warnings(), "\n")
			for _, key := range testCase.dropped {
				if !strings.Contains(warnings, key+" (needs -trust-project-hooks") {
					t.Fatalf("warnings %q do not name the dropped %s", warnings, key)
				}
			}
			if testCase.dropped == nil && warnings != "" {
				t.Fatalf("warnings = %q", warnings)
			}
		})
	}

	w := newLayerWorkspace(t)
	w.write(w.user, operator)
	w.write(w.projectPath(), `{"workspace_listing":true,"workspace_listing_max_files":600,"workspace_listing_max_bytes":3000}`)
	w.flags.TrustProjectHooks = BoolInput{Value: true, Set: true}
	if got, want := w.mustResolve().WorkspaceListing(), (WorkspaceListing{MaxFiles: 600, MaxBytes: 3000}); got != want {
		t.Fatalf("trusted listing policy = %+v, want %+v", got, want)
	}
}

// resolvedHookCommands lists event's commands in run order.
func resolvedHookCommands(c *ResolvedConfig, event string) []string {
	var commands []string
	for _, group := range c.Hooks().Hooks[event] {
		for _, command := range group.Hooks {
			commands = append(commands, command.Command)
		}
	}
	return commands
}

// Project hooks are one more settings layer: trusted repository hooks append
// to the operator's, higher layers and managed requirements apply to them,
// and config show lists them.
func TestProjectHooksResolveAsALayer(t *testing.T) {
	trusted := func(w *layerWorkspace) { w.flags.TrustProjectHooks = BoolInput{Value: true, Set: true} }

	t.Run("append in file order and show", func(t *testing.T) {
		w := newLayerWorkspace(t)
		trusted(w)
		w.write(w.user, `{"hooks":`+sessionStartHook("user")+`}`)
		w.write(w.projectPath(), `{"hooks":`+sessionStartHook("project")+`}`)
		w.write(w.localPath(), `{"hooks":`+sessionStartHook("local")+`}`)
		resolved := w.mustResolve()
		if got := resolvedHookCommands(resolved, "SessionStart"); !slices.Equal(got, []string{"user", "project", "local"}) {
			t.Fatalf("SessionStart commands = %v", got)
		}
		shown := settingFor(t, resolved, "hooks.SessionStart")
		if shown.Layer != LayerProjectLocal || shown.Source != w.localPath() || !strings.Contains(string(shown.Value), `"project"`) {
			t.Fatalf("config show hooks = %+v", shown)
		}
	})

	t.Run("untrusted hooks are dropped with a warning", func(t *testing.T) {
		w := newLayerWorkspace(t)
		w.write(w.user, `{"hooks":`+sessionStartHook("user")+`}`)
		w.write(w.projectPath(), `{"hooks":`+sessionStartHook("project")+`}`)
		resolved := w.mustResolve()
		if got := resolvedHookCommands(resolved, "SessionStart"); !slices.Equal(got, []string{"user"}) {
			t.Fatalf("SessionStart commands = %v", got)
		}
		warnings := strings.Join(resolved.Warnings(), "\n")
		if !strings.Contains(warnings, "hooks") || !strings.Contains(warnings, w.projectPath()) {
			t.Fatalf("warnings %q do not name the dropped project hooks", warnings)
		}
	})

	t.Run("a repository cannot remove operator hooks", func(t *testing.T) {
		for _, project := range []string{`{"hooks":null}`, `{"hooks":{"SessionStart":null}}`, `{"hooks":{"SessionStart":[]}}`} {
			w := newLayerWorkspace(t)
			trusted(w)
			w.write(w.user, `{"hooks":`+sessionStartHook("user")+`}`)
			w.write(w.projectPath(), project)
			if got := resolvedHookCommands(w.mustResolve(), "SessionStart"); !slices.Equal(got, []string{"user"}) {
				t.Fatalf("%s: SessionStart commands = %v", project, got)
			}
		}
	})

	t.Run("-c hooks=null unsets project hooks", func(t *testing.T) {
		w := newLayerWorkspace(t)
		trusted(w)
		w.write(w.projectPath(), `{"hooks":`+sessionStartHook("project")+`}`)
		w.flags.Overrides = StringsInput{Set: true, Values: []string{"hooks=null"}}
		if hooks := w.mustResolve().Hooks().Hooks; len(hooks) != 0 {
			t.Fatalf("hooks after -c hooks=null = %+v", hooks)
		}
	})

	t.Run("managed requirements pin project hooks", func(t *testing.T) {
		w := newLayerWorkspace(t)
		trusted(w)
		w.write(w.managed, `{"requirements":{"hooks":{"SessionStart":[]}}}`)
		w.write(w.projectPath(), `{"hooks":`+sessionStartHook("project")+`}`)
		_, err := w.resolve()
		var configErr *Error
		if !errors.As(err, &configErr) || configErr.Field != "hooks.SessionStart" {
			t.Fatalf("err = %v, want a hooks.SessionStart requirement error", err)
		}
		for _, name := range []string{w.managed, w.projectPath()} {
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("error %q does not name %s", err, name)
			}
		}
	})

	t.Run("operator-only trust settings in a repository", func(t *testing.T) {
		w := newLayerWorkspace(t)
		w.write(w.user, `{"hooks":`+sessionStartHook("user")+`}`)
		w.write(w.projectPath(), fmt.Sprintf(`{"trusted_hook_projects":[%q],"no_hooks":true,"hooks":%s}`, w.root, sessionStartHook("project")))
		resolved := w.mustResolve()
		if resolved.Hooks().NoHooks || !slices.Equal(resolvedHookCommands(resolved, "SessionStart"), []string{"user"}) {
			t.Fatalf("repository granted itself trust or disabled operator hooks: %+v", resolved.Hooks())
		}
	})
}

// An invalid project file the operator has not trusted is skipped with a
// warning; operator files, trusted projects and strict mode stay fatal.
func TestInvalidUntrustedProjectFileIsSkipped(t *testing.T) {
	invalid := map[string]func(w *layerWorkspace){
		"malformed json":        func(w *layerWorkspace) { w.write(w.projectPath(), `{ broken json`) },
		"not an object":         func(w *layerWorkspace) { w.write(w.projectPath(), `[1]`) },
		"wrong type":            func(w *layerWorkspace) { w.write(w.projectPath(), `{"model":"anthropic:project","max_tokens":"lots"}`) },
		"profiles not settings": func(w *layerWorkspace) { w.write(w.projectPath(), `{"profiles":5}`) },
		"directory": func(w *layerWorkspace) {
			if err := os.Mkdir(w.projectPath(), 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"oversized": func(w *layerWorkspace) {
			w.write(w.projectPath(), `{"model":"`+strings.Repeat("x", maxSettingsFileBytes)+`"}`)
		},
		"fifo": func(w *layerWorkspace) {
			if err := syscall.Mkfifo(w.projectPath(), 0o600); err != nil {
				t.Skip(err)
			}
		},
	}
	resolveWithin := func(t *testing.T, w *layerWorkspace) (*ResolvedConfig, error) {
		t.Helper()
		type result struct {
			resolved *ResolvedConfig
			err      error
		}
		done := make(chan result, 1)
		go func() {
			resolved, err := w.resolve()
			done <- result{resolved, err}
		}()
		select {
		case got := <-done:
			return got.resolved, got.err
		case <-time.After(10 * time.Second):
			t.Fatal("resolution blocked reading a project settings file")
			return nil, nil
		}
	}
	for name, write := range invalid {
		t.Run(name, func(t *testing.T) {
			w := newLayerWorkspace(t)
			w.write(w.user, `{"model":"anthropic:user"}`)
			write(w)
			w.flags.Overrides = StringsInput{Set: true, Values: []string{"max_tokens=1000"}}
			resolved, err := resolveWithin(t, w)
			if err != nil {
				t.Fatalf("untrusted invalid project file stopped resolution: %v", err)
			}
			if resolved.Model() != "anthropic:user" || resolved.MaxTokens() != 1000 {
				t.Fatalf("operator settings lost: model=%q max_tokens=%d", resolved.Model(), resolved.MaxTokens())
			}
			if !strings.Contains(strings.Join(resolved.Warnings(), "\n"), w.projectPath()) {
				t.Fatalf("warnings %q do not name the skipped file", resolved.Warnings())
			}

			for fatal, set := range map[string]func(*layerWorkspace){
				"strict":  func(w *layerWorkspace) { w.flags.StrictConfig = BoolInput{Value: true, Set: true} },
				"trusted": func(w *layerWorkspace) { w.flags.TrustProjectHooks = BoolInput{Value: true, Set: true} },
			} {
				set(w)
				if _, err := resolveWithin(t, w); err == nil || !strings.Contains(err.Error(), w.projectPath()) {
					t.Fatalf("%s: err = %v, want an error naming the project file", fatal, err)
				}
				w.flags.StrictConfig, w.flags.TrustProjectHooks = BoolInput{}, BoolInput{}
			}
		})
	}

	t.Run("a dropped key of the wrong type is not an error", func(t *testing.T) {
		w := newLayerWorkspace(t)
		w.write(w.projectPath(), `{"model":"anthropic:project","mcp_servers":5}`)
		resolved := w.mustResolve()
		if resolved.Model() != "anthropic:project" {
			t.Fatalf("model = %q", resolved.Model())
		}
	})

	t.Run("operator files stay fatal", func(t *testing.T) {
		for name, path := range map[string]func(w *layerWorkspace) string{
			"user":    func(w *layerWorkspace) string { return w.user },
			"managed": func(w *layerWorkspace) string { return w.managed },
		} {
			w := newLayerWorkspace(t)
			w.write(path(w), `{ broken json`)
			if _, err := w.resolve(); err == nil || !strings.Contains(err.Error(), path(w)) {
				t.Fatalf("%s: err = %v", name, err)
			}
		}
	})
}

// A session in another workspace (serve, ACP, the SDK) gets that workspace's
// hooks from the same resolution: its trust, -c overrides and requirements.
func TestWorkspaceHooksResolveAnotherWorkspace(t *testing.T) {
	other := newLayerWorkspace(t)
	other.write(other.projectPath(), `{"hooks":`+sessionStartHook("other project")+`}`)
	resolveFor := func(t *testing.T, w *layerWorkspace) ([]string, []string, error) {
		t.Helper()
		resolved := w.mustResolve()
		if settings, warnings, err := resolved.WorkspaceHooks(w.root); err != nil || warnings != nil || !reflect.DeepEqual(settings, resolved.Hooks()) {
			t.Fatalf("own workspace: settings=%+v warnings=%q err=%v", settings, warnings, err)
		}
		settings, warnings, err := resolved.WorkspaceHooks(other.root)
		var commands []string
		for _, group := range settings.Hooks["SessionStart"] {
			for _, command := range group.Hooks {
				commands = append(commands, command.Command)
			}
		}
		return commands, warnings, err
	}

	w := newLayerWorkspace(t)
	w.write(w.user, `{"hooks":`+sessionStartHook("user")+`}`)
	commands, warnings, err := resolveFor(t, w)
	if err != nil || !slices.Equal(commands, []string{"user"}) || !strings.Contains(strings.Join(warnings, "\n"), other.projectPath()) {
		t.Fatalf("untrusted: commands=%v warnings=%q err=%v", commands, warnings, err)
	}

	w.write(w.user, fmt.Sprintf(`{"trusted_hook_projects":[%q],"hooks":%s}`, other.root, sessionStartHook("user")))
	if commands, _, err := resolveFor(t, w); err != nil || !slices.Equal(commands, []string{"user", "other project"}) {
		t.Fatalf("trusted: commands=%v err=%v", commands, err)
	}

	w.flags.Overrides = StringsInput{Set: true, Values: []string{"hooks=null"}}
	if commands, _, err := resolveFor(t, w); err != nil || len(commands) != 0 {
		t.Fatalf("-c hooks=null: commands=%v err=%v", commands, err)
	}

	w.flags.Overrides = StringsInput{}
	w.write(w.managed, `{"requirements":{"hooks":{"SessionStart":[]}}}`)
	w.write(w.user, fmt.Sprintf(`{"trusted_hook_projects":[%q]}`, other.root))
	if _, _, err := resolveFor(t, w); err == nil || !strings.Contains(err.Error(), other.projectPath()) {
		t.Fatalf("pinned hooks: err=%v", err)
	}
}

// An untrusted repository narrows permissions and never widens them: its deny
// lists add to the lists below it, and allow lists, looser modes and nulls
// of a permission key are dropped with the trust warning. A trusted project
// replaces them like every other settings layer.
func TestProjectCannotWidenPermissions(t *testing.T) {
	const userDeny = `["bash","web_fetch"]`
	untrustedDeny := []struct {
		name     string
		place    func(w *layerWorkspace)
		want     []string
		layer    string
		source   func(w *layerWorkspace) string
		warnings []string
	}{
		{"project adds an entry", func(w *layerWorkspace) { w.write(w.projectPath(), `{"permission_deny_tools":["edit","bash"]}`) },
			[]string{"bash", "web_fetch", "edit"}, LayerProject, (*layerWorkspace).projectPath, nil},
		{"project empties the list", func(w *layerWorkspace) { w.write(w.projectPath(), `{"permission_deny_tools":[]}`) },
			[]string{"bash", "web_fetch"}, LayerUser, func(w *layerWorkspace) string { return w.user }, nil},
		{"project nulls the list", func(w *layerWorkspace) { w.write(w.projectPath(), `{"permission_deny_tools":null}`) },
			[]string{"bash", "web_fetch"}, LayerUser, func(w *layerWorkspace) string { return w.user }, []string{"permission_deny_tools"}},
		{"project-local replaces the list", func(w *layerWorkspace) {
			w.write(w.projectPath(), `{"permission_deny_tools":["edit"]}`)
			w.write(w.localPath(), `{"permission_deny_tools":["write"]}`)
		}, []string{"bash", "web_fetch", "edit", "write"}, LayerProjectLocal, (*layerWorkspace).localPath, nil},
		{"project profile replaces the list", func(w *layerWorkspace) {
			w.write(w.projectPath(), `{"profiles":{"review":{"permission_deny_tools":["write"]}}}`)
			w.flags.Profile = StringInput{Value: "review", Set: true}
		}, []string{"bash", "web_fetch", "write"}, LayerProfile, (*layerWorkspace).projectPath, nil},
		{"project profile nulls the list", func(w *layerWorkspace) {
			w.write(w.projectPath(), `{"profiles":{"review":{"permission_deny_tools":null}}}`)
			w.flags.Profile = StringInput{Value: "review", Set: true}
		}, []string{"bash", "web_fetch"}, LayerUser, func(w *layerWorkspace) string { return w.user }, []string{"permission_deny_tools"}},
		{"project nulls a profile holding the list", func(w *layerWorkspace) {
			w.write(w.user, `{"profiles":{"review":{"permission_deny_tools":["bash","web_fetch"]}}}`)
			w.write(w.projectPath(), `{"profiles":{"review":null}}`)
			w.flags.Profile = StringInput{Value: "review", Set: true}
		}, []string{"bash", "web_fetch"}, LayerProfile, func(w *layerWorkspace) string { return w.user }, []string{"permission_deny_tools"}},
	}
	for _, test := range untrustedDeny {
		t.Run("untrusted/"+test.name, func(t *testing.T) {
			w := newLayerWorkspace(t)
			w.write(w.user, `{"permission_deny_tools":`+userDeny+`}`)
			test.place(w)
			resolved := w.mustResolve()
			if got := resolved.PermissionDenyTools(); !slices.Equal(got, test.want) {
				t.Fatalf("deny tools = %v, want %v", got, test.want)
			}
			shown := settingFor(t, resolved, "permission_deny_tools")
			var shownDeny []string
			if err := json.Unmarshal(shown.Value, &shownDeny); err != nil || !slices.Equal(shownDeny, test.want) || shown.Layer != test.layer || shown.Source != test.source(w) {
				t.Fatalf("config show = %+v, want %v from %s %s", shown, test.want, test.layer, test.source(w))
			}
			warnings := strings.Join(resolved.Warnings(), "\n")
			for _, name := range test.warnings {
				if !strings.Contains(warnings, name) || !strings.Contains(warnings, w.projectPath()) || !strings.Contains(warnings, "-trust-project-hooks") {
					t.Fatalf("warnings %q do not name %s in %s", warnings, name, w.projectPath())
				}
			}
		})
	}

	looser := []struct {
		name    string
		user    string
		project string
		key     string
		check   func(c *ResolvedConfig) bool
	}{
		{"allow entry", `{}`, `{"permission_allow_tools":["bash"]}`, "permission_allow_tools", func(c *ResolvedConfig) bool { return len(c.PermissionAllowTools()) == 0 }},
		{"bypass mode", `{"permission_mode_default":"plan"}`, `{"permission_mode_default":"bypass"}`, "permission_mode_default", func(c *ResolvedConfig) bool { return c.PermissionModeDefault() == "plan" }},
		{"default mode", `{"permission_mode_default":"plan"}`, `{"permission_mode_default":"default"}`, "permission_mode_default", func(c *ResolvedConfig) bool { return c.PermissionModeDefault() == "plan" }},
		{"null mode", `{"permission_mode_default":"plan"}`, `{"permission_mode_default":null}`, "permission_mode_default", func(c *ResolvedConfig) bool { return c.PermissionModeDefault() == "plan" }},
		{"plan off", `{"plan":true}`, `{"plan":false}`, "plan", (*ResolvedConfig).Plan},
		{"null plan", `{"plan":true}`, `{"plan":null}`, "plan", (*ResolvedConfig).Plan},
		{"plan off in a profile", `{"plan":true}`, `{"profiles":{"review":{"plan":false}}}`, "plan", (*ResolvedConfig).Plan},
		{"null profile holding plan", `{"profiles":{"review":{"plan":true}}}`, `{"profiles":{"review":null}}`, "plan", (*ResolvedConfig).Plan},
	}
	for _, test := range looser {
		t.Run("untrusted/"+test.name, func(t *testing.T) {
			w := newLayerWorkspace(t)
			if strings.Contains(test.user+test.project, profilesKey) {
				w.flags.Profile = StringInput{Value: "review", Set: true}
			}
			w.write(w.user, test.user)
			w.write(w.projectPath(), test.project)
			resolved := w.mustResolve()
			if !test.check(resolved) {
				t.Fatalf("untrusted %s widened permissions", test.project)
			}
			if warnings := strings.Join(resolved.Warnings(), "\n"); !strings.Contains(warnings, test.key) || !strings.Contains(warnings, w.projectPath()) {
				t.Fatalf("warnings %q do not name %s", warnings, test.key)
			}
		})
	}

	t.Run("untrusted project narrows the mode and plan", func(t *testing.T) {
		w := newLayerWorkspace(t)
		w.write(w.user, `{"permission_mode_default":"bypass"}`)
		w.write(w.projectPath(), `{"permission_mode_default":"plan","plan":true}`)
		resolved := w.mustResolve()
		if resolved.PermissionModeDefault() != "plan" || !resolved.Plan() || len(resolved.Warnings()) != 0 {
			t.Fatalf("mode=%q plan=%t warnings=%q", resolved.PermissionModeDefault(), resolved.Plan(), resolved.Warnings())
		}
	})

	t.Run("trusted project replaces", func(t *testing.T) {
		w := newLayerWorkspace(t)
		w.flags.TrustProjectHooks = BoolInput{Value: true, Set: true}
		w.write(w.user, `{"permission_deny_tools":`+userDeny+`,"permission_mode_default":"plan","plan":true}`)
		w.write(w.projectPath(), `{"permission_deny_tools":["edit"],"permission_allow_tools":["bash"],"permission_mode_default":"bypass","plan":false}`)
		resolved := w.mustResolve()
		if !slices.Equal(resolved.PermissionDenyTools(), []string{"edit"}) || !slices.Equal(resolved.PermissionAllowTools(), []string{"bash"}) || resolved.PermissionModeDefault() != "bypass" || resolved.Plan() {
			t.Fatalf("deny=%v allow=%v mode=%q plan=%t", resolved.PermissionDenyTools(), resolved.PermissionAllowTools(), resolved.PermissionModeDefault(), resolved.Plan())
		}
		if shown := settingFor(t, resolved, "permission_deny_tools"); shown.Layer != LayerProject || string(shown.Value) != `["edit"]` {
			t.Fatalf("config show = %+v", shown)
		}
		w.write(w.projectPath(), `{"permission_deny_tools":null}`)
		if deny := w.mustResolve().PermissionDenyTools(); len(deny) != 0 {
			t.Fatalf("trusted null left deny tools %v", deny)
		}
	})
}
