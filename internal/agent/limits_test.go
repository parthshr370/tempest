package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/retry"
)

// metered gives message usage: 100 prompt tokens (60 uncached, 30 cache
// read, 10 cache write) and 10 output tokens.
func metered(message types.AssistantMessage) types.AssistantMessage {
	message.Usage = types.Usage{Input: 60, CacheRead: 30, CacheWrite: 10, Output: 10, TotalTokens: 110}
	return message
}

func meteredScript(messages ...types.AssistantMessage) *scriptedRequests {
	script := &scriptedRequests{}
	for _, message := range messages {
		script.script = append(script.script, metered(message))
	}
	return script
}

func textAnswer() types.AssistantMessage {
	return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("done")}, StopReason: types.StopStop}
}

// countingTool counts its executions.
func countingTool(name string, runs *atomic.Int32) AgentTool {
	return executorTool(name, "", ExecParallel, func(context.Context, string, json.RawMessage, AgentToolUpdateCallback) (AgentToolResult, error) {
		runs.Add(1)
		return AgentToolResult{Content: []types.ContentBlock{types.NewText("ok")}}, nil
	})
}

func newLimitAgent(script *scriptedRequests, budget Budget, tools ...AgentTool) *Agent {
	return NewAgent(AgentOptions{
		InitialState: &AgentState{SystemPrompt: "system", Model: reminderModel, Tools: tools},
		ConvertToLlm: func(messages []types.Message) []types.Message { return messages },
		StreamFn:     script.stream,
		Budget:       budget,
	})
}

// Each response uses 100 prompt and 10 output tokens. A cap of N admits no
// request once N is used, so the request that reaches or passes N is the
// last one: limit-1 and limit stop after 2 requests, limit+1 after 3.
func TestTokenCapsStopAtTheBoundary(t *testing.T) {
	for _, test := range []struct {
		name   string
		budget func(int) Budget
		limit  int
		want   StopReason
	}{
		{"input", func(n int) Budget { return Budget{MaxInputTokens: n} }, 200, StopMaxInputTokens},
		{"output", func(n int) Budget { return Budget{MaxOutputTokens: n} }, 20, StopMaxOutputTokens},
		{"total", func(n int) Budget { return Budget{MaxTotalTokens: n} }, 220, StopMaxTotalTokens},
	} {
		for _, step := range []struct {
			delta    int
			requests int
		}{{-1, 2}, {0, 2}, {1, 3}} {
			t.Run(fmt.Sprintf("%s/limit%+d", test.name, step.delta), func(t *testing.T) {
				var runs atomic.Int32
				script := meteredScript(toolTurns("work", 10)...)
				a := newLimitAgent(script, test.budget(test.limit+step.delta), countingTool("work", &runs))
				result, err := a.PromptText(context.Background(), "go")
				if err != nil {
					t.Fatal(err)
				}
				if result.StopReason != test.want || len(script.requests) != step.requests || result.Turns != step.requests {
					t.Fatalf("result = %+v requests = %d, want %s after %d", result, len(script.requests), test.want, step.requests)
				}
				if want := (types.Usage{Input: 60 * step.requests, CacheRead: 30 * step.requests, CacheWrite: 10 * step.requests, Output: 10 * step.requests, TotalTokens: 110 * step.requests, ToolCalls: step.requests}); result.Usage != want {
					t.Fatalf("usage = %+v, want %+v", result.Usage, want)
				}
				// The stopping turn's tools ran, so the transcript ends on a
				// complete turn.
				if got := runtimeRoles(a.State().Messages); !strings.HasSuffix(got, "assistant,toolResult") || int(runs.Load()) != step.requests {
					t.Fatalf("roles = %s runs = %d", got, runs.Load())
				}
			})
		}
	}
}

// Under a token cap a completed response that reports no tokens cannot be
// counted, so the run fails rather than treating it as free.
func TestTokenCapRefusesUnmeasuredResponse(t *testing.T) {
	script := &scriptedRequests{script: []types.AssistantMessage{textAnswer()}}
	result, err := newLimitAgent(script, Budget{MaxTotalTokens: 1000}).PromptText(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != StopError || !errors.Is(result.Err, ErrUnknownUsage) {
		t.Fatalf("result = %+v, want error wrapping ErrUnknownUsage", result)
	}
	script = &scriptedRequests{script: []types.AssistantMessage{textAnswer()}}
	if result, err = newLimitAgent(script, Budget{MaxToolCalls: 5}).PromptText(context.Background(), "go"); err != nil || result.StopReason != StopCompleted {
		t.Fatalf("without a token cap: result = %+v err = %v", result, err)
	}
}

// A child's response without usage fails the root run when the root has a
// token cap, even when the delegating tool turns the child's error into an
// ordinary result: the root sends nothing more. A cap the child alone has
// fails only the child.
func TestUnmeasuredChildResponseStopsATokenCappedRoot(t *testing.T) {
	for _, test := range []struct {
		name             string
		root, child      Budget
		wantRootErr      bool
		wantRootRequests int
	}{
		{"root cap", Budget{MaxTotalTokens: 1000}, Budget{}, true, 1},
		{"child cap only", Budget{}, Budget{MaxTotalTokens: 1000}, false, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			var childUnknown atomic.Bool
			delegate := executorTool("delegate", "", ExecParallel, func(ctx context.Context, _ string, _ json.RawMessage, _ AgentToolUpdateCallback) (AgentToolResult, error) {
				child := newLimitAgent(&scriptedRequests{script: []types.AssistantMessage{textAnswer()}}, test.child)
				result, err := child.PromptText(ctx, "go")
				childUnknown.Store(errors.Is(result.Err, ErrUnknownUsage))
				// The child's failure reaches the root only as tool output.
				return AgentToolResult{Content: []types.ContentBlock{types.NewText(fmt.Sprint(result.Err))}}, err
			})
			script := meteredScript(toolCallMessage("delegate"), textAnswer())
			result, err := newLimitAgent(script, test.root, delegate).PromptText(context.Background(), "go")
			if err != nil {
				t.Fatal(err)
			}
			if !childUnknown.Load() {
				t.Fatal("child run did not fail with ErrUnknownUsage")
			}
			if got := errors.Is(result.Err, ErrUnknownUsage); got != test.wantRootErr || len(script.requests) != test.wantRootRequests {
				t.Fatalf("root result = %+v after %d requests, want ErrUnknownUsage=%v after %d", result, len(script.requests), test.wantRootErr, test.wantRootRequests)
			}
		})
	}
}

// failedAfterUsage is an attempt that streams, reports usage, then drops:
// what a provider bills for although its stream failed.
func failedAfterUsage(usage types.Usage) func(context.Context, *stream.AssistantStream) {
	return func(_ context.Context, events *stream.AssistantStream) {
		events.Push(types.StreamEvent{Type: types.EvStart})
		events.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopError, Err: &types.AssistantMessage{
			StopReason: types.StopError, ErrorCode: string(retry.CodeStreamInterrupted), ErrorMessage: "stream dropped", Usage: usage,
		}})
	}
}

// A request is sent again, as a retry or to a backup, only while the run is
// under its caps: an attempt that failed after its usage reached a cost or
// token cap ends the run at that cap instead of paying for another attempt.
func TestRetryAndBackupAreReadmittedAfterAFailedAttempt(t *testing.T) {
	spent := types.Usage{Input: 100, TotalTokens: 100, Cost: types.Cost{Total: 0.01}}
	for _, test := range []struct {
		name     string
		budget   Budget
		backup   bool
		wantStop StopReason
		wantSent int
	}{
		{"retry: input cap", Budget{MaxInputTokens: 100}, false, StopMaxInputTokens, 1},
		{"retry: cost cap", Budget{MaxCostUSD: 0.01}, false, StopMaxCost, 1},
		{"retry: under the caps", Budget{MaxInputTokens: 101, MaxCostUSD: 0.02}, false, StopCompleted, 2},
		{"backup: input cap", Budget{MaxInputTokens: 100}, true, StopMaxInputTokens, 1},
		{"backup: total cap", Budget{MaxTotalTokens: 100}, true, StopMaxTotalTokens, 1},
		{"backup: cost cap", Budget{MaxCostUSD: 0.01}, true, StopMaxCost, 1},
		{"backup: under the caps", Budget{MaxInputTokens: 101, MaxCostUSD: 0.02}, true, StopCompleted, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy := retry.DefaultConfig()
			policy.Sleeper = instantSleeper{}
			primary := &scriptedProvider{attempts: []func(context.Context, *stream.AssistantStream){failedAfterUsage(spent), answeredAttempt}}
			model := types.Model{API: "test", Provider: "test", ID: "primary"}
			providers := []*scriptedProvider{primary}
			var streamFn StreamFn
			if test.backup {
				policy.MaxAttempts = 1
				primary.attempts = primary.attempts[:1]
				providers = append(providers, &scriptedProvider{attempts: []func(context.Context, *stream.AssistantStream){answeredAttempt}})
				var candidates []retry.Candidate
				for i, provider := range providers {
					retrier, err := retry.New(provider, policy)
					if err != nil {
						t.Fatal(err)
					}
					candidate := model
					if i > 0 {
						candidate.ID = "backup"
					}
					candidates = append(candidates, retry.Candidate{Model: candidate, Stream: retrier})
				}
				chain, err := retry.NewFallback(candidates)
				if err != nil {
					t.Fatal(err)
				}
				streamFn = chain.Stream
			} else {
				policy.MaxAttempts = 2
				retrier, err := retry.New(primary, policy)
				if err != nil {
					t.Fatal(err)
				}
				streamFn = retrier.Stream
			}
			a := NewAgent(AgentOptions{InitialState: &AgentState{Model: model}, StreamFn: streamFn, Budget: test.budget})
			result, err := a.PromptText(context.Background(), "go")
			if err != nil {
				t.Fatal(err)
			}
			sent := 0
			for _, provider := range providers {
				sent += provider.calls()
			}
			if result.Err != nil || result.StopReason != test.wantStop || sent != test.wantSent {
				t.Fatalf("result = %+v after %d attempts, want %q after %d", result, sent, test.wantStop, test.wantSent)
			}
			if want := 100 * test.wantSent; promptTokens(result.Usage) != want {
				t.Fatalf("prompt tokens = %d, want %d", promptTokens(result.Usage), want)
			}
		})
	}
}

// Lowering the budget while a run is active changes runs started after the
// call; the active run keeps the budget it started with.
func TestSetBudgetDuringARunAppliesToTheNextRun(t *testing.T) {
	var a *Agent
	var calls atomic.Int32
	work := executorTool("work", "", ExecParallel, func(context.Context, string, json.RawMessage, AgentToolUpdateCallback) (AgentToolResult, error) {
		calls.Add(1)
		a.SetBudget(Budget{MaxToolCalls: 1})
		return AgentToolResult{}, nil
	})
	a = newLimitAgent(meteredScript(toolCallMessage("work"), toolCallMessage("work"), textAnswer(), toolCallMessage("work", "work")), Budget{MaxToolCalls: 10}, work)
	result, err := a.PromptText(context.Background(), "go")
	if err != nil || result.StopReason != StopCompleted || calls.Load() != 2 {
		t.Fatalf("active run: result = %+v err = %v after %d calls, want completed after 2", result, err, calls.Load())
	}
	if result, err = a.PromptText(context.Background(), "again"); err != nil || result.StopReason != StopMaxToolCalls || calls.Load() != 2 {
		t.Fatalf("next run: result = %+v err = %v after %d calls, want max_tool_calls with none run", result, err, calls.Load())
	}
}

// A response's tool calls run together or not at all. Responses ask for 2,
// then 2 calls, then answer: a cap of 3 runs the first pair and refuses the
// second, a cap of 4 or 5 runs both and completes, and a cap of 1 refuses
// the first pair outright.
func TestToolCallCapAdmitsWholeBatches(t *testing.T) {
	for _, test := range []struct {
		limit, runs, requests int
		want                  StopReason
	}{
		{1, 0, 1, StopMaxToolCalls},
		{3, 2, 2, StopMaxToolCalls},
		{4, 4, 3, StopCompleted},
		{5, 4, 3, StopCompleted},
	} {
		t.Run(fmt.Sprintf("limit %d", test.limit), func(t *testing.T) {
			var runs atomic.Int32
			script := meteredScript(toolCallMessage("work", "work"), toolCallMessage("work", "work"), textAnswer())
			a := newLimitAgent(script, Budget{MaxToolCalls: test.limit}, countingTool("work", &runs))
			var started, ended int
			a.Subscribe(func(_ context.Context, ev AgentEvent) error {
				switch ev.Type {
				case EventToolExecutionStart:
					started++
				case EventToolExecutionEnd:
					ended++
				}
				return nil
			})
			result, err := a.PromptText(context.Background(), "go")
			if err != nil {
				t.Fatal(err)
			}
			if result.StopReason != test.want || int(runs.Load()) != test.runs || len(script.requests) != test.requests || result.Usage.ToolCalls != test.runs {
				t.Fatalf("result = %+v runs = %d requests = %d, want %s with %d runs", result, runs.Load(), len(script.requests), test.want, test.runs)
			}
			messages := a.State().Messages
			if err := everyCallAnswered(messages); err != nil {
				t.Fatal(err)
			}
			if started != ended || started != 2*(test.requests-boolInt(test.want == StopCompleted)) {
				t.Fatalf("tool events start=%d end=%d", started, ended)
			}
			if test.want != StopMaxToolCalls {
				return
			}
			for _, message := range messages[len(messages)-2:] {
				result, ok := message.(types.ToolResultMessage)
				if !ok || !result.IsError || !strings.Contains(result.Content[0].Text, fmt.Sprintf("tool-call limit of %d allows %d more", test.limit, test.limit-test.runs)) {
					t.Fatalf("refused result = %#v", message)
				}
			}
		})
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// everyCallAnswered is what a provider requires: each tool call has exactly
// one result before the next assistant message.
func everyCallAnswered(messages []types.Message) error {
	open := map[string]bool{}
	for i, message := range messages {
		switch m := message.(type) {
		case types.AssistantMessage:
			if len(open) > 0 {
				return fmt.Errorf("message %d: calls %v unanswered", i, open)
			}
			for _, block := range m.Content {
				if block.Type == types.BlockToolCall {
					open[block.ID] = true
				}
			}
		case types.ToolResultMessage:
			if !open[m.ToolCallID] {
				return fmt.Errorf("message %d: orphan or duplicate result %s", i, m.ToolCallID)
			}
			delete(open, m.ToolCallID)
		}
	}
	if len(open) > 0 {
		return fmt.Errorf("calls %v unanswered", open)
	}
	return nil
}

// A delegated child run spends from the root's caps: its tool calls take
// the root's tool-call allowance and its tokens count toward the root's
// token caps.
func TestChildRunsChargeTheRootLimits(t *testing.T) {
	type outcome struct {
		root       StopReason
		child      StopReason
		childRuns  int32
		rootRuns   int32
		rootCalls  int
		toolCalls  int
		rootTokens int
	}
	run := func(t *testing.T, budget Budget) outcome {
		var childRuns, rootRuns atomic.Int32
		var childStop StopReason
		delegate := executorTool("delegate", "", ExecParallel, func(ctx context.Context, _ string, _ json.RawMessage, _ AgentToolUpdateCallback) (AgentToolResult, error) {
			child := newLimitAgent(meteredScript(toolCallMessage("work", "work"), textAnswer()), Budget{}, countingTool("work", &childRuns))
			result, err := child.PromptText(ctx, "sub task")
			childStop = result.StopReason
			return AgentToolResult{Content: []types.ContentBlock{types.NewText(string(result.StopReason))}}, err
		})
		script := meteredScript(toolCallMessage("delegate"), toolCallMessage("work", "work"), textAnswer())
		root := newLimitAgent(script, budget, delegate, countingTool("work", &rootRuns))
		result, err := root.PromptText(context.Background(), "go")
		if err != nil {
			t.Fatal(err)
		}
		if err := everyCallAnswered(root.State().Messages); err != nil {
			t.Fatal(err)
		}
		return outcome{result.StopReason, childStop, childRuns.Load(), rootRuns.Load(), len(script.requests), result.Usage.ToolCalls, result.Usage.Output}
	}
	for _, test := range []struct {
		name   string
		budget Budget
		want   outcome
	}{
		// delegate (1) + child pair (3) fits 4; the root's own pair would make 5.
		{"tool calls: child fits, root refused", Budget{MaxToolCalls: 4}, outcome{StopMaxToolCalls, StopCompleted, 2, 0, 2, 3, 40}},
		// The child's pair would pass 2 while the delegate call holds 1.
		{"tool calls: child refused", Budget{MaxToolCalls: 2}, outcome{StopMaxToolCalls, StopMaxToolCalls, 0, 0, 2, 1, 30}},
		{"tool calls: all fit", Budget{MaxToolCalls: 5}, outcome{StopCompleted, StopCompleted, 2, 2, 3, 5, 50}},
		// Root 10 + child 2x10 output tokens reach 30 before the root's
		// second request; 31 admits it.
		{"tokens: child spend stops the root", Budget{MaxOutputTokens: 30}, outcome{StopMaxOutputTokens, StopCompleted, 2, 0, 1, 3, 30}},
		{"tokens: one under", Budget{MaxOutputTokens: 31}, outcome{StopMaxOutputTokens, StopCompleted, 2, 2, 2, 5, 40}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := run(t, test.budget); got != test.want {
				t.Fatalf("outcome = %+v, want %+v", got, test.want)
			}
		})
	}
}

// Usage recovered from a resumed logical run counts against the caps: the
// run stops before any request when a token cap is already reached, and a
// batch is refused when the recovered tool calls leave too little room.
func TestRecoveredUsageCountsAgainstTokenAndToolCallCaps(t *testing.T) {
	recovered := types.Usage{Input: 150, CacheRead: 50, Output: 20, ToolCalls: 3}
	for _, test := range []struct {
		name     string
		budget   Budget
		want     StopReason
		requests int
		runs     int32
	}{
		{"input reached", Budget{MaxInputTokens: 200}, StopMaxInputTokens, 0, 0},
		{"input left", Budget{MaxInputTokens: 201}, StopMaxInputTokens, 1, 2},
		{"output reached", Budget{MaxOutputTokens: 20}, StopMaxOutputTokens, 0, 0},
		{"total reached", Budget{MaxTotalTokens: 220}, StopMaxTotalTokens, 0, 0},
		{"tool calls short", Budget{MaxToolCalls: 4}, StopMaxToolCalls, 1, 0},
		{"tool calls exact", Budget{MaxToolCalls: 5}, StopCompleted, 2, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			var runs atomic.Int32
			script := meteredScript(toolCallMessage("work", "work"), textAnswer())
			a := newLimitAgent(script, test.budget, countingTool("work", &runs))
			a.Subscribe(func(_ context.Context, ev AgentEvent) error {
				if ev.Type == EventAgentStart {
					a.SetRunState(RunState{RunID: "r2", LogicalRunID: "r1", Usage: recovered})
				}
				return nil
			})
			result, err := a.PromptText(context.Background(), "go")
			if err != nil {
				t.Fatal(err)
			}
			if result.StopReason != test.want || len(script.requests) != test.requests || runs.Load() != test.runs {
				t.Fatalf("result = %+v requests = %d runs = %d", result, len(script.requests), runs.Load())
			}
			if result.Usage.ToolCalls != recovered.ToolCalls+int(test.runs) {
				t.Fatalf("tool calls = %d, want recovered %d plus %d", result.Usage.ToolCalls, recovered.ToolCalls, test.runs)
			}
		})
	}
}

func TestBudgetValidateRejectsNegativeTokenAndToolCallLimits(t *testing.T) {
	for _, budget := range []Budget{{MaxInputTokens: -1}, {MaxOutputTokens: -1}, {MaxTotalTokens: -1}, {MaxToolCalls: -1}} {
		if err := budget.Validate(types.Model{}); err == nil {
			t.Errorf("Validate(%+v) = nil, want an error", budget)
		}
	}
}

// Two parallel children race for the one tool call the root's cap has left
// after its two delegate calls: exactly one child's call runs.
func TestParallelChildrenShareTheLastToolCall(t *testing.T) {
	var childCalls atomic.Int32
	delegate := executorTool("delegate", "", ExecParallel, func(ctx context.Context, _ string, _ json.RawMessage, _ AgentToolUpdateCallback) (AgentToolResult, error) {
		child := newLimitAgent(meteredScript(toolCallMessage("work"), textAnswer()), Budget{}, countingTool("work", &childCalls))
		_, err := child.PromptText(ctx, "go")
		return AgentToolResult{}, err
	})
	result, err := newLimitAgent(meteredScript(toolCallMessage("delegate", "delegate"), textAnswer()), Budget{MaxToolCalls: 3}, delegate).PromptText(context.Background(), "go")
	if err != nil || result.Err != nil || childCalls.Load() != 1 || result.Usage.ToolCalls != 3 {
		t.Fatalf("child calls = %d, result = %+v, err = %v; want 1 child call and 3 charged", childCalls.Load(), result, err)
	}
}
