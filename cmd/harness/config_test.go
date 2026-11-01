package main

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
		// The -json output is indented, which re-indents nested array and object values.
		var compact bytes.Buffer
		if ok {
			if err := json.Compact(&compact, shown.Value); err != nil {
				t.Fatalf("%s: %v", setting.Key, err)
			}
		}
		if !ok || !bytes.Equal(compact.Bytes(), setting.Value) || shown.Layer != setting.Layer || shown.Source != setting.Source {
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

// An untrusted repository's deny list adds to the user's, and config show
// prints the combined list from the project file that added the entry.
func TestConfigShowAttributesUntrustedDenyUnion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	userPath := filepath.Join(home, ".config", config.ConfigDirName, "settings.json")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(root, config.ProjectConfigDirName, "settings.json")
	for path, content := range map[string]string{
		userPath:    `{"permission_deny_tools":["bash","web_fetch"]}`,
		projectPath: `{"permission_deny_tools":["edit"]}`,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)

	output, code := runConfigShow(t, "-key", "permission_deny_tools")
	if code != 0 {
		t.Fatalf("config show exit = %d", code)
	}
	shown := output.Settings["permission_deny_tools"]
	var deny []string
	if err := json.Unmarshal(shown.Value, &deny); err != nil || !slices.Equal(deny, []string{"bash", "web_fetch", "edit"}) || shown.Layer != config.LayerProject || shown.Source != projectPath {
		t.Fatalf("permission_deny_tools = %+v", shown)
	}

	if err := os.WriteFile(projectPath, []byte(`{"permission_deny_tools":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	output, code = runConfigShow(t, "-key", "permission_deny_tools")
	shown = output.Settings["permission_deny_tools"]
	if err := json.Unmarshal(shown.Value, &deny); code != 0 || err != nil || !slices.Equal(deny, []string{"bash", "web_fetch"}) || shown.Layer != config.LayerUser || shown.Source != userPath {
		t.Fatalf("after a project null: exit=%d shown=%+v", code, shown)
	}
	if warnings := strings.Join(output.Warnings, "\n"); !strings.Contains(warnings, "permission_deny_tools") || !strings.Contains(warnings, projectPath) {
		t.Fatalf("warnings %q do not name the dropped null", warnings)
	}
}

// The MCP client commands resolve the same server list as a run with the
// same flags: trust, -profile and -c apply, and the trust warning names a
// project server they drop.
func TestMCPClientCommandsSeeTheRunsSettingsLayers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	userPath := filepath.Join(home, ".config", config.ConfigDirName, "settings.json")
	projectPath := filepath.Join(root, config.ProjectConfigDirName, "settings.json")
	for path, content := range map[string]string{
		userPath:    `{"profiles":{"review":{"mcp_servers":[{"name":"profiled","transport":"stdio","command":"profiled-mcp"}]}}}`,
		projectPath: `{"mcp_servers":[{"name":"repo","transport":"stdio","command":"repo-mcp"}]}`,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)
	list := func(args ...string) (string, string, int) {
		var stdout, stderr bytes.Buffer
		code := run(append([]string{"mcp", "list"}, args...), &stdout, &stderr)
		return stdout.String(), stderr.String(), code
	}

	stdout, stderr, code := list()
	if code != 0 || strings.Contains(stdout, "repo") || !strings.Contains(stderr, "mcp_servers") || !strings.Contains(stderr, projectPath) {
		t.Fatalf("untrusted: exit=%d stdout=%q stderr=%q, want the project server dropped with a warning", code, stdout, stderr)
	}
	if stdout, stderr, code = list("-trust-project-hooks"); code != 0 || !strings.Contains(stdout, "repo\tstdio repo-mcp") {
		t.Fatalf("trusted: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout, stderr, code = list("-profile", "review"); code != 0 || !strings.Contains(stdout, "profiled\tstdio profiled-mcp") {
		t.Fatalf("profile: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout, stderr, code = list("-c", `mcp_servers=[{"name":"inline","transport":"stdio","command":"inline-mcp"}]`); code != 0 || !strings.Contains(stdout, "inline\tstdio inline-mcp") {
		t.Fatalf("-c: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

// -resume resolves the project settings of the session's workspace, where
// the resumed session runs, as the SDK does; the directory the command
// starts in contributes none.
func TestResumeResolvesTheSessionsWorkspaceSettings(t *testing.T) {
	for _, assignment := range os.Environ() {
		key, _, _ := strings.Cut(assignment, "=")
		if strings.HasPrefix(key, "HARNESS_") || strings.HasPrefix(key, "LYRA_") {
			t.Setenv(key, "")
		}
	}
	t.Setenv("HOME", t.TempDir())
	sessionDir := t.TempDir()
	script := filepath.Join(t.TempDir(), "provider.json")
	if err := os.WriteFile(script, []byte(`[{"text":"done"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, other := t.TempDir(), t.TempDir()
	for dir, content := range map[string]string{workspace: `{"permission_deny_tools":["bash"]}`, other: `{"permission_deny_tools":["edit"]}`} {
		path := filepath.Join(dir, config.ProjectConfigDirName, "settings.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(workspace)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-session-dir", sessionDir, "-faux-script", script, "-output-format", "json", "-p", "hi"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, &stderr)
	}
	var report runReportOutput
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || report.SessionID == "" {
		t.Fatalf("report %q: %v", stdout.String(), err)
	}

	t.Chdir(other)
	opts, err := parseCommandOptions("", []string{"-session-dir", sessionDir, "-resume", report.SessionID, "-p", "again"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolveCommandConfig(&opts); err != nil {
		t.Fatal(err)
	}
	if deny := opts.Config.PermissionDenyTools(); !slices.Equal(deny, []string{"bash"}) {
		t.Fatalf("resumed deny list = %v, want the session workspace's [bash]", deny)
	}
}
