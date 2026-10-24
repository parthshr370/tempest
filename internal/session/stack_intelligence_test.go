package session

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/extensionhost"
	"go.harness.dev/harness/internal/lsp"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/tools"
	"go.harness.dev/harness/tempest/extension"
)

func inertManager(t *testing.T, cwd string) *lsp.Manager {
	t.Helper()
	workspace, err := tools.NewLSPWorkspace(cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := lsp.New(workspace, lsp.Config{Enabled: true}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(errors.Join(err, workspace.Close()))
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	return manager
}

func stackToolNames(t *testing.T, cfg StackConfig) []string {
	t.Helper()
	provider := faux.New(faux.Options{})
	cfg.Model, cfg.StreamFn = provider.Model(), provider.StreamSimple
	stack, err := BuildAgentStack(cfg)
	if err != nil {
		t.Fatal(err)
	}
	names := ToolNames(stack.Agent.State().Tools)
	slices.Sort(names)
	return names
}

func TestPlanStackSelectsIntelligenceReadTools(t *testing.T) {
	cwd := t.TempDir()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ast-grep"), []byte("#!/bin/sh\nprintf '[]'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	names := stackToolNames(t, StackConfig{Cwd: cwd, Plan: true, LSPManager: inertManager(t, cwd)})
	want := []string{"ast_grep", "find", "gh", "grep", "ls", "lsp_definition", "lsp_diagnostics", "lsp_references", "lsp_rename", "manifest_diff", "read", "view_image"}
	if !slices.Equal(names, want) {
		t.Fatalf("plan tools = %v, want %v", names, want)
	}
	// Without a manager the lsp_* names stay out of every stack.
	names = stackToolNames(t, StackConfig{Cwd: cwd, Plan: true})
	if slices.Contains(names, "lsp_definition") || !slices.Contains(names, "manifest_diff") {
		t.Fatalf("plan tools without LSP = %v", names)
	}
	if slices.Contains(names, "ast_edit") || slices.Contains(names, "gh_checkout") {
		t.Fatalf("plan stack exposes mutation tools: %v", names)
	}
}

func TestChildStackNeverGetsLanguageServerTools(t *testing.T) {
	cwd := t.TempDir()
	names := stackToolNames(t, StackConfig{Cwd: cwd, Child: true, LSPManager: inertManager(t, cwd), AllowedTools: []string{"read", "edit", "lsp_definition", "lsp_rename", "gh_checkout"}})
	if !slices.Equal(names, []string{"edit", "read"}) {
		t.Fatalf("child tools = %v, want read and edit only", names)
	}
}

func TestStackConfigFromConfigCarriesIntelligenceSettings(t *testing.T) {
	dir := t.TempDir()
	policy := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"rules":[{"tool":"kubectl","context":"kind-dev","action":"allow_write"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := (config.Config{
		SettingsPath: filepath.Join(dir, "settings.json"),
		LookupEnv:    func(string) string { return "" },
		Flags:        config.FlagValues{EnableLSP: config.BoolInput{Value: true, Set: true}, CloudPolicy: config.StringInput{Value: policy, Set: true}},
	}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	cfg := StackConfigFromConfig(resolved)
	if cfg.LSP == nil || !cfg.LSP.Enabled || len(cfg.LSP.Servers) != 4 {
		t.Fatalf("LSP config = %#v", cfg.LSP)
	}
	if rules := cfg.PermissionPolicy.CloudRules; len(rules) != 1 || rules[0].Context != "kind-dev" {
		t.Fatalf("cloud rules = %#v", rules)
	}
	if cfg.LSPManager != nil {
		t.Fatal("configuration must never construct a manager")
	}
}

func TestRootEmptyAllowlistAndPlanCeilingRemainRestricted(t *testing.T) {
	for _, plan := range []bool{false, true} {
		runtime := &extensionhost.Runtime{}
		names := stackToolNames(t, StackConfig{Cwd: t.TempDir(), Plan: plan, AllowedTools: []string{}, Extensions: runtime})
		if len(names) != 0 {
			t.Fatalf("plan=%t empty allowlist activated %v", plan, names)
		}
		if runtime.AllowTool("ext__sample__tool", extension.EffectRead) {
			t.Fatalf("plan=%t empty allowlist admitted an extension tool", plan)
		}
	}
	names := stackToolNames(t, StackConfig{Cwd: t.TempDir(), Plan: true, AllowedTools: []string{"read", "bash"}})
	if !slices.Equal(names, []string{"read"}) {
		t.Fatalf("plan ignored operator tool ceiling: %v", names)
	}
}
