package prompt

import (
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/skills"
)

func TestBuildSystemPromptDefault(t *testing.T) {
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	got := BuildSystemPrompt(BuildSystemPromptOptions{
		Cwd:           "/home/user/project",
		SelectedTools: []string{"read", "bash", "edit", "write"},
		ToolSnippets: map[string]string{
			"read":  "Read file contents",
			"bash":  "Execute bash commands",
			"edit":  "Make precise edits",
			"write": "Write file contents",
		},
		PromptGuidelines: []string{"Test guideline"},
		Now:              now,
	})
	checks := []string{
		"helpful coding assistant operating in Harness",
		"<available_tools>",
		"- read: Read file contents",
		"- bash: Execute bash commands",
		"# Session Guidelines",
		"- Test guideline",
		"Be concise in your responses",
		"Show file paths clearly",
		"Current date: 2026-07-07",
		"Current working directory: /home/user/project",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q:\n%s", want, got)
		}
	}
}

func TestBuildSystemPromptProjectsOnlyActiveCapabilities(t *testing.T) {
	got := BuildSystemPrompt(BuildSystemPromptOptions{
		Cwd:           "/workspace/project",
		SelectedTools: []string{"read", "grep"},
		ToolSnippets: map[string]string{
			"read": "Read file contents",
			"grep": "Search file contents",
		},
		Skills: []skills.Skill{{
			Name:        "review",
			Description: "Review a change",
			FilePath:    "/workspace/skills/review/SKILL.md",
		}},
		Now: time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC),
	})
	for _, want := range []string{
		"- read: Read file contents",
		"- grep: Search file contents",
		"<available_skills>",
		"<name>review</name>",
		"<location>skill://review</location>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing active capability %q:\n%s", want, got)
		}
	}
	for _, absent := range []string{"vault://", "mcp://", "xd://", "- bash:", "- write:"} {
		if strings.Contains(got, absent) {
			t.Fatalf("prompt named unavailable capability %q:\n%s", absent, got)
		}
	}
	brandFragment := "p" + "i"
	for _, forbidden := range []string{brandFragment, "oh-my-" + brandFragment, "oh my " + brandFragment} {
		if strings.Contains(strings.ToLower(got), forbidden) {
			t.Fatalf("prompt contains forbidden vendor token %q:\n%s", forbidden, got)
		}
	}
}

func TestBuildSystemPromptCustomReplacesTemplate(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := BuildSystemPrompt(BuildSystemPromptOptions{
		CustomPrompt:  "Custom prompt text",
		SelectedTools: []string{"read"},
		Skills: []skills.Skill{
			{Name: "test-skill", Description: "A test skill", FilePath: "/tmp/skill.md"},
		},
		AppendSystemPrompt: "Appended section",
		ContextFiles: []ContextFile{
			{Path: "AGENTS.md", Content: "Instructions here"},
		},
		Cwd: "/tmp",
		Now: now,
	})
	checks := []string{
		"Custom prompt text",
		"Appended section",
		"<project_context>",
		`<project_instructions path="AGENTS.md">`,
		"Instructions here",
		"<available_skills>",
		"<name>test-skill</name>",
		"Current date: 2026-01-01",
		"Current working directory: /tmp",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Fatalf("custom prompt missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<system-conventions>") || strings.Contains(got, "# Engineering Principles") {
		t.Fatalf("custom prompt must replace the default template:\n%s", got)
	}
}

func TestBuildSystemPromptSkillsOnlyWithRead(t *testing.T) {
	now := time.Now()
	withRead := BuildSystemPrompt(BuildSystemPromptOptions{
		Cwd:           "/tmp",
		SelectedTools: []string{"read", "bash"},
		ToolSnippets:  map[string]string{"read": "Read files", "bash": "Execute commands"},
		Skills:        []skills.Skill{{Name: "visible", Description: "Shown", FilePath: "/tmp/s.md"}},
		Now:           now,
	})
	if !strings.Contains(withRead, "<available_skills>") {
		t.Fatal("skills should appear when read tool selected")
	}
	withoutRead := BuildSystemPrompt(BuildSystemPromptOptions{
		Cwd:           "/tmp",
		SelectedTools: []string{"bash"},
		ToolSnippets:  map[string]string{"bash": "Execute commands"},
		Skills:        []skills.Skill{{Name: "hidden", Description: "Not shown", FilePath: "/tmp/s.md"}},
		Now:           now,
	})
	if strings.Contains(withoutRead, "<available_skills>") {
		t.Fatal("skills should not appear when read tool not selected")
	}
}

func TestBuildSystemPromptToolOrderPreserved(t *testing.T) {
	got := BuildSystemPrompt(BuildSystemPromptOptions{
		Cwd:           "/tmp",
		SelectedTools: []string{"bash", "read"},
		ToolSnippets:  map[string]string{"bash": "Exec", "read": "Read"},
		Now:           time.Now(),
	})
	bashIdx := strings.Index(got, "- bash: Exec")
	readIdx := strings.Index(got, "- read: Read")
	if bashIdx < 0 || readIdx < 0 || bashIdx > readIdx {
		t.Fatal("tool order should be preserved as given")
	}
}

func TestBuildSystemPromptCustomIncludesCapabilities(t *testing.T) {
	got := BuildSystemPrompt(BuildSystemPromptOptions{
		CustomPrompt:  "Orchestrator instructions.",
		SelectedTools: []string{"read", "todo_write", "task"},
		ToolSnippets: map[string]string{
			"read":       "Read file contents",
			"todo_write": "Update the session todo list and progress checkpoint",
			"task":       "Delegate a focused task to a child agent",
		},
		Cwd:          "/workspace/project",
		ProgressPath: "/sessions/s.jsonl.d/progress.md",
		Now:          time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC),
	})
	for _, want := range []string{
		"Orchestrator instructions.",
		"<available_tools>",
		"- read: Read file contents",
		"- task: Delegate a focused task to a child agent",
		"<runtime>",
		"Full output:",
		"/sessions/s.jsonl.d/progress.md",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q:\n%s", want, got)
		}
	}
}

func TestCapabilitiesSectionOmitsUnavailableRuntimeNotes(t *testing.T) {
	got := CapabilitiesSection([]string{"write"}, map[string]string{"write": "Create or overwrite files"}, "/sessions/s.jsonl.d/progress.md")
	if !strings.Contains(got, "<available_tools>") || !strings.Contains(got, "- write: Create or overwrite files") {
		t.Fatalf("missing tool list: %s", got)
	}
	if !strings.Contains(got, "<runtime>") {
		t.Fatalf("missing runtime block: %s", got)
	}
	if !strings.Contains(got, "isolated, ephemeral sandbox") {
		t.Fatalf("missing sandbox awareness line: %s", got)
	}
	if !strings.Contains(got, "Network access exists") || !strings.Contains(got, "can be flaky") {
		t.Fatalf("missing corrected network semantics: %s", got)
	}
	if strings.Contains(got, "no external network") {
		t.Fatalf("false 'no external network' claim must not reappear: %s", got)
	}
	if strings.Contains(got, "Full output:") {
		t.Fatalf("spill note should be absent without read: %s", got)
	}
	if strings.Contains(got, "progress.md") {
		t.Fatalf("progress note should be absent without todo_write: %s", got)
	}
	if got := CapabilitiesSection([]string{"todo_write"}, map[string]string{"todo_write": "Update todos"}, ""); strings.Contains(got, "progress checkpoint") {
		t.Fatalf("progress note needs a checkpoint path: %s", got)
	}
	if CapabilitiesSection(nil, nil, "") != "" {
		t.Fatal("empty tool set should yield empty section")
	}
}
