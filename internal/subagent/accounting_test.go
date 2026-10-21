package subagent_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/agent"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/subagent"
)

func TestDelegatedUsageAndSharedCostAdmission(t *testing.T) {
	priced := ptypes.ModelCost{Input: 1_000_000, Output: 1_000_000}
	for _, test := range []struct {
		name                  string
		parentCost, childCost ptypes.ModelCost
		budget                float64
		parallel              bool
		wantChildCalls        int
		wantStop              agent.StopReason
	}{
		{name: "child cannot start another turn", childCost: priced, budget: .5, wantChildCalls: 1, wantStop: agent.StopMaxCost},
		{name: "parallel children share remaining funds", childCost: priced, budget: .5, parallel: true, wantChildCalls: 1, wantStop: agent.StopMaxCost},
		{name: "parent exhausted cap before delegation", parentCost: priced, childCost: priced, budget: .5, wantStop: agent.StopMaxCost},
		{name: "unpriced child refused under cost cap", childCost: ptypes.ModelCost{Unknown: true}, budget: .5, wantStop: agent.StopError},
		{name: "unpriced child stays unknown without cap", childCost: ptypes.ModelCost{Unknown: true}, wantChildCalls: 2, wantStop: agent.StopCompleted},
		{name: "parallel uncapped child usage is counted once", childCost: priced, parallel: true, wantChildCalls: 3, wantStop: agent.StopCompleted},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			var childUsage ptypes.Usage
			var usageMu sync.Mutex
			ctx = subagent.WithEventSink(ctx, func(_ context.Context, event agent.AgentEvent) error {
				if assistant, ok := event.Message.(ptypes.AssistantMessage); ok {
					usageMu.Lock()
					childUsage.Add(assistant.Usage)
					usageMu.Unlock()
				}
				return nil
			})
			child := faux.New(faux.Options{Models: []faux.ModelDef{{ID: "child", Cost: test.childCost}}})
			child.SetResponses(
				faux.Respond(faux.ToolCall("work", json.RawMessage(`{}`), "work-1")),
				faux.Respond(faux.Text("child done")),
				faux.Respond(faux.Text("child done")),
			)
			work := agent.AgentTool{
				Tool: ptypes.Tool{Name: "work", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)},
				Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
					return agent.AgentToolResult{Content: []ptypes.ContentBlock{ptypes.NewText("ok")}}, nil
				},
			}
			task := subagent.NewTool(subagent.Options{Model: child.Model(), StreamFn: child.StreamSimple, Tools: []agent.AgentTool{work}})
			calls := []ptypes.ContentBlock{faux.ToolCall("task", json.RawMessage(`{"prompt":"work"}`), "task-1")}
			if test.parallel {
				calls = append(calls, faux.ToolCall("task", json.RawMessage(`{"prompt":"work"}`), "task-2"))
			}
			parent := faux.New(faux.Options{Models: []faux.ModelDef{{ID: "parent", Cost: test.parentCost}}})
			parent.SetResponses(faux.Respond(calls...), faux.Respond(faux.Text("parent done")))
			a := agent.NewAgent(agent.AgentOptions{
				InitialState: &agent.AgentState{Model: parent.Model(), Tools: []agent.AgentTool{task}},
				StreamFn:     parent.StreamSimple,
				Budget:       agent.Budget{MaxCostUSD: test.budget},
			})
			store, err := journal.Create(ctx, t.TempDir(), "/workspace", journal.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			detach := session.AttachJournal(a, store)
			defer detach()
			result, err := a.PromptText(ctx, "delegate")
			if err != nil {
				t.Fatal(err)
			}
			if result.StopReason != test.wantStop || child.CallCount() != test.wantChildCalls {
				t.Fatalf("stop=%s child calls=%d, want %s/%d (error %v)", result.StopReason, child.CallCount(), test.wantStop, test.wantChildCalls, result.Err)
			}
			if test.budget > 0 && parent.CallCount() != 1 {
				t.Fatalf("parent made %d calls after cap or pricing refusal", parent.CallCount())
			}
			wantUsage := childUsage
			for _, message := range a.State().Messages {
				if assistant, ok := message.(ptypes.AssistantMessage); ok {
					wantUsage.Add(assistant.Usage)
				}
			}
			if !reflect.DeepEqual(result.Usage, wantUsage) {
				t.Fatalf("run usage=%+v, parent and child calls=%+v", result.Usage, wantUsage)
			}
			loaded, err := journal.Load(ctx, store.Path())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(loaded.UsageTotal, result.Usage) {
				t.Fatalf("journal usage=%+v, run usage=%+v", loaded.UsageTotal, result.Usage)
			}
			if loaded.LastRun == nil || loaded.LastRun.StopReason != string(result.StopReason) {
				t.Fatalf("journal last run=%+v, run stop=%s", loaded.LastRun, result.StopReason)
			}
			for _, message := range loaded.Messages {
				if assistant, ok := message.(ptypes.AssistantMessage); ok && assistant.Model == "child" {
					t.Fatal("child transcript leaked into parent journal")
				}
			}
			if test.childCost.Unknown && test.budget == 0 && !result.Usage.Cost.Unknown {
				t.Fatal("unknown child price became free")
			}
		})
	}
}

func TestChildUsageListenerFailureStopsParent(t *testing.T) {
	child := faux.New(faux.Options{})
	child.SetResponses(faux.Respond(faux.Text("child done")))
	task := subagent.NewTool(subagent.Options{Model: child.Model(), StreamFn: child.StreamSimple})
	parent := faux.New(faux.Options{})
	parent.SetResponses(
		faux.Respond(faux.ToolCall("task", json.RawMessage(`{"prompt":"work"}`), "task-1")),
		faux.Respond(faux.Text("must not run")),
	)
	a := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: parent.Model(), Tools: []agent.AgentTool{task}}, StreamFn: parent.StreamSimple})
	failure := errors.New("usage persistence failed")
	a.Subscribe(func(_ context.Context, event agent.AgentEvent) error {
		if event.Type == agent.EventUsage {
			return failure
		}
		return nil
	})
	result, err := a.PromptText(context.Background(), "delegate")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != agent.StopError || !errors.Is(result.Err, failure) || parent.CallCount() != 1 {
		t.Fatalf("result=%+v parent calls=%d", result, parent.CallCount())
	}
}
