package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
)

// lookTool succeeds with a result that depends only on its path argument,
// like reading a file that does not change.
func lookTool(name string) AgentTool {
	return executorTool(name, `{"type":"object","properties":{"path":{"type":"string"}}}`, ExecParallel, func(_ context.Context, _ string, params json.RawMessage, _ AgentToolUpdateCallback) (AgentToolResult, error) {
		var input struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(params, &input); err != nil {
			return AgentToolResult{}, err
		}
		return AgentToolResult{Content: []types.ContentBlock{types.NewText("contents of " + input.Path)}}, nil
	})
}

// counterTool succeeds with a different result on every call, so the same
// call never returns the same result.
func counterTool(name string) AgentTool {
	var calls atomic.Int64
	return executorTool(name, `{"type":"object"}`, ExecParallel, func(context.Context, string, json.RawMessage, AgentToolUpdateCallback) (AgentToolResult, error) {
		return AgentToolResult{Content: []types.ContentBlock{types.NewText(fmt.Sprintf("tick %d", calls.Add(1)))}}, nil
	})
}

// call is one tool call of a faux reply: tool name and JSON arguments.
type call struct{ tool, args string }

// callSteps scripts one reply per call, each with that single tool call.
func callSteps(calls ...call) []faux.ResponseStep {
	steps := make([]faux.ResponseStep, 0, len(calls))
	for index, c := range calls {
		steps = append(steps, faux.Respond(faux.ToolCall(c.tool, json.RawMessage(c.args), fmt.Sprintf("call-%d", index))))
	}
	return steps
}

func repeatCall(c call, count int) []call {
	calls := make([]call, count)
	for index := range calls {
		calls[index] = c
	}
	return calls
}

// loopAgent runs tools under thresholds with a generous turn cap and records
// the loop_guard events.
func loopAgent(f *faux.Faux, thresholds *LoopThresholds, tools ...AgentTool) (*Agent, *[]LoopDetection) {
	a := newRuntimeAgent(f, func(opts *AgentOptions) {
		opts.InitialState.Tools = tools
		opts.Budget = Budget{MaxTurns: 60}
		opts.LoopThresholds = thresholds
	})
	var detections []LoopDetection
	a.Subscribe(func(_ context.Context, event AgentEvent) error {
		if event.Type == EventLoopGuard {
			detections = append(detections, *event.Loop)
		}
		return nil
	})
	return a, &detections
}

// notices returns, per tool result, the host notice prefix it ends with ("" for none).
func notices(messages []types.Message) []string {
	var out []string
	for _, result := range toolResults(messages) {
		out = append(out, hostNotice(result.Content))
	}
	return out
}

// wantNotices fails unless exactly the results at the given indexes carry
// the given prefixes and every other result carries none.
func wantNotices(t *testing.T, messages []types.Message, want map[int]string) {
	t.Helper()
	for index, got := range notices(messages) {
		if got != want[index] {
			t.Fatalf("result %d notice = %q, want %q (all: %q)", index, got, want[index], notices(messages))
		}
	}
}

func wantLoopStop(t *testing.T, result RunResult, pattern LoopPattern, count int, tools ...string) {
	t.Helper()
	var loop *LoopError
	if result.StopReason != StopLoopDetected || !errors.As(result.Err, &loop) {
		t.Fatalf("result = %+v, want loop_detected with a *LoopError", result)
	}
	if loop.Pattern != pattern || loop.Count != count || loop.Action != LoopStop || strings.Join(loop.Tools, ",") != strings.Join(tools, ",") {
		t.Fatalf("loop = %+v, want %s x%d on %v", loop.LoopDetection, pattern, count, tools)
	}
}

func TestLoopGuardRepeatedResultNudgesThenStops(t *testing.T) {
	f := faux.New(faux.Options{})
	// Key order and whitespace differ; the call is the same.
	same := []call{{"look", `{"path":"a"}`}, {"look", `{ "path" : "a" }`}, {"look", `{"path":"a"}`}, {"look", `{"path":"a"}`}, {"look", `{"path": "a"}`}, {"look", `{"path":"a"}`}}
	f.SetResponses(append(callSteps(same...), faux.Respond(faux.Text("never")))...)
	a, detections := loopAgent(f, &LoopThresholds{RepeatedResult: 3}, lookTool("look"))

	result, err := a.PromptText(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	// The third identical result is nudged; the calls before the nudge are
	// not counted again, so the stop comes when three more repeat it.
	wantLoopStop(t, result, LoopRepeatedResult, 3, "look")
	if result.Turns != 6 || f.CallCount() != 6 {
		t.Fatalf("turns = %d calls = %d, want 6", result.Turns, f.CallCount())
	}
	wantNotices(t, a.State().Messages, map[int]string{2: loopNudgePrefix, 5: loopStopPrefix})
	if lastRole(t, a) != "toolResult" {
		t.Fatalf("last role %s, want the stopping turn's result last", lastRole(t, a))
	}
	if got := *detections; len(got) != 2 || got[0].Action != LoopNudge || got[1].Action != LoopStop || got[0].Pattern != LoopRepeatedResult {
		t.Fatalf("loop_guard events = %+v, want a nudge then a stop", got)
	}
	if !strings.Contains(result.Err.Error(), `look {"path":"a"}`) {
		t.Fatalf("run error %q does not name the call", result.Err)
	}
}

func TestLoopGuardRepeatedCallNudgesThenStops(t *testing.T) {
	f := faux.New(faux.Options{})
	tick := call{"tick", `{}`}
	other := call{"look", `{"path":"x"}`}
	// Four ticks, then a fifth after another call: five within ten calls.
	calls := append(repeatCall(tick, 4), other, tick)
	// After the nudge, five more ticks within the window stop the run.
	calls = append(calls, repeatCall(tick, 5)...)
	f.SetResponses(append(callSteps(calls...), faux.Respond(faux.Text("never")))...)
	a, _ := loopAgent(f, &LoopThresholds{RepeatedCall: 5}, counterTool("tick"), lookTool("look"))

	result, err := a.PromptText(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	wantLoopStop(t, result, LoopRepeatedCall, 5, "tick")
	wantNotices(t, a.State().Messages, map[int]string{5: loopNudgePrefix, 10: loopStopPrefix})
}

func TestLoopGuardRepeatedCallCountsOnlyWithinWindow(t *testing.T) {
	tick := call{"tick", `{}`}
	// Four ticks, some other calls, one more tick. With five other calls
	// the ten-call window still holds all five ticks; with six, the first
	// tick has left it.
	for others, want := range map[int]map[int]string{5: {9: loopNudgePrefix}, 6: {}} {
		calls := repeatCall(tick, 4)
		for index := range others {
			calls = append(calls, call{"look", fmt.Sprintf(`{"path":"f%d"}`, index)})
		}
		calls = append(calls, tick)
		f := faux.New(faux.Options{})
		f.SetResponses(append(callSteps(calls...), faux.Respond(faux.Text("done")))...)
		a, _ := loopAgent(f, &LoopThresholds{RepeatedCall: 5}, counterTool("tick"), lookTool("look"))

		result, err := a.PromptText(context.Background(), "go")
		if err != nil || result.StopReason != StopCompleted {
			t.Fatalf("%d others: result = %+v %v, want completed", others, result, err)
		}
		wantNotices(t, a.State().Messages, want)
	}
}

func TestLoopGuardAlternatingCalls(t *testing.T) {
	f := faux.New(faux.Options{})
	first, second := call{"look", `{"path":"a"}`}, call{"look", `{"path":"b"}`}
	var calls []call
	for range 6 {
		calls = append(calls, first, second)
	}
	f.SetResponses(append(callSteps(calls...), faux.Respond(faux.Text("never")))...)
	a, _ := loopAgent(f, &LoopThresholds{AlternatingCalls: 3}, lookTool("look"))

	result, err := a.PromptText(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	// Two and a half cycles say nothing; the third cycle is nudged and three
	// more cycles after it stop the run.
	wantLoopStop(t, result, LoopAlternatingCalls, 3, "look", "look")
	wantNotices(t, a.State().Messages, map[int]string{5: loopNudgePrefix, 11: loopStopPrefix})
}

// yieldExtension sends a finished model back to work with the same feedback
// every time, like a Stop hook whose check keeps failing.
type yieldExtension struct{}

func (yieldExtension) Prepare(ctx context.Context, current AgentContext) (context.Context, AgentContext, func(), error) {
	return ctx, current, func() {}, nil
}
func (yieldExtension) BeforeTool(context.Context, types.ContentBlock) (json.RawMessage, error) {
	return nil, nil
}
func (yieldExtension) AfterTool(_ context.Context, _ types.ContentBlock, result AgentToolResult) (AgentToolResult, error) {
	return result, nil
}
func (yieldExtension) AfterResponse(context.Context, types.AssistantMessage) error { return nil }
func (yieldExtension) Yield(context.Context) (string, error)                   { return "the tests still fail", nil }
func (yieldExtension) BeforeCompaction(context.Context) error                  { return nil }

func TestLoopGuardRepeatedTextNudgesThenStops(t *testing.T) {
	f := faux.New(faux.Options{})
	// Case and whitespace differ; the text is the same.
	replies := []string{"I am done.", "i am  DONE.", "I am done.", "I am done.", "I am\ndone.", "I am done.", "never"}
	for _, reply := range replies {
		f.AppendResponses(faux.Respond(faux.Text(reply)))
	}
	a := newRuntimeAgent(f, func(opts *AgentOptions) {
		opts.Extensions = yieldExtension{}
		opts.LoopThresholds = &LoopThresholds{RepeatedText: 3}
	})

	result, err := a.PromptText(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	wantLoopStop(t, result, LoopRepeatedText, 3)
	if result.Turns != 6 || f.CallCount() != 6 {
		t.Fatalf("turns = %d calls = %d, want the run to end before a seventh call", result.Turns, f.CallCount())
	}
	var roles []string
	for _, message := range a.State().Messages {
		text := ""
		if user, ok := message.(types.UserMessage); ok {
			text = user.Content.Text
		}
		switch {
		case strings.HasPrefix(text, loopNudgePrefix):
			roles = append(roles, "nudge")
		case strings.HasPrefix(text, loopStopPrefix):
			roles = append(roles, "stop")
		case isHookContinuation(text):
			roles = append(roles, "feedback")
		default:
			roles = append(roles, message.Role())
		}
	}
	want := "user assistant feedback assistant feedback assistant feedback nudge assistant feedback assistant feedback assistant feedback stop"
	if got := strings.Join(roles, " "); got != want {
		t.Fatalf("transcript = %s\nwant %s", got, want)
	}
}

func TestLoopGuardRepeatedTextResetsOnUserMessage(t *testing.T) {
	f := faux.New(faux.Options{})
	for range 4 {
		f.AppendResponses(faux.Respond(faux.Text("Waiting for the other jobs.")))
	}
	a := newRuntimeAgent(f, func(opts *AgentOptions) {
		opts.LoopThresholds = &LoopThresholds{RepeatedText: 2}
	})
	// Each answer is to new input, so the same answer can be right.
	for index := range 4 {
		result, err := a.PromptText(context.Background(), fmt.Sprintf("job %d finished", index))
		if err != nil || result.StopReason != StopCompleted {
			t.Fatalf("prompt %d = %+v %v, want completed", index, result, err)
		}
	}
	for _, message := range a.State().Messages {
		if user, ok := message.(types.UserMessage); ok && strings.HasPrefix(user.Content.Text, "[Loop") {
			t.Fatalf("unexpected notice %q", user.Content.Text)
		}
	}
}

func TestLoopGuardExemptsPollingTools(t *testing.T) {
	f := faux.New(faux.Options{})
	poll := lookTool("wait")
	poll.Polls = true
	calls := repeatCall(call{"wait", `{"path":"job-1"}`}, 8)
	// Polls stay in the sequence: between A and B they break the A-B
	// alternation that A, B, A, B, A, B alone would be.
	for range 3 {
		calls = append(calls, call{"look", `{"path":"a"}`}, call{"wait", `{"path":"job-1"}`}, call{"look", `{"path":"b"}`}, call{"wait", `{"path":"job-1"}`})
	}
	f.SetResponses(append(callSteps(calls...), faux.Respond(faux.Text("done")))...)
	a, detections := loopAgent(f, &LoopThresholds{RepeatedCall: 5, AlternatingCalls: 3}, poll, lookTool("look"))

	result, err := a.PromptText(context.Background(), "go")
	if err != nil || result.StopReason != StopCompleted {
		t.Fatalf("result = %+v %v, want completed: polling is not a loop", result, err)
	}
	if len(*detections) != 0 {
		t.Fatalf("loop_guard events = %+v, want none", *detections)
	}
	wantNotices(t, a.State().Messages, nil)
}

func TestLoopGuardRepeatedFailureStillStopsPollingTool(t *testing.T) {
	f := faux.New(faux.Options{})
	probe := probeTool()
	probe.Polls = true
	f.SetResponses(append(probeSteps(repeatArgs(`{"id":"x"}`, RepeatedFailureLimit)...), faux.Respond(faux.Text("never")))...)
	a, detections := loopAgent(f, nil, probe)

	result, err := a.PromptText(context.Background(), "go")
	if err != nil || result.StopReason != StopRepeatedFailure || result.Turns != RepeatedFailureLimit {
		t.Fatalf("result = %+v %v, want repeated_failure after %d calls", result, err, RepeatedFailureLimit)
	}
	if len(*detections) != 0 {
		t.Fatalf("loop_guard events = %+v, want the failure notice alone", *detections)
	}
}

// TestLoopGuardCountsFailuresAcrossOtherCalls: the failure streak resets on
// another call, but the identical failure coming back after it is a repeated
// result, and the batches the failure guard already noted get no second
// notice.
func TestLoopGuardCountsFailuresAcrossOtherCalls(t *testing.T) {
	f := faux.New(faux.Options{})
	args := append(repeatArgs(`{"id":"x"}`, 4), `{"id":"y"}`, `{"id":"x"}`)
	f.SetResponses(append(probeSteps(args...), faux.Respond(faux.Text("done")))...)
	a, _ := loopAgent(f, nil, probeTool())

	result, err := a.PromptText(context.Background(), "go")
	if err != nil || result.StopReason != StopCompleted {
		t.Fatalf("result = %+v %v, want completed", result, err)
	}
	wantNotices(t, a.State().Messages, map[int]string{1: repeatedFailurePrefix, 2: repeatedFailurePrefix, 3: repeatedFailurePrefix, 5: loopNudgePrefix})
}

func TestLoopGuardNoticeGoesOnLastResultOfBatch(t *testing.T) {
	f := faux.New(faux.Options{})
	same := json.RawMessage(`{"path":"a"}`)
	f.SetResponses(
		faux.Respond(faux.ToolCall("look", same, "a1"), faux.ToolCall("look", same, "a2"), faux.ToolCall("look", same, "a3"), faux.ToolCall("look", json.RawMessage(`{"path":"b"}`), "b1")),
		faux.Respond(faux.Text("done")),
	)
	a, detections := loopAgent(f, &LoopThresholds{RepeatedResult: 3}, lookTool("look"))

	result, err := a.PromptText(context.Background(), "go")
	if err != nil || result.StopReason != StopCompleted {
		t.Fatalf("result = %+v %v, want completed", result, err)
	}
	wantNotices(t, a.State().Messages, map[int]string{3: loopNudgePrefix})
	if got := *detections; len(got) != 1 || got[0].Tools[0] != "look" || got[0].Count != 3 {
		t.Fatalf("loop_guard events = %+v, want one nudge about look x3", got)
	}
}

// resumed rebuilds an agent from a's transcript as the journal stores it.
func resumed(t *testing.T, a *Agent, f *faux.Faux, thresholds *LoopThresholds, tools ...AgentTool) *Agent {
	t.Helper()
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
	return newRuntimeAgent(f, func(opts *AgentOptions) {
		opts.InitialState.Tools = tools
		opts.InitialState.Messages = transcript
		opts.LoopThresholds = thresholds
	})
}

func TestLoopGuardResumeKeepsNudgeAndStopDecisions(t *testing.T) {
	thresholds := &LoopThresholds{RepeatedResult: 3}
	same := call{"look", `{"path":"a"}`}
	first := faux.New(faux.Options{})
	first.SetResponses(callSteps(repeatCall(same, 4)...)...)
	a := newRuntimeAgent(first, func(opts *AgentOptions) {
		opts.InitialState.Tools = []AgentTool{lookTool("look")}
		opts.Budget = Budget{MaxTurns: 4}
		opts.LoopThresholds = thresholds
	})
	result, err := a.PromptText(context.Background(), "go")
	if err != nil || result.StopReason != StopMaxTurns {
		t.Fatalf("first run = %+v %v, want max_turns", result, err)
	}
	wantNotices(t, a.State().Messages, map[int]string{2: loopNudgePrefix})

	// The nudge in the transcript is a boundary and its loop is known, so
	// two more repeats stop the run and no second nudge is given.
	second := faux.New(faux.Options{})
	second.SetResponses(append(callSteps(repeatCall(same, 3)...), faux.Respond(faux.Text("never")))...)
	b := resumed(t, a, second, thresholds, lookTool("look"))
	result, err = b.Continue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantLoopStop(t, result, LoopRepeatedResult, 3, "look")
	if result.Turns != 2 {
		t.Fatalf("resumed turns = %d, want 2", result.Turns)
	}
	wantNotices(t, b.State().Messages, map[int]string{2: loopNudgePrefix, 5: loopStopPrefix})

	// A stop notice is not a boundary: after a restart, one more repeat of
	// the loop stops again at once, without a nudge.
	third := faux.New(faux.Options{})
	third.SetResponses(callSteps(same)...)
	c := resumed(t, b, third, thresholds, lookTool("look"))
	result, err = c.Continue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantLoopStop(t, result, LoopRepeatedResult, 4, "look")
	if result.Turns != 1 {
		t.Fatalf("restarted turns = %d, want 1", result.Turns)
	}

	// A model that moved on is not stopped by the loop still in the window.
	fourth := faux.New(faux.Options{})
	fourth.SetResponses(append(callSteps(call{"look", `{"path":"b"}`}), faux.Respond(faux.Text("done")))...)
	d := resumed(t, c, fourth, thresholds, lookTool("look"))
	if result, err = d.Continue(context.Background()); err != nil || result.StopReason != StopCompleted {
		t.Fatalf("moved-on run = %+v %v, want completed", result, err)
	}
}

func TestLoopGuardNewPromptStartsOver(t *testing.T) {
	thresholds := &LoopThresholds{RepeatedResult: 3}
	same := call{"look", `{"path":"a"}`}
	f := faux.New(faux.Options{})
	f.SetResponses(append(callSteps(repeatCall(same, 3)...), faux.Respond(faux.Text("ok")))...)
	a, _ := loopAgent(f, thresholds, lookTool("look"))
	if result, err := a.PromptText(context.Background(), "go"); err != nil || result.StopReason != StopCompleted {
		t.Fatalf("first prompt = %+v %v", result, err)
	}
	// The user's next prompt starts a new count: the same loop is nudged
	// again on its third repeat, not stopped.
	f.SetResponses(append(callSteps(repeatCall(same, 3)...), faux.Respond(faux.Text("ok")))...)
	if result, err := a.PromptText(context.Background(), "again"); err != nil || result.StopReason != StopCompleted {
		t.Fatalf("second prompt = %+v %v, want completed", result, err)
	}
	wantNotices(t, a.State().Messages, map[int]string{2: loopNudgePrefix, 5: loopNudgePrefix})
}

func TestLoopThresholdsValidate(t *testing.T) {
	if err := DefaultLoopThresholds.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (LoopThresholds{}).Validate(); err != nil {
		t.Fatalf("all off: %v", err)
	}
	if err := (LoopThresholds{RepeatedText: 1}).Validate(); err == nil || !strings.Contains(err.Error(), "repeated_text") {
		t.Fatalf("threshold 1 = %v, want an error naming repeated_text", err)
	}
	a := newRuntimeAgent(faux.New(faux.Options{}), func(opts *AgentOptions) { opts.LoopThresholds = &LoopThresholds{RepeatedCall: -1} })
	if _, err := a.PromptText(context.Background(), "go"); err == nil {
		t.Fatal("run started with an invalid threshold")
	}
}
