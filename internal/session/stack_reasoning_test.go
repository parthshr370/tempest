package session

import (
	"slices"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/provider/faux"
)

func TestBuildAgentStackAppliesRoleReasoning(t *testing.T) {
	provider := faux.New(faux.Options{})
	for _, test := range []struct {
		name string
		cfg  StackConfig
		want agent.ThinkingLevel
	}{
		{name: "unset", cfg: StackConfig{}, want: agent.ThinkingOff},
		{name: "default role", cfg: StackConfig{Reasoning: "low"}, want: agent.ThinkingLow},
		{name: "off", cfg: StackConfig{Reasoning: "off"}, want: agent.ThinkingOff},
		{name: "plan override", cfg: StackConfig{Plan: true, Reasoning: "low", PlanReasoning: "high"}, want: agent.ThinkingHigh},
		{name: "plan inherits", cfg: StackConfig{Plan: true, Reasoning: "medium"}, want: agent.ThinkingMedium},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.cfg.Cwd = t.TempDir()
			test.cfg.Model = provider.Model()
			test.cfg.StreamFn = provider.StreamSimple
			stack, err := BuildAgentStack(test.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got := stack.Agent.State().ThinkingLevel; got != test.want {
				t.Fatalf("thinking level = %q, want %q", got, test.want)
			}
		})
	}
}

// view_image is a read-tier tool: it is in the full stack and the plan stack.
func TestViewImageInDefaultAndPlanStacks(t *testing.T) {
	provider := faux.New(faux.Options{})
	for _, plan := range []bool{false, true} {
		stack, err := BuildAgentStack(StackConfig{Cwd: t.TempDir(), Model: provider.Model(), StreamFn: provider.StreamSimple, Plan: plan})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(ToolNames(stack.Agent.State().Tools), "view_image") {
			t.Fatalf("plan=%v tools = %v", plan, ToolNames(stack.Agent.State().Tools))
		}
	}
}
