package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
)

// billedStream answers every call with one tool call whose usage carries cost,
// the way a provider that reports billed cost does.
func billedStream(cost types.Cost, calls *int) StreamFn {
	return func(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
		*calls++
		s := stream.NewAssistantStream(model.API, model.Provider, model.ID)
		message := &types.AssistantMessage{
			API: model.API, Provider: model.Provider, Model: model.ID,
			Content:    []types.ContentBlock{types.NewToolCall("tc", "work", json.RawMessage(`{}`))},
			StopReason: types.StopToolUse,
			Usage:      types.Usage{Input: 10, Output: 2, TotalTokens: 12, Cost: cost},
		}
		go func() {
			s.Push(types.StreamEvent{Type: types.EvStart, Partial: message})
			s.Push(types.StreamEvent{Type: types.EvDone, Reason: types.StopToolUse, Message: message})
		}()
		return s
	}
}

func newBilledAgent(model types.Model, budget Budget, streamFn StreamFn) *Agent {
	return NewAgent(AgentOptions{
		InitialState: &AgentState{Model: model, Tools: []AgentTool{okTool("work")}},
		ConvertToLlm: func(messages []types.Message) []types.Message { return messages },
		StreamFn:     streamFn,
		Budget:       budget,
	})
}

// A route that reports billed cost needs no price list for a cost budget: the
// run starts, sums the billed amounts, and stops once they reach the cap.
func TestCostBudgetEnforcedFromBilledCost(t *testing.T) {
	model := types.Model{API: "openai-completions", Provider: "openrouter", ID: "vendor/unlisted", Cost: types.ModelCost{Unknown: true}}
	calls := 0
	a := newBilledAgent(model, Budget{MaxCostUSD: 0.5}, billedStream(types.Cost{Total: 0.3}, &calls))
	result, err := a.PromptText(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != StopMaxCost || calls != 2 || result.Usage.Cost.Unknown || result.Usage.Cost.Total != 0.6 {
		t.Fatalf("result = %+v calls=%d, want max_cost after two billed calls totalling 0.6", result, calls)
	}
}

// A billed-cost route whose call came back without a cost leaves the budget
// unenforceable; the run fails rather than treating the call as free.
func TestCostBudgetFailsWhenBilledCostMissing(t *testing.T) {
	model := types.Model{API: "openai-completions", Provider: "openrouter", ID: "vendor/unlisted", Cost: types.ModelCost{Unknown: true}}
	calls := 0
	a := newBilledAgent(model, Budget{MaxCostUSD: 0.5}, billedStream(types.Cost{Unknown: true}, &calls))
	result, err := a.PromptText(context.Background(), "go")
	if !errors.Is(err, ErrUnknownPrice) && !errors.Is(result.Err, ErrUnknownPrice) {
		t.Fatalf("err = %v result.Err = %v, want ErrUnknownPrice", err, result.Err)
	}
	if calls != 1 {
		t.Fatalf("provider calls = %d, want the run to stop after the unpriced call", calls)
	}
}

func TestBudgetValidateAcceptsBilledCostRoute(t *testing.T) {
	billed := types.Model{Provider: "openrouter", ID: "vendor/unlisted", Cost: types.ModelCost{Unknown: true}}
	if err := (Budget{MaxCostUSD: 1}).Validate(billed); err != nil {
		t.Fatalf("Validate(billed-cost route) = %v, want nil", err)
	}
	unpriced := types.Model{Provider: "openai", ID: "unlisted", Cost: types.ModelCost{Unknown: true}}
	if err := (Budget{MaxCostUSD: 1}).Validate(unpriced); !errors.Is(err, ErrUnknownPrice) {
		t.Fatalf("Validate(unpriced route) = %v, want ErrUnknownPrice", err)
	}
}
