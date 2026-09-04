package subagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/agent"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/schema"
)

// streamingTool builds a no-op tool with the given name.
func namedTool(name string) agent.AgentTool {
	return agent.AgentTool{
		Tool:  ptypes.Tool{Name: name, Parameters: schema.Object(schema.JSON{}, "")},
		Label: name,
		Execute: func(ctx context.Context, _ string, _ json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
			return agent.AgentToolResult{}, nil
		},
	}
}

func TestRunnerStripsTaskFromChildTools(t *testing.T) {
	opts := Options{Tools: []agent.AgentTool{namedTool("read"), namedTool("task"), namedTool("bash")}}
	runner := NewRunner(opts)
	for _, tool := range runner.opts.Tools {
		if tool.Name == "task" {
			t.Fatal("child tool set still contains the task tool")
		}
	}
	if len(runner.opts.Tools) != 2 {
		t.Fatalf("child tool count = %d, want 2", len(runner.opts.Tools))
	}
}

func TestRunnerDefinitionModelAndStreamOverride(t *testing.T) {
	smol := ptypes.Model{Provider: "anthropic", ID: "smol-model"}
	definition := Definition{Tools: []agent.AgentTool{namedTool("read")}, Model: smol}
	opts := Options{Registry: Registry{"explore": definition}}
	runner := NewRunner(opts)
	if _, ok := runner.opts.Registry["explore"]; !ok {
		t.Fatal("registry lost the explore definition")
	}
	if runner.opts.Registry["explore"].Model.ID != "smol-model" {
		t.Fatalf("explore model = %q, want smol-model", runner.opts.Registry["explore"].Model.ID)
	}
}

func TestRegistryListingNamesBothBuiltins(t *testing.T) {
	listing := RegistryListing(Registry{
		"explore": {Description: "read-only investigation"},
		"general": {Description: "full coding"},
	})
	for _, want := range []string{"explore", "general"} {
		if !strings.Contains(listing, want) {
			t.Fatalf("registry listing missing %q:\n%s", want, listing)
		}
	}
}
