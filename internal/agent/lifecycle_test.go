package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
)

// lateUpdateDelay gives a tool's stray goroutine time to fire after the run
// has ended, so the test observes whether its update escapes.
const lateUpdateDelay = 20 * time.Millisecond

func okTool(name string) AgentTool {
	return executorTool(name, "", ExecParallel, func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
		return AgentToolResult{Content: []types.ContentBlock{types.NewText("ok")}}, nil
	})
}

func toolCallStep(id string) faux.ResponseStep {
	return faux.Respond(faux.ToolCall("work", json.RawMessage(`{}`), id))
}

func newBudgetAgent(f *faux.Faux, budget Budget, tool AgentTool) *Agent {
	return newRuntimeAgent(f, func(opts *AgentOptions) {
		opts.InitialState.Tools = []AgentTool{tool}
		opts.Budget = budget
	})
}

func lastRole(t *testing.T, a *Agent) string {
	t.Helper()
	messages := a.State().Messages
	if len(messages) == 0 {
		t.Fatal("transcript is empty")
	}
	return messages[len(messages)-1].Role()
}

func TestRunCompletesOnTurnWithoutToolCalls(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(toolCallStep("tc1"), faux.Respond(faux.Text("done")))
	a := newBudgetAgent(f, Budget{}, okTool("work"))

	result, err := a.PromptText(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != StopCompleted || result.Err != nil || result.Turns != 2 || f.CallCount() != 2 {
		t.Fatalf("result = %+v calls=%d, want completed after 2 turns", result, f.CallCount())
	}
	var want types.Usage
	for _, message := range a.State().Messages {
		if assistant, ok := message.(types.AssistantMessage); ok {
			want.Add(assistant.Usage)
		}
	}
	if want.TotalTokens == 0 || result.Usage.TotalTokens != want.TotalTokens {
		t.Fatalf("run usage tokens = %d, want sum of call usage %d", result.Usage.TotalTokens, want.TotalTokens)
	}
}

func TestRunEndsWithStopErrorOnProviderFailure(t *testing.T) {
	f := faux.New(faux.Options{}) // no responses queued: the call fails
	a := newRuntimeAgent(f)

	result, err := a.PromptText(context.Background(), "go")
	if err != nil {
		t.Fatalf("a started run must report failure in the result, got err %v", err)
	}
	if result.StopReason != StopError || result.Err == nil || result.Turns != 1 {
		t.Fatalf("result = %+v, want StopError with cause", result)
	}
}

func TestRunStopsAtMaxTurnsAfterToolsFinish(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(toolCallStep("tc1"), toolCallStep("tc2"), faux.Respond(faux.Text("never")))
	a := newBudgetAgent(f, Budget{MaxTurns: 1}, okTool("work"))

	result, err := a.PromptText(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != StopMaxTurns || result.Turns != 1 || f.CallCount() != 1 {
		t.Fatalf("result = %+v calls=%d, want max_turns after 1 call", result, f.CallCount())
	}
	// The tripped turn's tools ran, so the transcript is resumable.
	if got := lastRole(t, a); got != "toolResult" {
		t.Fatalf("last message role = %q, want toolResult", got)
	}
}

func TestBudgetBoundaryKeepsDrainedSteering(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(toolCallStep("tc1"), faux.Respond(faux.Text("never")))
	var a *Agent
	steeringTool := executorTool("work", "", ExecParallel, func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
		a.Steer(userMessage("steer mid-run"))
		return AgentToolResult{Content: []types.ContentBlock{types.NewText("ok")}}, nil
	})
	a = newBudgetAgent(f, Budget{MaxTurns: 1}, steeringTool)

	result, err := a.PromptText(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != StopMaxTurns {
		t.Fatalf("result = %+v, want max_turns", result)
	}
	if got := runtimeRoles(a.State().Messages); got != "user,assistant,toolResult,user" {
		t.Fatalf("roles = %q, want the drained steering message kept", got)
	}
}

func TestRunStopsAtMaxCost(t *testing.T) {
	// $1 per input or output token: any call exceeds the $0.50 cap.
	f := faux.New(faux.Options{Models: []faux.ModelDef{{ID: "priced", Cost: types.ModelCost{Input: 1_000_000, Output: 1_000_000}}}})
	f.SetResponses(toolCallStep("tc1"), faux.Respond(faux.Text("never")))
	a := newBudgetAgent(f, Budget{MaxCostUSD: 0.5}, okTool("work"))

	result, err := a.PromptText(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != StopMaxCost || result.Turns != 1 || result.Usage.Cost.Total < 0.5 {
		t.Fatalf("result = %+v, want max_cost after 1 priced call", result)
	}
}

func TestCostBudgetRefusesUnknownPrice(t *testing.T) {
	f := faux.New(faux.Options{Models: []faux.ModelDef{{ID: "unpriced", Cost: types.ModelCost{Unknown: true}}}})
	f.SetResponses(faux.Respond(faux.Text("never")))
	a := newBudgetAgent(f, Budget{MaxCostUSD: 1}, okTool("work"))

	if _, err := a.PromptText(context.Background(), "go"); !errors.Is(err, ErrUnknownPrice) {
		t.Fatalf("Prompt err = %v, want ErrUnknownPrice", err)
	}
	if f.CallCount() != 0 {
		t.Fatalf("provider called %d times for a refused run", f.CallCount())
	}
}

func TestRunStopsAtMaxWallTime(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(toolCallStep("tc1"), faux.Respond(faux.Text("never")))
	a := newBudgetAgent(f, Budget{MaxWallTime: time.Nanosecond}, okTool("work"))

	result, err := a.PromptText(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != StopMaxWallTime || result.Turns != 1 || f.CallCount() != 1 {
		t.Fatalf("result = %+v calls=%d, want max_wall_time after 1 call", result, f.CallCount())
	}
}

// eventLog records events and flags any event delivered after agent_end.
type eventLog struct {
	mu         sync.Mutex
	events     []AgentEvent
	ended      bool
	afterEnd   []AgentEventType
	agentEnded int
}

func (l *eventLog) listen(ctx context.Context, ev AgentEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		l.afterEnd = append(l.afterEnd, ev.Type)
	}
	if ev.Type == EventAgentEnd {
		l.ended = true
		l.agentEnded++
	}
	l.events = append(l.events, ev)
	return nil
}

func TestAbortDuringToolJoinsToolBeforeAgentEnd(t *testing.T) {
	for _, mode := range []ToolExecutionMode{ExecParallel, ExecSequential} {
		t.Run(string(mode), func(t *testing.T) {
			f := faux.New(faux.Options{})
			f.SetResponses(toolCallStep("tc1"), faux.Respond(faux.Text("never")))
			started := make(chan struct{})
			toolReturned := make(chan struct{})
			var stray sync.WaitGroup
			tool := executorTool("work", "", mode, func(ctx context.Context, id string, params json.RawMessage, onUpdate AgentToolUpdateCallback) (AgentToolResult, error) {
				close(started)
				<-ctx.Done()
				// A goroutine the tool leaks reports progress after the run
				// has ended; that update must not reach listeners.
				stray.Add(1)
				go func() {
					defer stray.Done()
					time.Sleep(lateUpdateDelay)
					onUpdate(AgentToolResult{Content: []types.ContentBlock{types.NewText("late")}})
				}()
				close(toolReturned)
				return AgentToolResult{}, ctx.Err()
			})
			a := newBudgetAgent(f, Budget{}, tool)
			log := &eventLog{}
			a.Subscribe(log.listen)

			done := make(chan RunResult, 1)
			go func() {
				result, err := a.PromptText(context.Background(), "go")
				if err != nil {
					t.Errorf("Prompt err = %v", err)
				}
				done <- result
			}()
			waitRuntimeEvent(t, started)
			a.Abort()

			var result RunResult
			select {
			case result = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("run did not end after Abort")
			}
			select {
			case <-toolReturned:
			default:
				t.Fatal("Prompt returned before the tool goroutine returned")
			}
			if result.StopReason != StopAborted || f.CallCount() != 1 {
				t.Fatalf("result = %+v calls=%d, want aborted without another model call", result, f.CallCount())
			}

			stray.Wait()
			log.mu.Lock()
			defer log.mu.Unlock()
			if log.agentEnded != 1 || len(log.afterEnd) != 0 {
				t.Fatalf("agent_end count = %d, events after agent_end = %v", log.agentEnded, log.afterEnd)
			}
			last := log.events[len(log.events)-1]
			if last.Type != EventAgentEnd || last.StopReason != StopAborted {
				t.Fatalf("last event = %s %q, want agent_end aborted", last.Type, last.StopReason)
			}
			if got := lastRole(t, a); got != "toolResult" {
				t.Fatalf("last message role = %q, want the aborted call's result", got)
			}
		})
	}
}

func TestEventSeqStrictlyIncreasesAcrossRuns(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(toolCallStep("tc1"), faux.Respond(faux.Text("one")), faux.Respond(faux.Text("two")))
	a := newBudgetAgent(f, Budget{}, okTool("work"))
	log := &eventLog{}
	a.Subscribe(log.listen)

	for _, prompt := range []string{"first", "second"} {
		if _, err := a.PromptText(context.Background(), prompt); err != nil {
			t.Fatal(err)
		}
	}

	log.mu.Lock()
	defer log.mu.Unlock()
	starts := 0
	var previous uint64
	for _, ev := range log.events {
		if ev.Seq <= previous || ev.At.IsZero() {
			t.Fatalf("event %s seq=%d at=%v after seq %d: want strictly increasing seq and a timestamp", ev.Type, ev.Seq, ev.At, previous)
		}
		previous = ev.Seq
		if ev.Type == EventAgentStart {
			starts++
		}
	}
	if starts != 2 {
		t.Fatalf("agent_start count = %d, want 2", starts)
	}
}
