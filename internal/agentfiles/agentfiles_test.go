package agentfiles

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeAgent(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func agentFile(name, description, extra string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n" + extra + "---\nInstructions for " + name + ".\n"
}

func TestLoadPrecedenceAcrossThreeDirectories(t *testing.T) {
	project, agentDir := t.TempDir(), t.TempDir()
	native := writeAgent(t, filepath.Join(project, ".harness", "agents", "reviewer.md"), agentFile("reviewer", "Native reviewer", ""))
	claudeShadowed := writeAgent(t, filepath.Join(project, ".claude", "agents", "reviewer.md"), agentFile("reviewer", "Claude reviewer", ""))
	claudeOnly := writeAgent(t, filepath.Join(project, ".claude", "agents", "tester.md"), agentFile("tester", "Claude tester", ""))
	userShadowed := writeAgent(t, filepath.Join(agentDir, "agents", "tester.md"), agentFile("tester", "User tester", ""))
	userOnly := writeAgent(t, filepath.Join(agentDir, "agents", "writer.md"), agentFile("writer", "User writer", ""))
	writeAgent(t, filepath.Join(agentDir, "agents", "notes.txt"), "not an agent")

	agents, diagnostics, err := Load(Options{ProjectDir: project, AgentDir: agentDir})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Agent{}
	var order []string
	for _, agent := range agents {
		got[agent.Preset.ID] = agent
		order = append(order, agent.Preset.ID)
	}
	if !slices.Equal(order, []string{"reviewer", "tester", "writer"}) {
		t.Fatalf("agents %v, want reviewer, tester, writer", order)
	}
	for name, want := range map[string]struct{ source, description string }{
		"reviewer": {native, "Native reviewer"},
		"tester":   {claudeOnly, "Claude tester"},
		"writer":   {userOnly, "User writer"},
	} {
		if got[name].Source != want.source || got[name].Preset.Description != want.description {
			t.Errorf("%s from %s (%q), want %s (%q)", name, got[name].Source, got[name].Preset.Description, want.source, want.description)
		}
	}
	if len(diagnostics) != 2 {
		t.Fatalf("diagnostics %+v, want two shadowed files", diagnostics)
	}
	for i, shadow := range []struct{ path, winner string }{{claudeShadowed, native}, {userShadowed, claudeOnly}} {
		d := diagnostics[i]
		if d.Path != shadow.path || d.Err != nil || !strings.Contains(d.Message, shadow.winner) {
			t.Errorf("diagnostic %d: %+v, want %s shadowed by %s", i, d, shadow.path, shadow.winner)
		}
	}
}

func TestInvalidFileNamesFileAndField(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		file, content, field string
	}{
		{"no-header.md", "just instructions\n", "frontmatter"},
		{"no-name.md", "---\ndescription: d\n---\nbody\n", "name"},
		{"reserved.md", agentFile("explore", "d", ""), "name"},
		{"no-description.md", "---\nname: lonely\n---\nbody\n", "description"},
		{"bad-tool.md", agentFile("bad-tool", "d", "tools: read, teleport\n"), "tools"},
		{"bad-model.md", agentFile("bad-model", "d", "model: openai:gpt-5@https://evil.example/v1\n"), "model"},
		{"bad-reasoning.md", agentFile("bad-reasoning", "d", "reasoning: extreme\n"), "reasoning"},
		{"bad-schema.md", agentFile("bad-schema", "d", "output_schema: '{\"type\": 7}'\n"), "output_schema"},
		{"empty-body.md", "---\nname: empty\ndescription: d\n---\n\n", "body"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			path := writeAgent(t, filepath.Join(dir, tc.file), tc.content)
			_, err := Parse(path, nil)
			var fieldErr *FieldError
			if !errors.As(err, &fieldErr) {
				t.Fatalf("Parse error %v, want a FieldError", err)
			}
			if fieldErr.Path != path || fieldErr.Field != tc.field {
				t.Fatalf("error names %s field %s, want %s field %s", fieldErr.Path, fieldErr.Field, path, tc.field)
			}
			if msg := err.Error(); !strings.Contains(msg, path) || !strings.Contains(msg, tc.field) {
				t.Fatalf("message %q lacks the file or field", msg)
			}
		})
	}
	// Load keeps going past an invalid file and reports it with its error.
	project := t.TempDir()
	bad := writeAgent(t, filepath.Join(project, ".harness", "agents", "bad.md"), agentFile("bad", "d", "reasoning: extreme\n"))
	writeAgent(t, filepath.Join(project, ".harness", "agents", "good.md"), agentFile("good", "d", ""))
	agents, diagnostics, err := Load(Options{ProjectDir: project})
	if err != nil || len(agents) != 1 || agents[0].Preset.ID != "good" {
		t.Fatalf("agents %+v err %v", agents, err)
	}
	if len(diagnostics) != 1 || diagnostics[0].Path != bad || diagnostics[0].Err == nil || !strings.Contains(diagnostics[0].Message, "reasoning") {
		t.Fatalf("diagnostics %+v", diagnostics)
	}
}

func TestModelAliasesAndClaudeToolNames(t *testing.T) {
	dir := t.TempDir()
	for raw, want := range map[string]string{
		"":                            "",
		"default":                     RoleDefault,
		"plan":                        RolePlan,
		"smol":                        RoleSmol,
		"haiku":                       RoleSmol,
		"sonnet":                      RoleDefault,
		"opus":                        RoleDefault,
		"inherit":                     RoleDefault,
		"openai-responses:gpt-6-luna": "openai-responses:gpt-6-luna",
	} {
		extra := ""
		if raw != "" {
			extra = "model: " + raw + "\n"
		}
		path := writeAgent(t, filepath.Join(dir, "model.md"), agentFile("model-agent", "d", extra))
		agent, err := Parse(path, nil)
		if err != nil {
			t.Fatalf("model %q: %v", raw, err)
		}
		if agent.Preset.Model != want {
			t.Errorf("model %q resolved to %q, want %q", raw, agent.Preset.Model, want)
		}
	}
	path := writeAgent(t, filepath.Join(dir, "claude.md"), agentFile("claude-agent", "d", "tools: Read, Grep, Glob, Read\nreasoning: HIGH\noutput_schema:\n  type: object\n  properties:\n    verdict: {type: string}\n"))
	agent, err := Parse(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(agent.Preset.AllowedTools, []string{"read", "grep", "find"}) {
		t.Errorf("tools %v, want [read grep find]", agent.Preset.AllowedTools)
	}
	if agent.Preset.Reasoning != "high" || !strings.Contains(string(agent.Preset.OutputSchema), `"verdict"`) {
		t.Errorf("preset %+v", agent.Preset)
	}
	empty := writeAgent(t, filepath.Join(dir, "none.md"), agentFile("no-tools", "d", "tools: []\n"))
	if agent, err := Parse(empty, nil); err != nil || agent.Preset.AllowedTools == nil || len(agent.Preset.AllowedTools) != 0 {
		t.Fatalf("an empty list must give no tools, not the default set: %+v %v", agent.Preset.AllowedTools, err)
	}
}
