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
// like a command that prints the same thing each time. It is not marked as
// a workspace read, so the guard treats it as a command, MCP or extension
// tool would be treated.
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

// readTool is lookTool marked as a workspace read, like reading a file that
// does not change.
func readTool(name string) AgentTool {
	tool := lookTool(name)
	tool.ReadsWorkspace = func(json.RawMessage) bool { return true }
	return tool
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

// cycle repeats the calls of one round count times, in order.
func cycle(count int, round func(index int) []call) []call {
	var calls []call
	for index := range count {
		calls = append(calls, round(index)...)
	}
	return calls
}

// loopAgent runs tools under thresholds with a generous turn cap and records
// the loop_guard events.
func loopAgent(f *faux.Faux, thresholds *LoopThresholds, tools ...AgentTool) (*Agent, *[]LoopDetection) {
	return loopAgentWith(f, func(opts *AgentOptions) {
		opts.InitialState.Tools = tools
		opts.LoopThresholds = thresholds
	})
}

func loopAgentWith(f *faux.Faux, mutate func(*AgentOptions)) (*Agent, *[]LoopDetection) {
	a := newRuntimeAgent(f, func(opts *AgentOptions) {
		opts.Budget = Budget{MaxTurns: 60}
		mutate(opts)
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

// runCalls runs one prompt whose model makes calls, one per reply, then
// answers "done".
func runCalls(t *testing.T, thresholds *LoopThresholds, calls []call, tools ...AgentTool) (*Agent, RunResult, []LoopDetection) {
	t.Helper()
	f := faux.New(faux.Options{})
	f.SetResponses(append(callSteps(calls...), faux.Respond(faux.Text("done")))...)
	a, detections := loopAgent(f, thresholds, tools...)
	result, err := a.PromptText(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	return a, result, *detections
}

// wantNoLoop fails unless the run completed with no loop notice.
func wantNoLoop(t *testing.T, a *Agent, result RunResult, detections []LoopDetection) {
	t.Helper()
	if result.StopReason != StopCompleted || len(detections) != 0 {
		t.Fatalf("result = %s after %d turns, loop_guard events = %+v; want completed with none", result.StopReason, result.Turns, detections)
	}
	wantNotices(t, a.State().Messages, nil)
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
	// Key order and whitespace differ; the call is the same.
	same := []call{{"look", `{"path":"a"}`}, {"look", `{ "path" : "a" }`}, {"look", `{"path":"a"}`}, {"look", `{"path":"a"}`}, {"look", `{"path": "a"}`}, {"look", `{"path":"a"}`}}
	a, result, detections := runCalls(t, &LoopThresholds{RepeatedResult: 3}, same, readTool("look"))
	// The third identical read is nudged; the calls before the nudge are not
	// counted again, so the stop comes when three more repeat it.
	wantLoopStop(t, result, LoopRepeatedResult, 3, "look")
	if result.Turns != 6 {
		t.Fatalf("turns = %d, want 6", result.Turns)
	}
	wantNotices(t, a.State().Messages, map[int]string{2: loopNudgePrefix, 5: loopStopPrefix})
	if lastRole(t, a) != "toolResult" {
		t.Fatalf("last role %s, want the stopping turn's result last", lastRole(t, a))
	}
	if len(detections) != 2 || detections[0].Action != LoopNudge || detections[1].Action != LoopStop || detections[0].Pattern != LoopRepeatedResult {
		t.Fatalf("loop_guard events = %+v, want a nudge then a stop", detections)
	}
	if !strings.Contains(result.Err.Error(), `look {"path":"a"}`) {
		t.Fatalf("run error %q does not name the call", result.Err)
	}
}

// TestLoopGuardRereadAfterAnEditIsProgress: reading the same unchanged file
// three times is nudged only when nothing changed in between; an edit in
// between is progress.
func TestLoopGuardRereadAfterAnEditIsProgress(t *testing.T) {
	read := call{"read", `{"path":"config.yaml"}`}
	calls := cycle(8, func(index int) []call {
		return []call{read, {"edit", fmt.Sprintf(`{"revision":%d}`, index)}}
	})
	a, result, detections := runCalls(t, nil, calls, readTool("read"), counterTool("edit"))
	wantNoLoop(t, a, result, detections)
}

// TestLoopGuardProductiveEditTestCycles: the review's productive traces.
// An edit followed by the same test command, whose output changes or stays
// the same, and two stateful tools whose answers keep changing, are work,
// not loops, under the default thresholds.
func TestLoopGuardProductiveEditTestCycles(t *testing.T) {
	test := call{"bash", `{"command":"go test ./pkg"}`}
	editThenTest := func(cycles int) []call {
		return cycle(cycles, func(index int) []call {
			return []call{{"edit", fmt.Sprintf(`{"revision":%d}`, index)}, test}
		})
	}
	t.Run("changing test output", func(t *testing.T) {
		a, result, detections := runCalls(t, nil, editThenTest(11), counterTool("edit"), counterTool("bash"))
		wantNoLoop(t, a, result, detections)
	})
	t.Run("same test output", func(t *testing.T) {
		a, result, detections := runCalls(t, nil, editThenTest(12), counterTool("edit"), lookTool("bash"))
		wantNoLoop(t, a, result, detections)
	})
	t.Run("alternating stateful tools", func(t *testing.T) {
		calls := cycle(7, func(int) []call { return []call{{"next_page", `{}`}, {"process_page", `{}`}} })
		a, result, detections := runCalls(t, nil, calls, counterTool("next_page"), counterTool("process_page"))
		wantNoLoop(t, a, result, detections)
	})
}

// TestLoopGuardLetsCommandsPoll: a command whose answer has not changed yet
// may be polling for a change, so the default guard allows nine unchanged
// answers, nudges on the tenth and stops after ten more.
func TestLoopGuardLetsCommandsPoll(t *testing.T) {
	health := call{"bash", `{"command":"curl -s localhost:8080/health"}`}
	t.Run("seven probes", func(t *testing.T) {
		a, result, detections := runCalls(t, nil, repeatCall(health, 7), lookTool("bash"))
		wantNoLoop(t, a, result, detections)
	})
	t.Run("probes and sleeps", func(t *testing.T) {
		calls := cycle(9, func(int) []call { return []call{health, {"bash", `{"command":"sleep 2"}`}} })
		a, result, detections := runCalls(t, nil, calls, lookTool("bash"))
		wantNoLoop(t, a, result, detections)
	})
	t.Run("bound", func(t *testing.T) {
		a, result, detections := runCalls(t, nil, repeatCall(health, 25), lookTool("bash"))
		wantLoopStop(t, result, LoopRepeatedPoll, 10, "bash")
		wantNotices(t, a.State().Messages, map[int]string{9: loopNudgePrefix, 19: loopStopPrefix})
		if detections[0].Pattern != LoopRepeatedPoll {
			t.Fatalf("loop_guard events = %+v", detections)
		}
		nudge := toolResults(a.State().Messages)[9].Content
		if text := nudge[len(nudge)-1].Text; !strings.Contains(text, "until") || !strings.Contains(text, "wait tool") {
			t.Fatalf("poll nudge %q does not say how to wait", text)
		}
	})
	t.Run("an answer that changes", func(t *testing.T) {
		var probes atomic.Int64
		// Starting, starting, ..., then ready: each change is progress.
		probe := executorTool("bash", `{"type":"object"}`, ExecParallel, func(context.Context, string, json.RawMessage, AgentToolUpdateCallback) (AgentToolResult, error) {
			return AgentToolResult{Content: []types.ContentBlock{types.NewText(fmt.Sprintf("starting, %d of 3 services up", probes.Add(1)/8))}}, nil
		})
		a, result, detections := runCalls(t, nil, repeatCall(health, 24), probe)
		wantNoLoop(t, a, result, detections)
	})
}

// TestLoopGuardAlternatingRepeats: A-B-A-B with both legs answering the same
// each time is a loop. The nudge about one leg covers the other leg, which
// was repeating with it, so the stop comes on whichever leg repeats next.
func TestLoopGuardAlternatingRepeats(t *testing.T) {
	calls := cycle(6, func(int) []call { return []call{{"look", `{"path":"a"}`}, {"look", `{"path":"b"}`}} })
	a, result, detections := runCalls(t, nil, calls, readTool("look"))
	wantLoopStop(t, result, LoopRepeatedResult, 3, "look")
	wantNotices(t, a.State().Messages, map[int]string{4: loopNudgePrefix, 9: loopStopPrefix})
	if len(detections) != 2 {
		t.Fatalf("loop_guard events = %+v, want a nudge then a stop", detections)
	}
}

// TestLoopGuardStopsOnlyAfterThatPatternsNotice: a nudge about a polling
// command does not make a later repeated read a stop; the read gets its own
// notice first.
func TestLoopGuardStopsOnlyAfterThatPatternsNotice(t *testing.T) {
	probe, read := call{"bash", `{"command":"status"}`}, call{"read", `{"path":"a"}`}
	calls := append(repeatCall(probe, 3), repeatCall(read, 6)...)
	a, result, detections := runCalls(t, &LoopThresholds{RepeatedResult: 3, RepeatedPoll: 3}, calls, lookTool("bash"), readTool("read"))
	wantLoopStop(t, result, LoopRepeatedResult, 3, "read")
	wantNotices(t, a.State().Messages, map[int]string{2: loopNudgePrefix, 5: loopNudgePrefix, 8: loopStopPrefix})
	if len(detections) != 3 || detections[0].Pattern != LoopRepeatedPoll || detections[1].Pattern != LoopRepeatedResult || detections[1].Action != LoopNudge {
		t.Fatalf("loop_guard events = %+v", detections)
	}

	// The review's trace: a file read three times unchanged, then read
	// again while it changes. The changing reads are not repeats, so the
	// nudge is never followed by a stop.
	var reads atomic.Int64
	changing := executorTool("read", `{"type":"object"}`, ExecParallel, func(context.Context, string, json.RawMessage, AgentToolUpdateCallback) (AgentToolResult, error) {
		text := "unchanged initial contents"
		if n := reads.Add(1); n > 3 {
			text = fmt.Sprintf("new file revision %d", n)
		}
		return AgentToolResult{Content: []types.ContentBlock{types.NewText(text)}}, nil
	})
	changing.ReadsWorkspace = func(json.RawMessage) bool { return true }
	a, result, detections = runCalls(t, nil, repeatCall(call{"read", `{}`}, 9), changing)
	if result.StopReason != StopCompleted || len(detections) != 1 || detections[0].Pattern != LoopRepeatedResult {
		t.Fatalf("result = %s, loop_guard events = %+v; want completed after one nudge", result.StopReason, detections)
	}
}

// TestLoopGuardRepeatedFailureBetweenOtherCalls: the same command failing
// the same way, with only reads of other files between, is a loop even
// though the failure streak resets on each read.
func TestLoopGuardRepeatedFailureBetweenOtherCalls(t *testing.T) {
	calls := cycle(6, func(index int) []call {
		return []call{{"probe", `{"id":"x"}`}, {"read", fmt.Sprintf(`{"path":"file-%d"}`, index)}}
	})
	a, result, _ := runCalls(t, nil, calls, probeTool(), readTool("read"))
	wantLoopStop(t, result, LoopRepeatedResult, 3, "probe")
	wantNotices(t, a.State().Messages, map[int]string{4: loopNudgePrefix, 10: loopStopPrefix})
}

// TestLoopGuardCountsRepeatsCloseTogether: a call made again within ten
// calls of its last run continues its count; one made after more than ten
// other calls starts over.
func TestLoopGuardCountsRepeatsCloseTogether(t *testing.T) {
	same := call{"read", `{"path":"a"}`}
	for _, others := range []int{9, 10} {
		var calls []call
		next := 0
		for round := range 3 {
			calls = append(calls, same)
			for range others * min(1, 2-round) {
				calls = append(calls, call{"read", fmt.Sprintf(`{"path":"f%d"}`, next)})
				next++
			}
		}
		a, result, _ := runCalls(t, nil, calls, readTool("read"))
		if result.StopReason != StopCompleted {
			t.Fatalf("%d others: result = %+v, want completed", others, result)
		}
		want := map[int]string{}
		if others == 9 {
			want[2*others+2] = loopNudgePrefix
		}
		wantNotices(t, a.State().Messages, want)
	}
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
func (yieldExtension) Yield(context.Context) (string, error)                       { return "the tests still fail", nil }
func (yieldExtension) BeforeCompaction(context.Context) error                      { return nil }

// textRoles names each transcript message, telling the guard's notices,
// hook feedback and reminders apart from other user messages.
func textRoles(messages []types.Message) string {
	var roles []string
	for _, message := range messages {
		text := ""
		user, isUser := message.(types.UserMessage)
		if isUser {
			text = user.Content.Text
		}
		switch {
		case isUser && user.Reminder != "":
			roles = append(roles, "reminder")
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
	return strings.Join(roles, " ")
}

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
	want := "user assistant feedback assistant feedback assistant feedback nudge assistant feedback assistant feedback assistant feedback stop"
	if got := textRoles(a.State().Messages); got != want {
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

// TestLoopGuardSeesThroughReminders: reminders restate instructions; they
// are not new input. A request-scope reminder never reaches the transcript,
// and a conversation reminder in it neither ends a loop episode nor counts
// as a reply, so both loops are nudged and stopped exactly as without them.
func TestLoopGuardSeesThroughReminders(t *testing.T) {
	for _, scope := range []ReminderScope{ReminderRequest, ReminderConversation} {
		t.Run(string(scope), func(t *testing.T) {
			reminders := []Reminder{{Name: "rule", Text: "Keep the tests green.", Scope: scope, MaxFires: 100}}
			read := call{"look", `{"path":"a"}`}
			f := faux.New(faux.Options{})
			f.SetResponses(append(callSteps(repeatCall(read, 6)...), faux.Respond(faux.Text("never")))...)
			a, _ := loopAgentWith(f, func(opts *AgentOptions) {
				opts.InitialState.Tools = []AgentTool{readTool("look")}
				opts.Reminders = reminders
			})
			result, err := a.PromptText(context.Background(), "go")
			if err != nil {
				t.Fatal(err)
			}
			wantLoopStop(t, result, LoopRepeatedResult, 3, "look")
			wantNotices(t, a.State().Messages, map[int]string{2: loopNudgePrefix, 5: loopStopPrefix})

			f = faux.New(faux.Options{})
			for range 7 {
				f.AppendResponses(faux.Respond(faux.Text("I am done.")))
			}
			a = newRuntimeAgent(f, func(opts *AgentOptions) {
				opts.Extensions = yieldExtension{}
				opts.Reminders = reminders
			})
			if result, err = a.PromptText(context.Background(), "go"); err != nil {
				t.Fatal(err)
			}
			wantLoopStop(t, result, LoopRepeatedText, 3)
			if result.Turns != 6 {
				t.Fatalf("turns = %d, want 6", result.Turns)
			}
			if scope == ReminderConversation && !strings.Contains(textRoles(a.State().Messages), "reminder assistant") {
				t.Fatalf("transcript = %s, want conversation reminders in it", textRoles(a.State().Messages))
			}

			// The failure streak runs on through reminders too.
			f = faux.New(faux.Options{})
			f.SetResponses(append(probeSteps(repeatArgs(`{"id":"x"}`, RepeatedFailureLimit)...), faux.Respond(faux.Text("never")))...)
			a, _ = loopAgentWith(f, func(opts *AgentOptions) {
				opts.InitialState.Tools = []AgentTool{probeTool()}
				opts.Reminders = reminders
			})
			if result, err = a.PromptText(context.Background(), "go"); err != nil || result.StopReason != StopRepeatedFailure || result.Turns != RepeatedFailureLimit {
				t.Fatalf("failing run = %+v %v, want repeated_failure after %d calls", result, err, RepeatedFailureLimit)
			}
		})
	}
}

func TestLoopGuardExemptsPollingTools(t *testing.T) {
	poll := lookTool("wait")
	poll.Polls = true
	calls := repeatCall(call{"wait", `{"path":"job-1"}`}, 25)
	a, result, detections := runCalls(t, nil, calls, poll)
	wantNoLoop(t, a, result, detections)
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
// result, and the results the failure guard already noted get no second
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

// TestLoopGuardStopsMixedBatches: the review's batch of one unchanged
// successful call and two identical failures, sent again and again. The
// success resets the failure streak and the failure notice on one result no
// longer hides the batch from the loop guard, so the repeated failure is
// nudged on the third batch and stopped on the sixth, with both notices on
// the last result.
func TestLoopGuardStopsMixedBatches(t *testing.T) {
	f := faux.New(faux.Options{})
	for index := range 12 {
		f.AppendResponses(faux.Respond(
			faux.ToolCall("look", json.RawMessage(`{"path":"same"}`), fmt.Sprintf("look-%d", index)),
			faux.ToolCall("probe", json.RawMessage(`{"id":"missing"}`), fmt.Sprintf("probe-%d-a", index)),
			faux.ToolCall("probe", json.RawMessage(`{"id":"missing"}`), fmt.Sprintf("probe-%d-b", index)),
		))
	}
	f.AppendResponses(faux.Respond(faux.Text("done")))
	a, detections := loopAgent(f, nil, lookTool("look"), probeTool())
	result, err := a.PromptText(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	wantLoopStop(t, result, LoopRepeatedResult, 3, "probe")
	if result.Turns != 6 || len(*detections) != 2 {
		t.Fatalf("turns = %d, loop_guard events = %+v; want a nudge on turn 3 and a stop on turn 6", result.Turns, *detections)
	}
	results := toolResults(a.State().Messages)
	nudged := results[8].Content
	if !hasHostNotice(nudged, repeatedFailurePrefix) || hostNotice(nudged) != loopNudgePrefix {
		t.Fatalf("third batch's last result = %+v, want the failure notice then the loop notice", nudged)
	}
	if got := resultContent(nudged); len(got) != 1 || !strings.Contains(got[0].Text, "no job") {
		t.Fatalf("content without notices = %+v", got)
	}
}

// TestLoopGuardCountsBatchesAndNoticesTheLastResult: identical calls in one
// reply count once, since the model had seen none of their results, and the
// notice goes on the last result of the batch that completes the loop.
func TestLoopGuardCountsBatchesAndNoticesTheLastResult(t *testing.T) {
	f := faux.New(faux.Options{})
	same := json.RawMessage(`{"path":"a"}`)
	f.SetResponses(
		faux.Respond(faux.ToolCall("look", same, "a1"), faux.ToolCall("look", same, "a2"), faux.ToolCall("look", same, "a3")),
		faux.Respond(faux.ToolCall("look", same, "a4")),
		faux.Respond(faux.ToolCall("look", same, "a5"), faux.ToolCall("look", json.RawMessage(`{"path":"b"}`), "b1")),
		faux.Respond(faux.Text("done")),
	)
	a, detections := loopAgent(f, &LoopThresholds{RepeatedResult: 3}, readTool("look"))

	result, err := a.PromptText(context.Background(), "go")
	if err != nil || result.StopReason != StopCompleted {
		t.Fatalf("result = %+v %v, want completed", result, err)
	}
	wantNotices(t, a.State().Messages, map[int]string{5: loopNudgePrefix})
	if got := *detections; len(got) != 1 || got[0].Pattern != LoopRepeatedResult || got[0].Tools[0] != "look" || got[0].Count != 3 {
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
		opts.InitialState.Tools = []AgentTool{readTool("look")}
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
	b := resumed(t, a, second, thresholds, readTool("look"))
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
	c := resumed(t, b, third, thresholds, readTool("look"))
	result, err = c.Continue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantLoopStop(t, result, LoopRepeatedResult, 4, "look")
	if result.Turns != 1 {
		t.Fatalf("restarted turns = %d, want 1", result.Turns)
	}

	// A model that moved on is not stopped by the loop it left.
	fourth := faux.New(faux.Options{})
	fourth.SetResponses(append(callSteps(call{"look", `{"path":"b"}`}), faux.Respond(faux.Text("done")))...)
	d := resumed(t, c, fourth, thresholds, readTool("look"))
	if result, err = d.Continue(context.Background()); err != nil || result.StopReason != StopCompleted {
		t.Fatalf("moved-on run = %+v %v, want completed", result, err)
	}
}

// TestLoopGuardProgressForgivesANudge: after a nudge, progress (an edit)
// means the next report of the same loop is a nudge again, not a stop.
func TestLoopGuardProgressForgivesANudge(t *testing.T) {
	read := call{"read", `{"path":"a"}`}
	calls := append(repeatCall(read, 3), call{"edit", `{"revision":1}`})
	calls = append(calls, repeatCall(read, 3)...)
	a, result, detections := runCalls(t, nil, calls, readTool("read"), counterTool("edit"))
	if result.StopReason != StopCompleted || len(detections) != 2 || detections[1].Action != LoopNudge {
		t.Fatalf("result = %s, loop_guard events = %+v; want completed after two nudges", result.StopReason, detections)
	}
	// The read just after the edit is the first of the new count.
	wantNotices(t, a.State().Messages, map[int]string{2: loopNudgePrefix, 6: loopNudgePrefix})
}

// TestLoopGuardUnchangedCommandStartsReadsOver: the review's productive
// trace. A command that appends to a file and prints nothing answers the
// same every time, yet each run changed the workspace, so the reference file
// read after it is not a repeated read. The command's own unchanged answers
// count toward the polling bound only: twelve cycles get one notice, on the
// tenth run, and complete.
func TestLoopGuardUnchangedCommandStartsReadsOver(t *testing.T) {
	appendItem := call{"bash", `{"command":"append next item"}`}
	reference := call{"read", `{"path":"reference.txt"}`}
	t.Run("productive cycles", func(t *testing.T) {
		calls := cycle(12, func(int) []call { return []call{appendItem, reference} })
		a, result, detections := runCalls(t, nil, calls, lookTool("bash"), readTool("read"))
		if result.StopReason != StopCompleted || len(detections) != 1 || detections[0].Pattern != LoopRepeatedPoll || detections[0].Tools[0] != "bash" {
			t.Fatalf("result = %s, loop_guard events = %+v; want completed after one repeated_poll notice on bash", result.StopReason, detections)
		}
		wantNotices(t, a.State().Messages, map[int]string{18: loopNudgePrefix})
		nudge := toolResults(a.State().Messages)[18].Content
		if text := nudge[len(nudge)-1].Text; !strings.Contains(text, "you may go on") || !strings.Contains(text, "10 more unchanged results stop the run") {
			t.Fatalf("poll nudge %q does not let the model go on when it is making progress", text)
		}
	})
	t.Run("bounded by polling", func(t *testing.T) {
		calls := cycle(25, func(int) []call { return []call{appendItem, reference} })
		a, result, _ := runCalls(t, nil, calls, lookTool("bash"), readTool("read"))
		wantLoopStop(t, result, LoopRepeatedPoll, 10, "bash")
		wantNotices(t, a.State().Messages, map[int]string{18: loopNudgePrefix, 38: loopStopPrefix})
	})
	t.Run("forgives a read notice", func(t *testing.T) {
		// The second stretch of three reads follows a run of the command
		// that may have changed the file, so it is nudged again, not stopped.
		calls := append([]call{appendItem}, repeatCall(reference, 3)...)
		calls = append(append(calls, appendItem), repeatCall(reference, 3)...)
		a, result, detections := runCalls(t, nil, calls, lookTool("bash"), readTool("read"))
		if result.StopReason != StopCompleted || len(detections) != 2 || detections[1].Action != LoopNudge {
			t.Fatalf("result = %s, loop_guard events = %+v; want completed after two nudges", result.StopReason, detections)
		}
		wantNotices(t, a.State().Messages, map[int]string{3: loopNudgePrefix, 7: loopNudgePrefix})
	})
	t.Run("reads with nothing between", func(t *testing.T) {
		calls := append([]call{appendItem}, repeatCall(reference, 6)...)
		a, result, _ := runCalls(t, nil, calls, lookTool("bash"), readTool("read"))
		wantLoopStop(t, result, LoopRepeatedResult, 3, "read")
		wantNotices(t, a.State().Messages, map[int]string{3: loopNudgePrefix, 6: loopStopPrefix})
	})
}

// TestLoopGuardUnchangedCommandKeepsFailedReads: an unchanged command
// forgives a workspace read's unchanged answer, not its unchanged failure. An
// identical failure between runs of a stable command is nudged on the third
// failure and stopped on the sixth, whether or not the failing tool reads the
// workspace.
func TestLoopGuardUnchangedCommandKeepsFailedReads(t *testing.T) {
	for _, reads := range []bool{false, true} {
		t.Run(fmt.Sprintf("reads workspace %v", reads), func(t *testing.T) {
			failing := executorTool("read", `{"type":"object"}`, ExecParallel, func(context.Context, string, json.RawMessage, AgentToolUpdateCallback) (AgentToolResult, error) {
				return AgentToolResult{}, errors.New("no such file")
			})
			if reads {
				failing.ReadsWorkspace = func(json.RawMessage) bool { return true }
			}
			calls := cycle(12, func(int) []call { return []call{{"bash", `{"command":"status"}`}, {"read", `{"path":"missing.txt"}`}} })
			a, result, _ := runCalls(t, nil, calls, lookTool("bash"), failing)
			wantLoopStop(t, result, LoopRepeatedResult, 3, "read")
			wantNotices(t, a.State().Messages, map[int]string{5: loopNudgePrefix, 11: loopStopPrefix})
		})
	}
}

// TestLoopGuardStopsUndoRedo: two edits undoing each other answer the same
// each time they are redone (an edit's result is its diff and the written
// file's content hash), so each leg repeats and the polling bound nudges,
// then stops.
func TestLoopGuardStopsUndoRedo(t *testing.T) {
	calls := cycle(22, func(int) []call { return []call{{"edit", `{"path":"on"}`}, {"edit", `{"path":"off"}`}} })
	a, result, detections := runCalls(t, nil, calls, lookTool("edit"))
	wantLoopStop(t, result, LoopRepeatedPoll, 10, "edit")
	if len(detections) != 2 || detections[0].Action != LoopNudge {
		t.Fatalf("loop_guard events = %+v, want a nudge then a stop", detections)
	}
	if got := notices(a.State().Messages); got[len(got)-1] != loopStopPrefix {
		t.Fatalf("notices = %q, want the stop on the last result", got)
	}
}

// TestLoopGuardObservesEverySiblingResult: identical calls in one reply
// count once, but each result is observed. A changed later result is new, so
// a stateful tool whose second answer in each reply is fresh makes progress;
// identical results still count once per reply.
func TestLoopGuardObservesEverySiblingResult(t *testing.T) {
	pairs := func(t *testing.T, replies int, second func(index int64) string) (RunResult, []LoopDetection) {
		t.Helper()
		var runs atomic.Int64
		tool := executorTool("stateful", `{"type":"object"}`, ExecSequential, func(context.Context, string, json.RawMessage, AgentToolUpdateCallback) (AgentToolResult, error) {
			text := "stable"
			if n := runs.Add(1); n%2 == 0 {
				text = second(n)
			}
			return AgentToolResult{Content: []types.ContentBlock{types.NewText(text)}}, nil
		})
		f := faux.New(faux.Options{})
		for index := range replies {
			f.AppendResponses(faux.Respond(
				faux.ToolCall("stateful", json.RawMessage(`{}`), fmt.Sprintf("a%d", index)),
				faux.ToolCall("stateful", json.RawMessage(`{}`), fmt.Sprintf("b%d", index)),
			))
		}
		f.AppendResponses(faux.Respond(faux.Text("done")))
		a, detections := loopAgent(f, nil, tool)
		result, err := a.PromptText(context.Background(), "consume the next two results each time")
		if err != nil {
			t.Fatal(err)
		}
		return result, *detections
	}
	t.Run("changed later result", func(t *testing.T) {
		result, detections := pairs(t, 24, func(n int64) string { return fmt.Sprintf("fresh result %d", n) })
		if result.StopReason != StopCompleted || len(detections) != 0 {
			t.Fatalf("result = %s after %d turns, loop_guard events = %+v; want completed with none", result.StopReason, result.Turns, detections)
		}
	})
	t.Run("identical results", func(t *testing.T) {
		result, detections := pairs(t, 24, func(int64) string { return "stable" })
		wantLoopStop(t, result, LoopRepeatedPoll, 10, "stateful")
		if result.Turns != 20 || len(detections) != 2 {
			t.Fatalf("turns = %d, loop_guard events = %+v; want a nudge on reply 10 and a stop on reply 20", result.Turns, detections)
		}
	})
}

func TestLoopGuardNewPromptStartsOver(t *testing.T) {
	thresholds := &LoopThresholds{RepeatedResult: 3}
	same := call{"look", `{"path":"a"}`}
	f := faux.New(faux.Options{})
	f.SetResponses(append(callSteps(repeatCall(same, 3)...), faux.Respond(faux.Text("ok")))...)
	a, _ := loopAgent(f, thresholds, readTool("look"))
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
	if err := (LoopThresholds{RepeatedPoll: 1}).Validate(); err == nil || !strings.Contains(err.Error(), "repeated_poll") {
		t.Fatalf("threshold 1 = %v, want an error naming repeated_poll", err)
	}
	a := newRuntimeAgent(faux.New(faux.Options{}), func(opts *AgentOptions) { opts.LoopThresholds = &LoopThresholds{RepeatedResult: -1} })
	if _, err := a.PromptText(context.Background(), "go"); err == nil {
		t.Fatal("run started with an invalid threshold")
	}
}
