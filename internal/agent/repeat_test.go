package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
)

// probeTool fails with an error that depends only on its id argument, like a
// lookup of a job that doesn't exist, and succeeds when ok is set.
func probeTool() AgentTool {
	return executorTool("probe", `{"type":"object","properties":{"id":{"type":"string"},"ok":{"type":"boolean"}}}`, ExecParallel, func(_ context.Context, _ string, params json.RawMessage, _ AgentToolUpdateCallback) (AgentToolResult, error) {
		var input struct {
			ID string `json:"id"`
			OK bool   `json:"ok"`
		}
		if err := json.Unmarshal(params, &input); err != nil {
			return AgentToolResult{}, err
		}
		if input.OK {
			return AgentToolResult{Content: []types.ContentBlock{types.NewText("found")}}, nil
		}
		return AgentToolResult{}, fmt.Errorf("no job %q", input.ID)
	})
}

func probeSteps(args ...string) []faux.ResponseStep {
	steps := make([]faux.ResponseStep, 0, len(args))
	for _, arg := range args {
		steps = append(steps, faux.Respond(faux.ToolCall("probe", json.RawMessage(arg), "call")))
	}
	return steps
}

func repeatArgs(arg string, count int) []string {
	args := make([]string, count)
	for index := range args {
		args[index] = arg
	}
	return args
}

func toolResults(messages []types.Message) []types.ToolResultMessage {
	var results []types.ToolResultMessage
	for _, message := range messages {
		if result, ok := message.(types.ToolResultMessage); ok {
			results = append(results, result)
		}
	}
	return results
}

// hasNotice reports whether result carries the repeated-failure notice and,
// if so, whether it names streak.
func hasNotice(result types.ToolResultMessage, streak int) bool {
	last := result.Content[len(result.Content)-1]
	return len(result.Content) == 2 && strings.HasPrefix(last.Text, repeatedFailurePrefix) && strings.Contains(last.Text, strconv.Itoa(streak))
}

func TestRepeatedIdenticalFailureWarnsThenStops(t *testing.T) {
	f := faux.New(faux.Options{})
	// Key order and whitespace differ; the call is the same.
	args := []string{`{"id":"x"}`, `{ "id" : "x" }`, `{"id":"x"}`, `{"id": "x"}`, `{"id":"x"}`, `{"id":"x"}`}
	f.SetResponses(append(probeSteps(args...), faux.Respond(faux.Text("never")))...)
	a := newBudgetAgent(f, Budget{MaxTurns: 40}, probeTool())

	result, err := a.PromptText(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != StopRepeatedFailure || result.Turns != RepeatedFailureLimit || f.CallCount() != RepeatedFailureLimit {
		t.Fatalf("result = %+v calls=%d, want repeated_failure after %d calls", result, f.CallCount(), RepeatedFailureLimit)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "probe") || !strings.Contains(result.Err.Error(), `no job "x"`) {
		t.Fatalf("run error = %v, want the call and its error", result.Err)
	}
	results := toolResults(a.State().Messages)
	if len(results) != RepeatedFailureLimit || lastRole(t, a) != "toolResult" {
		t.Fatalf("results = %d, last role %s; want the stopping turn's result last", len(results), lastRole(t, a))
	}
	if len(results[0].Content) != 1 {
		t.Fatalf("first failure = %#v, want it unchanged", results[0].Content)
	}
	for index, result := range results[1:] {
		if !hasNotice(result, index+2) {
			t.Fatalf("failure %d = %#v, want the notice naming %d", index+2, result.Content, index+2)
		}
	}
}

func TestRepeatedFailureStreakResetsOnDifferentCallOrSuccess(t *testing.T) {
	f := faux.New(faux.Options{})
	stuck := RepeatedFailureLimit - 1
	var args []string
	args = append(args, repeatArgs(`{"id":"x"}`, stuck)...)
	args = append(args, `{"id":"y"}`)
	args = append(args, repeatArgs(`{"id":"x"}`, stuck)...)
	args = append(args, `{"id":"x","ok":true}`)
	args = append(args, repeatArgs(`{"id":"x"}`, stuck)...)
	f.SetResponses(append(probeSteps(args...), faux.Respond(faux.Text("done")))...)
	a := newBudgetAgent(f, Budget{MaxTurns: 40}, probeTool())

	result, err := a.PromptText(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != StopCompleted || result.Turns != len(args)+1 {
		t.Fatalf("result = %+v, want completed: no streak reached %d", result, RepeatedFailureLimit)
	}
	results := toolResults(a.State().Messages)
	for _, index := range []int{stuck, stuck + 1, 2*stuck + 2} {
		if len(results[index].Content) != 1 {
			t.Fatalf("result %d = %#v, want no notice right after a reset", index, results[index].Content)
		}
	}
	if success := results[2*stuck+1]; success.IsError || len(success.Content) != 1 || success.Content[0].Text != "found" {
		t.Fatalf("success = %#v, want it unchanged", success)
	}
	if last := results[len(results)-1]; !hasNotice(last, stuck) {
		t.Fatalf("last failure = %#v, want the notice naming %d", last.Content, stuck)
	}
}

func TestRepeatedFailureCountSurvivesResume(t *testing.T) {
	first := faux.New(faux.Options{})
	before := RepeatedFailureLimit - 2
	first.SetResponses(probeSteps(repeatArgs(`{"id":"x"}`, before)...)...)
	a := newBudgetAgent(first, Budget{MaxTurns: before}, probeTool())
	result, err := a.PromptText(context.Background(), "go")
	if err != nil || result.StopReason != StopMaxTurns {
		t.Fatalf("first run = %+v %v, want max_turns", result, err)
	}

	// Resume from the transcript as the journal stores it.
	var transcript []types.Message
	for _, message := range a.State().Messages {
		raw, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := types.UnmarshalMessage(raw)
		if err != nil {
			t.Fatal(err)
		}
		transcript = append(transcript, decoded)
	}
	second := faux.New(faux.Options{})
	second.SetResponses(append(probeSteps(repeatArgs(`{"id":"x"}`, RepeatedFailureLimit)...), faux.Respond(faux.Text("never")))...)
	resumed := newRuntimeAgent(second, func(opts *AgentOptions) {
		opts.InitialState.Tools = []AgentTool{probeTool()}
		opts.InitialState.Messages = transcript
	})
	result, err = resumed.Continue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := RepeatedFailureLimit - before; result.StopReason != StopRepeatedFailure || result.Turns != want {
		t.Fatalf("resumed run = %+v, want repeated_failure after %d more calls", result, want)
	}
}
