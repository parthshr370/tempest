package permission

import (
	"context"
	"encoding/json"
	"testing"
)

type mcpPrompter struct{ calls int }

func (p *mcpPrompter) Approve(context.Context, PromptRequest) (Choice, error) {
	p.calls++
	return AllowOnce, nil
}
func TestMCPPermission(t *testing.T) {
	name := "mcp__issues__read"
	for _, mode := range []Mode{ModeDefault, ModeAcceptEdits} {
		policy := &Policy{}
		if got := Gate(context.Background(), mode, name, json.RawMessage(`{}`), policy); got.Allow || got.Source != SourceNoPrompter {
			t.Fatalf("headless default: %#v", got)
		}
		prompt := &mcpPrompter{}
		policy.Prompter = prompt
		if got := Gate(context.Background(), mode, name, nil, policy); !got.Allow || prompt.calls != 1 || got.Source != SourcePrompt {
			t.Fatalf("prompt default: %#v", got)
		}
	}
	cases := []struct {
		name     string
		mode     Mode
		override map[string]Decision
		defaults map[string]Decision
		allow    bool
	}{
		{"exact", ModeDefault, map[string]Decision{name: DecideAllow}, nil, true},
		{"unrelated", ModeDefault, map[string]Decision{"mcp__issues__other": DecideAllow}, nil, false},
		{"trusted-server", ModeDefault, nil, map[string]Decision{name: DecideAllow}, true},
		{"server-deny", ModeDefault, nil, map[string]Decision{name: DecideDeny}, false},
		{"exact-deny", ModeBypass, map[string]Decision{name: DecideDeny}, map[string]Decision{name: DecideAllow}, false},
		{"plan", ModePlan, map[string]Decision{name: DecideAllow}, map[string]Decision{name: DecideAllow}, false},
		{"bypass", ModeBypass, nil, map[string]Decision{name: DecideDeny}, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := Gate(context.Background(), test.mode, name, nil, &Policy{Overrides: test.override, MCPDefaults: test.defaults})
			if got.Allow != test.allow {
				t.Fatalf("outcome %#v", got)
			}
		})
	}
	if got := Gate(context.Background(), ModeDefault, "embedder_tool", nil, &Policy{}); !got.Allow {
		t.Fatal("changed ordinary unknown-tool behavior")
	}
	if ClassifyTool(name) != TierExec {
		t.Fatal("MCP name was trusted as read-only")
	}
}
