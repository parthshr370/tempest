package session

import (
	"context"
	"go.harness.dev/harness/internal/compaction"
	"go.harness.dev/harness/internal/config"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/prompt"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/skills"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestBuildAgentStackUsesGenericCodingTools(t *testing.T) {
	provider := faux.New(faux.Options{})
	stack, err := BuildAgentStack(StackConfig{Cwd: t.TempDir(), Model: provider.Model(), StreamFn: provider.StreamSimple})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, tool := range stack.Agent.State().Tools {
		seen[tool.Name] = true
	}
	for _, want := range []string{"read", "bash", "edit", "write"} {
		if !seen[want] {
			t.Fatalf("generic stack missing %q", want)
		}
	}
}

func TestBuildPlanStackReadOnly(t *testing.T) {
	provider := faux.New(faux.Options{})
	stack, err := BuildAgentStack(StackConfig{Cwd: t.TempDir(), Model: provider.Model(), StreamFn: provider.StreamSimple, EnableWeb: true, Plan: true})
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"read": true, "grep": true, "find": true, "ls": true, "web_search": true, "web_fetch": true}
	for _, tool := range stack.Agent.State().Tools {
		if !allowed[tool.Name] {
			t.Fatalf("plan stack exposed non-read-only tool %q", tool.Name)
		}
	}
}

func TestStackConfigFromConfigPreservesReadOnlyPlan(t *testing.T) {
	resolved, err := (config.Config{
		SettingsPath: filepath.Join(t.TempDir(), "settings.json"),
		LookupEnv:    func(string) string { return "" },
		Flags: config.FlagValues{
			Plan:       config.BoolInput{Value: true, Set: true},
			EnableTask: config.BoolInput{Value: true, Set: true},
		},
	}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	cfg := StackConfigFromConfig(resolved)
	provider := faux.New(faux.Options{})
	cfg.Cwd, cfg.SpillDir = t.TempDir(), t.TempDir()
	cfg.Model, cfg.StreamFn = provider.Model(), provider.StreamSimple
	stack, err := BuildAgentStack(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range stack.Agent.State().Tools {
		names = append(names, tool.Name)
	}
	if got := strings.Join(names, ","); got != "read,grep,find,ls" {
		t.Fatalf("config-backed plan tools = %q, want read,grep,find,ls", got)
	}
}

func TestBuildAndPlanStacksProjectTheirActiveToolSets(t *testing.T) {
	provider := faux.New(faux.Options{})
	buildStack, err := BuildAgentStack(StackConfig{Cwd: t.TempDir(), Model: provider.Model(), StreamFn: provider.StreamSimple})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buildStack.Agent.State().SystemPrompt, "- write: Create or overwrite files") {
		t.Fatalf("build prompt did not project active write tool:\n%s", buildStack.Agent.State().SystemPrompt)
	}

	planStack, err := BuildAgentStack(StackConfig{Cwd: t.TempDir(), Model: provider.Model(), StreamFn: provider.StreamSimple, Plan: true})
	if err != nil {
		t.Fatal(err)
	}
	planPrompt := planStack.Agent.State().SystemPrompt
	if !strings.Contains(planPrompt, "- read: Read file contents") {
		t.Fatalf("plan prompt did not project active read tool:\n%s", planPrompt)
	}
	if strings.Contains(planPrompt, "- write:") || strings.Contains(planPrompt, "- edit:") {
		t.Fatalf("plan prompt named a tool outside its active selection:\n%s", planPrompt)
	}
}

func TestBuildAgentStackProjectsProductName(t *testing.T) {
	provider := faux.New(faux.Options{})
	stack, err := BuildAgentStack(StackConfig{
		Cwd:         t.TempDir(),
		Model:       provider.Model(),
		StreamFn:    provider.StreamSimple,
		ProductName: "Control Plane",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stack.Agent.State().SystemPrompt, "operating in Control Plane.") {
		t.Fatalf("stack prompt did not project product name:\n%s", stack.Agent.State().SystemPrompt)
	}
}

func TestBuildAgentStackProjectsPromptOptions(t *testing.T) {
	dir := t.TempDir()
	provider := faux.New(faux.Options{})
	stack, err := BuildAgentStack(StackConfig{
		Cwd:              dir,
		Model:            provider.Model(),
		StreamFn:         provider.StreamSimple,
		PromptGuidelines: []string{"Use the project policy."},
		ContextFiles:     []prompt.ContextFile{{Path: "AGENTS.md", Content: "Project instructions."}},
		Skills: []skills.Skill{{
			Name:        "review",
			Description: "Review changes.",
			FilePath:    filepath.Join(dir, "skills", "review", "SKILL.md"),
			BaseDir:     filepath.Join(dir, "skills", "review"),
		}},
		Rules:        []prompt.PromptRule{{Name: "backend", Description: "Keep handlers small.", Globs: []string{"*.go"}}},
		GenericRules: []string{"Use checked inputs."},
		Personality:  "Direct and careful.",
	})
	if err != nil {
		t.Fatal(err)
	}
	rendered := stack.Agent.State().SystemPrompt
	for _, want := range []string{
		"Use the project policy.",
		"Project instructions.",
		"<name>review</name>",
		"backend (*.go): Keep handlers small.",
		"Use checked inputs.",
		"Direct and careful.",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("stack prompt missing %q:\n%s", want, rendered)
		}
	}
}

func TestGenericRulesReachEveryProviderContextAcrossCompaction(t *testing.T) {
	provider := faux.New(faux.Options{})
	model := provider.Model()
	model.ContextWindow = 100
	var mu sync.Mutex
	var ordinary []ptypes.Context
	sawSummary := false
	step := func(c ptypes.Context, _ *ptypes.StreamOptions, _ faux.State, _ ptypes.Model) (ptypes.AssistantMessage, error) {
		if c.SystemPrompt == compaction.SummarizationSystemPrompt {
			mu.Lock()
			sawSummary = true
			mu.Unlock()
			return ptypes.AssistantMessage{Content: []ptypes.ContentBlock{ptypes.NewText("summary")}}, nil
		}
		mu.Lock()
		ordinary = append(ordinary, c)
		mu.Unlock()
		return ptypes.AssistantMessage{Content: []ptypes.ContentBlock{ptypes.NewText(strings.Repeat("response ", 300))}}, nil
	}
	steps := make([]faux.ResponseStep, 16)
	for index := range steps {
		steps[index] = step
	}
	provider.SetResponses(steps...)
	stack, err := BuildAgentStack(StackConfig{
		Cwd:          t.TempDir(),
		Model:        model,
		StreamFn:     provider.StreamSimple,
		GenericRules: []string{"user rule body", "project rule body"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{strings.Repeat("first ", 50_000), strings.Repeat("second ", 50_000)} {
		if _, err := stack.Agent.PromptText(context.Background(), text); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !sawSummary {
		t.Fatal("provider did not receive a compaction request")
	}
	if len(ordinary) < 2 {
		t.Fatalf("ordinary provider contexts = %d, want at least 2", len(ordinary))
	}
	for index, got := range ordinary {
		for _, rule := range []string{"user rule body", "project rule body"} {
			if !strings.Contains(got.SystemPrompt, rule) {
				t.Fatalf("ordinary provider context %d missing %q", index, rule)
			}
		}
	}
}

func TestBuildAgentStackPlanModelOverride(t *testing.T) {
	provider := faux.New(faux.Options{})
	stack, err := BuildAgentStack(StackConfig{Cwd: t.TempDir(), Model: ptypes.Model{ID: "build-model"}, PlanModel: ptypes.Model{ID: "plan-model"}, StreamFn: provider.StreamSimple, Plan: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := stack.Model.ID; got != "plan-model" {
		t.Fatalf("plan stack model = %q, want %q", got, "plan-model")
	}
}
