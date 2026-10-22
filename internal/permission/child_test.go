package permission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestChildCeilingIntersectsLivePolicyWithoutGrants(t *testing.T) {
	policy := &Policy{Overrides: map[string]Decision{"bash": DecidePrompt}}
	ceiling := CaptureCeiling(ModeAcceptEdits, []string{"read", "write", "bash"}, policy)
	var requests []PromptRequest
	policy.Prompter = choicePrompter(func(_ context.Context, request PromptRequest) (Choice, error) {
		requests = append(requests, request)
		return AllowOnce, nil
	})
	args := json.RawMessage(`{"command":"printf hello"}`)
	scope := ChildScope{JobID: "job_first", SessionID: "child_first", RunID: "run_first", CallID: "message:0", ToolCallID: "call_1", Cwd: "/owned/tree"}
	outcome := GateChild(t.Context(), ceiling, ModeAcceptEdits, ceiling.Tools, policy, "bash", args, scope)
	if !outcome.Allow || len(requests) != 1 {
		t.Fatalf("outcome=%+v requests=%+v", outcome, requests)
	}
	if requests[0].ToolCallID != "child:child_first:run_first:message:0" || requests[0].ChildToolCallID != "call_1" || requests[0].Cwd != "/owned/tree" {
		t.Fatalf("request=%+v", requests[0])
	}
	policy.Overrides["bash"] = DecideAllow
	outcome = GateChild(t.Context(), ceiling, ModeBypass, ceiling.Tools, policy, "bash", args, scope)
	if !outcome.Allow || len(requests) != 2 {
		t.Fatal("loosening live policy escaped launch prompt ceiling")
	}
	policy.Overrides["bash"] = DecideDeny
	outcome = GateChild(t.Context(), ceiling, ModeBypass, ceiling.Tools, policy, "bash", args, scope)
	if outcome.Allow || outcome.Source != SourceInherited || len(requests) != 2 {
		t.Fatal("live exact deny wasn't inherited")
	}
	outcome = GateChild(t.Context(), ceiling, ModePlan, ceiling.Tools, policy, "write", json.RawMessage(`{"path":"x","content":"y"}`), scope)
	if outcome.Allow {
		t.Fatal("plan tightening allowed a child write")
	}
	outcome = GateChild(t.Context(), ceiling, ModeBypass, []string{"read"}, policy, "write", nil, scope)
	if outcome.Allow {
		t.Fatal("excluded live tool returned through child registry")
	}
}
func TestChildAsksAreScopedAndNeverRememberAlways(t *testing.T) {
	policy := &Policy{Overrides: map[string]Decision{"bash": DecidePrompt}}
	ceiling := CaptureCeiling(ModeDefault, []string{"bash"}, policy)
	var ids []string
	policy.Prompter = choicePrompter(func(_ context.Context, request PromptRequest) (Choice, error) {
		ids = append(ids, request.ToolCallID)
		return AllowAlways, nil
	})
	for _, child := range []string{"one", "two"} {
		outcome := GateChild(t.Context(), ceiling, ModeDefault, ceiling.Tools, policy, "bash", json.RawMessage(`{"command":"printf hello"}`), ChildScope{SessionID: child, RunID: "run", CallID: "m:0", ToolCallID: "call_1"})
		if outcome.Allow {
			t.Fatal("child accepted an always grant")
		}
	}
	if len(ids) != 2 || ids[0] == ids[1] || !strings.HasPrefix(ids[0], "child:") {
		t.Fatalf("scoped ids=%v", ids)
	}
	if len(policy.promptOverrides) != 0 {
		t.Fatal("child modified parent's interactive policy")
	}
	policy.Prompter = nil
	outcome := GateChild(t.Context(), ceiling, ModeDefault, ceiling.Tools, policy, "bash", json.RawMessage(`{"command":"printf hello"}`), ChildScope{})
	if outcome.Allow {
		t.Fatal("nil root prompter approved child")
	}
}
func TestChildDestructiveGuardAsksEveryCall(t *testing.T) {
	policy := &Policy{}
	ceiling := CaptureCeiling(ModeDefault, []string{"bash"}, policy)
	calls := 0
	policy.Prompter = choicePrompter(func(_ context.Context, request PromptRequest) (Choice, error) {
		calls++
		if request.Reason == "" {
			t.Error("guard ask lost reason")
		}
		return AllowOnce, nil
	})
	for range 2 {
		outcome := GateChild(t.Context(), ceiling, ModeDefault, ceiling.Tools, policy, "bash", json.RawMessage(`{"command":"rm -rf disposable"}`), ChildScope{})
		if !outcome.Allow {
			t.Fatalf("guard approval=%+v", outcome)
		}
	}
	if calls != 2 || len(policy.promptOverrides) != 0 {
		t.Fatalf("guard remembered child grant: calls=%d", calls)
	}
}
func TestChildLaunchExactDenyCannotBeLoosened(t *testing.T) {
	policy := &Policy{Overrides: map[string]Decision{"write": DecideDeny}}
	ceiling := CaptureCeiling(ModeDefault, []string{"write"}, policy)
	policy.Overrides["write"] = DecideAllow
	if GateChild(t.Context(), ceiling, ModeBypass, ceiling.Tools, policy, "write", nil, ChildScope{}).Allow {
		t.Fatal("live loosening escaped launch exact deny")
	}
}
func TestJobsPermissionDependsOnParsedAction(t *testing.T) {
	if !Gate(t.Context(), ModePlan, "jobs", json.RawMessage(`{"action":"list"}`), nil).Allow {
		t.Fatal("read-only jobs listing denied")
	}
	if Gate(t.Context(), ModePlan, "jobs", json.RawMessage(`{"action":"cleanup","job_id":"job_one"}`), nil).Allow {
		t.Fatal("cleanup classified read-only")
	}
}
