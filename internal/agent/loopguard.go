package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"slices"
	"strings"
	"time"

	"go.harness.dev/harness/internal/engine/types"
)

// The loop guard catches a model that keeps doing the same thing while its
// calls succeed, which the repeated-failure guard (repeat.go) cannot see. It
// has four detectors, after Pydantic AI's DetectLoops:
//
//   - repeated call: the same call (tool and canonical arguments) made
//     RepeatedCall times within the window of recent calls;
//   - repeated result: the same call returning the same result (outcome and
//     content) each of its last RepeatedResult times within the window, with
//     no different result in between; other calls in between don't matter;
//   - alternating calls: the last 2 x AlternatingCalls calls alternate
//     strictly between two different calls;
//   - repeated text: RepeatedText consecutive replies without tool calls,
//     separated only by hook feedback that sent the model back to work, whose
//     text is the same ignoring case and whitespace.
//
// A detector reports only a loop the newest batch (or reply) extends, so a
// pattern the model already left does not fire later. The first report of a
// loop is a nudge: a notice appended at the tail of the transcript, to the
// batch's last tool result or as a user message before the next model call.
// The calls before a nudge are then not counted again, so the loop has to
// build up anew. The same loop (the same calls, or the same text) reported
// again after its nudge stops the run with StopLoopDetected.
//
// Like the failure streak, everything is derived from the transcript each
// time, never kept in memory: a nudge is recognized by its prefix, and the
// loop it was about is recomputed from the slice of transcript that produced
// it. A resumed or replayed session therefore reaches the same decisions and
// never repeats a nudge. Counting starts over at any user message (a prompt,
// a steer, a job notice, a compaction summary); Continue adds none, so it
// carries the count on. A stop notice is not a boundary: after a stop, the
// next repetition of the same loop stops the run again at once.

// LoopThresholds configures the loop guard. Each threshold is 0 (that
// detector off) or at least 2.
type LoopThresholds struct {
	// RepeatedCall reports the same call made this many times within the
	// window of recent calls.
	RepeatedCall int
	// RepeatedResult reports the same call returning the same result each of
	// its last this-many times within the window.
	RepeatedResult int
	// AlternatingCalls reports two different calls made in turn (A-B-A-B)
	// for this many A-B cycles.
	AlternatingCalls int
	// RepeatedText reports this many consecutive replies without tool calls
	// whose text is the same.
	RepeatedText int
}

// DefaultLoopThresholds are DetectLoops' defaults. A model calling one tool
// identically five times in ten calls, getting the same answer three times,
// or flipping between two calls three times is not making progress, and the
// first report is only a notice.
var DefaultLoopThresholds = LoopThresholds{RepeatedCall: 5, RepeatedResult: 3, AlternatingCalls: 3, RepeatedText: 3}

// minLoopWindow is how many recent calls the call detectors look at, unless
// a threshold needs more.
const minLoopWindow = 10

// Validate rejects a threshold that is neither 0 nor at least 2: a single
// call or reply is not a repetition.
func (t LoopThresholds) Validate() error {
	for _, threshold := range []struct {
		name  string
		value int
	}{{"repeated_call", t.RepeatedCall}, {"repeated_result", t.RepeatedResult}, {"alternating_calls", t.AlternatingCalls}, {"repeated_text", t.RepeatedText}} {
		if threshold.value != 0 && threshold.value < 2 {
			return fmt.Errorf("loop detection %s must be 0 (off) or at least 2, got %d", threshold.name, threshold.value)
		}
	}
	return nil
}

// window is how many recent calls the call detectors look at: at least
// minLoopWindow, and always enough for every threshold to be reachable.
func (t LoopThresholds) window() int {
	return max(minLoopWindow, t.RepeatedCall, t.RepeatedResult, 2*t.AlternatingCalls)
}

func (t LoopThresholds) callsOff() bool {
	return t.RepeatedCall == 0 && t.RepeatedResult == 0 && t.AlternatingCalls == 0
}

// loopThresholds is a run's thresholds: the defaults when none were given.
func loopThresholds(configured *LoopThresholds) LoopThresholds {
	if configured == nil {
		return DefaultLoopThresholds
	}
	return *configured
}

// LoopPattern names the kind of loop the guard found.
type LoopPattern string

const (
	LoopRepeatedCall     LoopPattern = "repeated_call"
	LoopRepeatedResult   LoopPattern = "repeated_result"
	LoopAlternatingCalls LoopPattern = "alternating_calls"
	LoopRepeatedText     LoopPattern = "repeated_text"
)

// LoopAction is what the guard did about a loop.
type LoopAction string

const (
	// LoopNudge: the model was told it is looping.
	LoopNudge LoopAction = "nudge"
	// LoopStop: the loop went on after its nudge, so the run stopped.
	LoopStop LoopAction = "stop"
)

// LoopDetection is one loop the guard found, carried by the loop_guard
// event and, for a stop, by the run's [LoopError].
type LoopDetection struct {
	Pattern LoopPattern `json:"pattern"`
	// Tools names the looping calls' tools; empty for repeated text.
	Tools []string `json:"tools,omitempty"`
	// Count is how many times the pattern repeated: calls, results, A-B
	// cycles or replies.
	Count  int        `json:"count"`
	Action LoopAction `json:"action"`
}

// LoopError is the run error of a run the loop guard stopped.
type LoopError struct {
	LoopDetection
	// Detail names the looping calls with their arguments, or the text.
	Detail string
}

func (e *LoopError) Error() string {
	return fmt.Sprintf("loop detected (%s): %s, again after a loop notice", e.Pattern, e.Detail)
}

// The notice prefixes. A nudge is a boundary: the calls before it are not
// counted again. A stop notice is not.
const (
	loopNudgePrefix = "[Loop notice from the host]"
	loopStopPrefix  = "[Loop stop from the host]"
)

const changeApproach = "Change your approach, or explain what is blocking you."

// detectedLoop is one report of a detector.
type detectedLoop struct {
	pattern LoopPattern
	tools   []string
	count   int
	// key identifies the loop: its calls' fingerprints, or its text.
	key string
	// message describes the loop to the model; detail names it in the run
	// error.
	message string
	detail  string
}

func (loop *detectedLoop) detection(action LoopAction) LoopDetection {
	return LoopDetection{Pattern: loop.pattern, Tools: slices.Clone(loop.tools), Count: loop.count, Action: action}
}

func (loop *detectedLoop) notice(action LoopAction) string {
	if action == LoopStop {
		return fmt.Sprintf("%s %s You were already told about this loop, so the run has stopped.", loopStopPrefix, loop.message)
	}
	return fmt.Sprintf("%s %s %s If it goes on, the run stops.", loopNudgePrefix, loop.message, changeApproach)
}

// loopCall is one tool call of the episode with its result.
type loopCall struct {
	tool string
	args []byte
	// key is the call's fingerprint: tool and canonical arguments.
	key    string
	result types.ToolResultMessage
	// polls marks a call to a polling tool, which is never the subject of a
	// report but still takes its place in the sequence.
	polls bool
}

func (call loopCall) describe() string {
	return call.tool + " " + quoted(string(call.args))
}

// loopBatch is the calls of one assistant message, in source order.
type loopBatch struct {
	calls []loopCall
	// nudged is set when one of the batch's results carries a nudge.
	nudged bool
}

// noteLoop runs the loop guard once per tool batch, on its last result:
// history is the transcript up to and including the assistant message whose
// calls produced earlier and last, the batch's other results in source
// order. A nudge or stop notice is appended to last and reported with a
// loop_guard event; a stop also returns the run's error. A batch in which
// the failure guard already noted a result gets no second notice, and after
// cancellation nothing is checked.
func noteLoop(ctx context.Context, config AgentLoopConfig, current AgentContext, earlier []types.ToolResultMessage, last *types.ToolResultMessage, emit AgentEventSink) (*LoopError, error) {
	thresholds := loopThresholds(config.LoopThresholds)
	if ctx.Err() != nil || thresholds.callsOff() {
		return nil, nil
	}
	batch := append(slices.Clip(earlier), *last)
	for _, result := range batch {
		if hostNotice(result.Content) == repeatedFailurePrefix {
			return nil, nil
		}
	}
	loop, action := thresholds.decideCalls(toolEpisode(current.Messages, batch, current.Tools))
	if loop == nil {
		return nil, nil
	}
	content := make([]types.ContentBlock, 0, len(last.Content)+1)
	last.Content = append(append(content, last.Content...), types.NewText(loop.notice(action)))
	detection := loop.detection(action)
	if err := emit(ctx, AgentEvent{Type: EventLoopGuard, Loop: &detection}); err != nil {
		return nil, err
	}
	if action == LoopStop {
		return &LoopError{LoopDetection: loop.detection(action), Detail: loop.detail}, nil
	}
	return nil, nil
}

// noteRepeatedText runs the loop guard on the replies without tool calls
// before a model call, once the messages that send the model back to work
// are in the transcript. A nudge or stop notice is appended as a user
// message and reported with a loop_guard event; a stop also returns the
// run's error, and the run must end before the model call.
func (run *loopRun) noteRepeatedText(ctx context.Context, current *AgentContext) (*LoopError, error) {
	thresholds := loopThresholds(run.config.LoopThresholds)
	if thresholds.RepeatedText == 0 {
		return nil, nil
	}
	loop, action := thresholds.decideText(textEpisode(current.Messages))
	if loop == nil {
		return nil, nil
	}
	detection := loop.detection(action)
	if err := run.emit(ctx, AgentEvent{Type: EventLoopGuard, Loop: &detection}); err != nil {
		return nil, err
	}
	notice := types.UserMessage{Content: types.StringContent(loop.notice(action)), Timestamp: time.Now().UnixMilli()}
	if err := run.appendMessages(ctx, current, []types.Message{notice}); err != nil {
		return nil, err
	}
	if action == LoopStop {
		return &LoopError{LoopDetection: loop.detection(action), Detail: loop.detail}, nil
	}
	return nil, nil
}

// decideCalls reports the loop the newest batch of the episode extends and
// whether it is a nudge or a stop: a stop when an earlier nudge of the
// episode was about the same loop.
func (t LoopThresholds) decideCalls(batches []loopBatch) (*detectedLoop, LoopAction) {
	if len(batches) == 0 {
		return nil, ""
	}
	nudged := map[string]bool{}
	boundary := 0
	for index, batch := range batches[:len(batches)-1] {
		if !batch.nudged {
			continue
		}
		if loop := t.detectCalls(batches[boundary : index+1]); loop != nil {
			nudged[loop.key] = true
		}
		boundary = index + 1
	}
	loop := t.detectCalls(batches[boundary:])
	if loop == nil {
		return nil, ""
	}
	if nudged[loop.key] {
		return loop, LoopStop
	}
	return loop, LoopNudge
}

// detectCalls runs the call detectors, in DetectLoops' priority order, over
// the window of recent calls in batches, reporting only a loop the last
// batch extends.
func (t LoopThresholds) detectCalls(batches []loopBatch) *detectedLoop {
	var calls []loopCall
	for _, batch := range batches {
		calls = append(calls, batch.calls...)
	}
	recent := calls[max(0, len(calls)-t.window()):]
	newest := recent[max(0, len(recent)-len(batches[len(batches)-1].calls)):]
	if len(newest) == 0 {
		return nil
	}
	if loop := t.repeatedResult(recent, newest); loop != nil {
		return loop
	}
	if loop := t.alternatingCalls(recent); loop != nil {
		return loop
	}
	return t.repeatedCall(recent, newest)
}

func (t LoopThresholds) repeatedResult(recent, newest []loopCall) *detectedLoop {
	if t.RepeatedResult == 0 {
		return nil
	}
	type streak struct {
		result string
		count  int
	}
	streaks := map[string]streak{}
	for _, call := range recent {
		if call.polls {
			continue
		}
		digest := resultDigest(call.result)
		current := streaks[call.key]
		if current.count > 0 && current.result == digest {
			current.count++
		} else {
			current = streak{result: digest, count: 1}
		}
		streaks[call.key] = current
	}
	for index := len(newest) - 1; index >= 0; index-- {
		call := newest[index]
		if count := streaks[call.key].count; !call.polls && count >= t.RepeatedResult {
			return &detectedLoop{
				pattern: LoopRepeatedResult, tools: []string{call.tool}, count: count, key: call.key,
				message: fmt.Sprintf("Your last %d calls to `%s` used the same arguments and returned the same result, so calling it again will not change the outcome.", count, call.tool),
				detail:  fmt.Sprintf("%s returned the same result %d times in a row", call.describe(), count),
			}
		}
	}
	return nil
}

func (t LoopThresholds) alternatingCalls(recent []loopCall) *detectedLoop {
	cycles := t.AlternatingCalls
	if cycles == 0 || len(recent) < 2*cycles {
		return nil
	}
	tail := recent[len(recent)-2*cycles:]
	first, second := tail[0], tail[1]
	if first.key == second.key || first.polls || second.polls {
		return nil
	}
	for index, call := range tail {
		if want := []string{first.key, second.key}[index%2]; call.key != want {
			return nil
		}
	}
	calls := fmt.Sprintf("alternated between the same calls to `%s` and `%s`", first.tool, second.tool)
	if first.tool == second.tool {
		calls = fmt.Sprintf("alternated between two calls to `%s` with different arguments", first.tool)
	}
	keys := []string{first.key, second.key}
	slices.Sort(keys)
	return &detectedLoop{
		pattern: LoopAlternatingCalls, tools: []string{first.tool, second.tool}, count: cycles, key: strings.Join(keys, "\x01"),
		message: fmt.Sprintf("You have %s %d times without making progress.", calls, cycles),
		detail:  fmt.Sprintf("%s and %s alternated %d times", first.describe(), second.describe(), cycles),
	}
}

func (t LoopThresholds) repeatedCall(recent, newest []loopCall) *detectedLoop {
	if t.RepeatedCall == 0 {
		return nil
	}
	counts := map[string]int{}
	for _, call := range recent {
		if !call.polls {
			counts[call.key]++
		}
	}
	var best *loopCall
	for index := range newest {
		if call := &newest[index]; !call.polls && (best == nil || counts[call.key] > counts[best.key]) {
			best = call
		}
	}
	if best == nil || counts[best.key] < t.RepeatedCall {
		return nil
	}
	count := counts[best.key]
	return &detectedLoop{
		pattern: LoopRepeatedCall, tools: []string{best.tool}, count: count, key: best.key,
		message: fmt.Sprintf("You have called `%s` with the same arguments %d times in your last %d tool calls, which is not making progress.", best.tool, count, len(recent)),
		detail:  fmt.Sprintf("%s was called %d times in the last %d tool calls", best.describe(), count, len(recent)),
	}
}

// toolEpisode is the tool batches since the last user message, oldest first.
// history is the transcript up to and including the assistant message whose
// calls produced batch, which is the last one. Errored or aborted replies
// carry no executed calls and are skipped.
func toolEpisode(history []types.Message, batch []types.ToolResultMessage, tools []AgentTool) []loopBatch {
	start := len(history)
	for start > 0 {
		role := history[start-1].Role()
		if role != "assistant" && role != "toolResult" {
			break
		}
		start--
	}
	polling := map[string]bool{}
	for _, tool := range tools {
		if tool.Polls {
			polling[tool.Name] = true
		}
	}
	var batches []loopBatch
	var calls []types.ContentBlock
	var results []*types.ToolResultMessage
	flush := func() {
		var built loopBatch
		for index, call := range calls {
			result := results[index]
			if result == nil {
				continue
			}
			args := canonicalArguments(call.Arguments)
			built.calls = append(built.calls, loopCall{tool: call.Name, args: args, key: call.Name + "\x00" + string(args), result: *result, polls: polling[call.Name]})
			built.nudged = built.nudged || hostNotice(result.Content) == loopNudgePrefix
		}
		if len(built.calls) > 0 {
			batches = append(batches, built)
		}
		calls, results = nil, nil
	}
	// Results follow their assistant message in source order; calls that
	// share an id take their results in order.
	attach := func(result types.ToolResultMessage) {
		for index, call := range calls {
			if results[index] == nil && call.ID == result.ToolCallID {
				results[index] = &result
				return
			}
		}
	}
	for _, message := range history[start:] {
		switch message := message.(type) {
		case types.AssistantMessage:
			flush()
			if message.StopReason == types.StopError || message.StopReason == types.StopAborted {
				continue
			}
			for _, block := range message.Content {
				if block.Type == types.BlockToolCall {
					calls = append(calls, block)
				}
			}
			results = make([]*types.ToolResultMessage, len(calls))
		case types.ToolResultMessage:
			attach(message)
		}
	}
	for _, result := range batch {
		attach(result)
	}
	flush()
	return batches
}

// textItem is one element of a text episode: a reply without tool calls
// (its normalized text) or a nudge.
type textItem struct {
	text  string
	nudge bool
}

// textEpisode is the replies without tool calls at the end of messages,
// oldest first, with the nudges between them. Only hook feedback that sent
// the model back to work (and the guard's own notices) may separate them;
// any other message ends the episode. Errored replies are skipped.
func textEpisode(messages []types.Message) []textItem {
	var items []textItem
walk:
	for index := len(messages) - 1; index >= 0; index-- {
		switch message := messages[index].(type) {
		case types.AssistantMessage:
			if message.StopReason == types.StopError || message.StopReason == types.StopAborted {
				continue
			}
			if hasToolCalls(message) {
				break walk
			}
			items = append(items, textItem{text: normalizedReply(message)})
		case types.UserMessage:
			text := message.Content.Text
			switch {
			case message.Content.IsBlocks():
				break walk
			case strings.HasPrefix(text, loopNudgePrefix):
				items = append(items, textItem{nudge: true})
			case strings.HasPrefix(text, loopStopPrefix) || isHookContinuation(text):
			default:
				break walk
			}
		default:
			break walk
		}
	}
	slices.Reverse(items)
	return items
}

// decideText is decideCalls for a text episode.
func (t LoopThresholds) decideText(items []textItem) (*detectedLoop, LoopAction) {
	nudged := map[string]bool{}
	boundary := 0
	for index, item := range items {
		if !item.nudge {
			continue
		}
		if loop := t.detectText(items[boundary:index]); loop != nil {
			nudged[loop.key] = true
		}
		boundary = index + 1
	}
	loop := t.detectText(items[boundary:])
	if loop == nil {
		return nil, ""
	}
	if nudged[loop.key] {
		return loop, LoopStop
	}
	return loop, LoopNudge
}

// detectText reports the newest reply when the replies before it, back to
// the episode start, repeat its text RepeatedText times. An empty reply
// repeats nothing.
func (t LoopThresholds) detectText(items []textItem) *detectedLoop {
	if t.RepeatedText == 0 || len(items) == 0 {
		return nil
	}
	text := items[len(items)-1].text
	if text == "" {
		return nil
	}
	count := 0
	for index := len(items) - 1; index >= 0 && items[index].text == text; index-- {
		count++
	}
	if count < t.RepeatedText {
		return nil
	}
	return &detectedLoop{
		pattern: LoopRepeatedText, count: count, key: "text\x00" + text,
		message: fmt.Sprintf("Your last %d replies had the same text and no tool calls, so repeating it will not change the outcome.", count),
		detail:  fmt.Sprintf("the reply %q was repeated %d times", quoted(text), count),
	}
}

// normalizedReply is a reply's text with whitespace collapsed and lowercased.
func normalizedReply(message types.AssistantMessage) string {
	var text strings.Builder
	for _, block := range message.Content {
		if block.Type == types.BlockText {
			text.WriteString(block.Text)
			text.WriteByte(' ')
		}
	}
	return strings.ToLower(strings.Join(strings.Fields(text.String()), " "))
}

// resultDigest identifies what a call returned: its outcome and its content
// without a host notice.
func resultDigest(result types.ToolResultMessage) string {
	digest := sha256.New()
	outcome := byte(0)
	if result.IsError {
		outcome = 1
	}
	digest.Write([]byte{outcome})
	for _, block := range resultContent(result.Content) {
		for _, field := range []string{string(block.Type), block.Text, block.Data, block.MimeType, block.RefStore, block.RefKey} {
			writeField(digest, field)
		}
	}
	return string(digest.Sum(nil))
}

// writeField writes field length-prefixed, so field boundaries are unambiguous.
func writeField(digest hash.Hash, field string) {
	var length [8]byte
	binary.LittleEndian.PutUint64(length[:], uint64(len(field)))
	digest.Write(length[:])
	_, _ = io.WriteString(digest, field)
}
