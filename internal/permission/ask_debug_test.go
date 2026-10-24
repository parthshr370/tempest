package permission

import (
	"encoding/json"
	"testing"
)

func TestAskDebugPermissionTiers(t *testing.T) {
	if ClassifyTool("ask") != TierRead || ClassifyTool("debug") != TierExec {
		t.Fatal("incorrect ask/debug tiers")
	}
	if outcome := Gate(t.Context(), ModePlan, "ask", json.RawMessage(`{}`), &Policy{}); !outcome.Allow {
		t.Fatalf("plan denied ask: %+v", outcome)
	}
	for _, action := range []string{"launch", "attach", "stack_trace", "evaluate", "terminate"} {
		if outcome := Gate(t.Context(), ModePlan, "debug", json.RawMessage(`{"action":"`+action+`"}`), &Policy{Overrides: map[string]Decision{"debug": DecideAllow}}); outcome.Allow {
			t.Fatalf("plan allowed debug %s", action)
		}
	}
}
