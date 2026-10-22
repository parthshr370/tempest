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
	"go.harness.dev/harness/internal/jobs"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/subagent"
	"go.harness.dev/harness/internal/work"
)

func TestDelegatedUsageAndSharedCostAdmission(t *testing.T) {
	priced := ptypes.ModelCost{Input: 1_000_000, Output: 1_000_000}
	for _, test := range []struct {
		name     string
		cost     ptypes.ModelCost
		budget   float64
		parallel bool
		maxCalls int
	}{{"priced child shares cap", priced, .5, false, 1}, {"parallel children share cap", priced, .5, true, 1}, {"unpriced child refused", ptypes.ModelCost{Unknown: true}, .5, false, 0}, {"uncapped usage stays unknown", ptypes.ModelCost{Unknown: true}, 0, false, 2}, {"parallel uncapped counted once", priced, 0, true, 3}} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			cwd := t.TempDir()
			store, err := journal.Create(ctx, t.TempDir(), cwd, journal.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			queues := session.NewDurableQueues(store, nil, nil)
			childProvider := faux.New(faux.Options{Models: []faux.ModelDef{{ID: "child", Cost: test.cost}}})
			childProvider.SetResponses(faux.Respond(faux.ToolCall("work", json.RawMessage(`{}`), "work-1")), faux.Respond(faux.Text("child done")), faux.Respond(faux.Text("child done")))
			var childUsage ptypes.Usage
			var usageMu sync.Mutex
			host := work.Host{SessionID: store.ID(), JournalPath: store.Path(), Cwd: cwd, Queues: queues, Append: store.AppendWorkFact, Enqueue: queues.Enqueue, PrepareTask: func(context.Context, work.TaskSpec) (work.TaskEnvironment, error) {
				return work.TaskEnvironment{Model: "child", Provider: "faux"}, nil
			}, RunTask: func(runCtx context.Context, launch work.TaskLaunch) (work.TaskResult, error) {
				// The callback is a test runner: production creates and records the fixed child journal in session.
				child := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: childProvider.Model(), Tools: []agent.AgentTool{{Tool: ptypes.Tool{Name: "work", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}, Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
					return agent.AgentToolResult{Content: []ptypes.ContentBlock{ptypes.NewText("ok")}}, nil
				}}}}, StreamFn: childProvider.StreamSimple})
				detach := child.Subscribe(func(_ context.Context, event agent.AgentEvent) error {
					if event.Type == agent.EventMessageEnd {
						if message, ok := event.Message.(ptypes.AssistantMessage); ok {
							usageMu.Lock()
							childUsage.Add(message.Usage)
							usageMu.Unlock()
						}
					}
					return nil
				})
				defer detach()
				result, runErr := child.PromptText(runCtx, launch.Spec.Prompt)
				return work.TaskResult{Text: "child result", Usage: result.Usage, StopReason: string(result.StopReason)}, runErr
			}}
			manager, err := jobs.New(host, jobs.Options{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			parentProvider := faux.New(faux.Options{Models: []faux.ModelDef{{ID: "parent"}}})
			calls := []ptypes.ContentBlock{faux.ToolCall("task", json.RawMessage(`{"prompt":"work","subagent_type":"explore"}`), "task-1")}
			if test.parallel {
				calls = append(calls, faux.ToolCall("task", json.RawMessage(`{"prompt":"work","subagent_type":"explore"}`), "task-2"))
			}
			parentProvider.SetResponses(faux.Respond(calls...), faux.Respond(faux.Text("parent reasoning")), faux.Respond(faux.Text("parent done")), faux.Respond(faux.Text("all children settled")))
			parent := agent.NewAgent(agent.AgentOptions{InitialState: &agent.AgentState{Model: parentProvider.Model(), Tools: []agent.AgentTool{subagent.NewTool(manager)}}, StreamFn: parentProvider.StreamSimple, Budget: agent.Budget{MaxCostUSD: test.budget}, QueueSource: queues, Work: manager})
			detach := session.AttachJournalWithOptions(parent, store, session.RecordOptions{Queues: queues})
			defer detach()
			result, err := parent.PromptText(ctx, "delegate")
			if err != nil {
				t.Fatal(err)
			}
			if childProvider.CallCount() > test.maxCalls {
				t.Fatalf("child calls=%d cap=%d", childProvider.CallCount(), test.maxCalls)
			}
			if test.budget == 0 && result.StopReason != agent.StopCompleted {
				t.Fatalf("result=%+v", result)
			}
			if test.budget > 0 && test.cost.Unknown && result.StopReason != agent.StopError {
				t.Fatalf("unpriced result=%+v", result)
			}
			usageMu.Lock()
			want := childUsage
			usageMu.Unlock()
			for _, message := range parent.State().Messages {
				if assistant, ok := message.(ptypes.AssistantMessage); ok {
					want.Add(assistant.Usage)
				}
			}
			if !reflect.DeepEqual(result.Usage, want) {
				t.Fatalf("usage=%+v want=%+v", result.Usage, want)
			}
			loaded, err := journal.Load(ctx, store.Path())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(loaded.UsageTotal, result.Usage) {
				t.Fatalf("journal=%+v run=%+v", loaded.UsageTotal, result.Usage)
			}
			for _, message := range loaded.Messages {
				if assistant, ok := message.(ptypes.AssistantMessage); ok && assistant.Model == "child" {
					t.Fatal("child transcript leaked")
				}
			}
			if test.cost.Unknown && test.budget == 0 && !result.Usage.Cost.Unknown {
				t.Fatal("unknown child became free")
			}
		})
	}
}

func TestTaskAdmissionErrorIsToolFailure(t *testing.T) {
	failure := errors.New("admission failure")
	controller := &rejectTask{failure: failure}
	tool := subagent.NewTool(controller)
	if _, err := tool.Execute(context.Background(), "raw", json.RawMessage(`{"prompt":"work","subagent_type":"explore"}`), nil); !errors.Is(err, failure) {
		t.Fatal(err)
	}
}

type rejectTask struct {
	work.Controller
	failure error
}

func (controller *rejectTask) StartTask(context.Context, work.TaskSpec) (work.Job, error) {
	return work.Job{}, controller.failure
}
