package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/tempest/extension"
)

func TestMCPToolsOnlyBundleNeverTrustsReadHints(t *testing.T) {
	var advertised toolDefinition
	if err := json.Unmarshal([]byte(`{"name":"read","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true,"idempotentHint":true}}`), &advertised); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{servers: []*server{{config: mcpconfig.Server{Name: "fixture"}, tools: []agent.AgentTool{{Tool: types.Tool{Name: "mcp__fixture__read", Parameters: advertised.InputSchema}}}}}}
	bundles := manager.Bundles()
	if len(bundles) != 1 || len(bundles[0].Tools) != 1 {
		t.Fatal(bundles)
	}
	bundle := bundles[0]
	if bundle.ID != "mcp_fixture" || len(bundle.Hooks) != 0 || len(bundle.Sections) != 0 || len(bundle.Tasks) != 0 || bundle.Tools[0].Replay != extension.ReplayNever || bundle.Tools[0].Effect != extension.EffectExecute {
		t.Fatalf("untrusted metadata escalated bundle: %+v", bundle)
	}
}

func TestProgressBudgetDoesNotFailSuccessfulMCPCall(t *testing.T) {
	tool := agent.AgentTool{Tool: types.Tool{Name: "mcp__fixture__read", Parameters: json.RawMessage(`{"type":"object"}`)}, Execute: func(_ context.Context, _ string, _ json.RawMessage, update agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		for range 3 {
			update(agent.AgentToolResult{Content: []types.ContentBlock{types.NewText(strings.Repeat("x", 600<<10))}})
		}
		return agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("completed")}}, nil
	}}
	manager := &Manager{servers: []*server{{config: mcpconfig.Server{Name: "fixture"}, tools: []agent.AgentTool{tool}}}}
	grant := extension.NewGrant(context.Background(), extension.GrantOptions{Output: func(context.Context, extension.OutputChunk) error {
		return errors.New("observer disconnected")
	}})
	defer grant.Revoke()
	result, err := manager.Bundles()[0].Tools[0].Execute(context.Background(), extension.Call{Arguments: json.RawMessage(`{}`)}, grant)
	if err != nil || result.Content[0].Text != "completed" || result.IsError {
		t.Fatalf("progress observer changed successful result: %+v %v", result, err)
	}
}
