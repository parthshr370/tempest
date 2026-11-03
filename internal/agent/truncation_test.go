package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
)

// cutOff makes step's response end the way a provider reports a response
// that hit its output-token limit.
func cutOff(step faux.ResponseStep) faux.ResponseStep {
	return func(c types.Context, options *types.StreamOptions, state faux.State, model types.Model) (types.AssistantMessage, error) {
		message, err := step(c, options, state, model)
		message.StopReason = types.StopLength
		return message, err
	}
}

// A final answer cut off at max_tokens is not a completed run.
func TestTruncatedFinalAnswerStopsOutputTruncated(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(cutOff(faux.Respond(faux.Text("The answer begins and then"))))
	a := newBudgetAgent(f, Budget{}, probeTool())
	result, err := a.PromptText(context.Background(), "explain")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != StopOutputTruncated || !result.StopReason.Failed() || result.Err == nil || !strings.Contains(result.Err.Error(), "output-token limit") {
		t.Fatalf("result = %+v", result)
	}
	if text := finalAnswerText(a.State().Messages); text != "The answer begins and then" {
		t.Fatalf("partial answer = %q", text)
	}
}

// Tool calls in a cut-off response never run: the last one's arguments may be
// incomplete. Each is answered with the cause and the run goes on.
func TestTruncatedToolCallsAreAnsweredNotRun(t *testing.T) {
	var runs atomic.Int32
	tool := executorTool("probe", `{"type":"object","properties":{"id":{"type":"string"}}}`, ExecParallel, func(context.Context, string, json.RawMessage, AgentToolUpdateCallback) (AgentToolResult, error) {
		runs.Add(1)
		return AgentToolResult{Content: []types.ContentBlock{types.NewText("ran")}}, nil
	})
	f := faux.New(faux.Options{})
	f.SetResponses(
		cutOff(faux.Respond(faux.ToolCall("probe", json.RawMessage(`{"id":"a"}`), "first"), faux.ToolCall("probe", json.RawMessage(`{}`), "second"))),
		faux.Respond(faux.Text("done")),
	)
	a := newBudgetAgent(f, Budget{}, tool)
	result, err := a.PromptText(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != StopCompleted || result.Turns != 2 || runs.Load() != 0 {
		t.Fatalf("result = %+v, tool runs = %d", result, runs.Load())
	}
	results := toolResults(a.State().Messages)
	if len(results) != 2 {
		t.Fatalf("results = %d, want one per call", len(results))
	}
	for _, result := range results {
		if !result.IsError || !strings.Contains(result.Content[0].Text, "output-token limit") || !strings.Contains(result.Content[0].Text, "smaller calls") {
			t.Fatalf("result = %#v", result)
		}
	}
}

// A model that keeps sending the same oversized call is stopped by the
// repeated-failure guard instead of looping.
func TestRepeatedTruncatedToolCallStops(t *testing.T) {
	f := faux.New(faux.Options{})
	steps := make([]faux.ResponseStep, 0, RepeatedFailureLimit+1)
	for range RepeatedFailureLimit {
		steps = append(steps, cutOff(faux.Respond(faux.ToolCall("probe", json.RawMessage(`{"id":"x"}`), "call"))))
	}
	f.SetResponses(append(steps, faux.Respond(faux.Text("never")))...)
	a := newBudgetAgent(f, Budget{}, probeTool())
	result, err := a.PromptText(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != StopRepeatedFailure || f.CallCount() != RepeatedFailureLimit {
		t.Fatalf("result = %+v calls = %d", result, f.CallCount())
	}
}
