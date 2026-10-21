package subagent

import (
	"context"
	"testing"

	"go.harness.dev/harness/internal/agent"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
)

// The task tool discards the child run's RunResult, so a child's spend never
// reaches the parent run: `-max-cost` with `-enable-task` does not bound the
// subagents, and the parent's usage/cost_usd under-reports the run. Here the
// parent model is free and the child costs $1 per token, so the child alone
// blows through the parent's $0.50 cap.
func TestChildSpendCountsAgainstParentCostBudget(t *testing.T) {
	child := faux.New(faux.Options{Models: []faux.ModelDef{{ID: "child-priced", Cost: ptypes.ModelCost{Input: 1_000_000, Output: 1_000_000}}}})
	child.SetResponses(faux.Respond(faux.Text("child done")))
	taskTool := NewTool(Options{Model: child.Model(), StreamFn: child.StreamSimple})

	parent := faux.New(faux.Options{Models: []faux.ModelDef{{ID: "parent-free"}}})
	parent.SetResponses(
		faux.Respond(faux.ToolCall("task", rawTask(t, map[string]any{"prompt": "spend", "subagent_type": "worker"}), "t1")),
		faux.Respond(faux.Text("parent done")),
	)
	runner := agent.NewAgent(agent.AgentOptions{
		InitialState: &agent.AgentState{Model: parent.Model(), Tools: []agent.AgentTool{taskTool}},
		StreamFn:     parent.StreamSimple,
		Budget:       agent.Budget{MaxCostUSD: 0.5},
	})

	result, err := runner.PromptText(context.Background(), "delegate")
	if err != nil {
		t.Fatal(err)
	}
	if child.CallCount() != 1 {
		t.Fatalf("child calls = %d, want 1", child.CallCount())
	}
	if result.Usage.Cost.Total <= 0 {
		t.Errorf("parent run cost = %+v, want the child's spend included", result.Usage.Cost)
	}
	if result.StopReason != agent.StopMaxCost {
		t.Errorf("parent stop = %s, want max_cost once the child's spend passes the cap", result.StopReason)
	}
}
