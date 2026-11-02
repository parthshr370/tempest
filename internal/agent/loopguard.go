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

// The loop guard catches a model that keeps doing the same work without
// making progress, which the repeated-failure guard (repeat.go) cannot see
// when the calls succeed or other calls come between them. It rests on one
// rule: a loop is repeated work with no progress in between.
//
// A call is its tool and canonical arguments; its result is its outcome and
// content. A call returning what it returned the last time is a repeat. Any
// other call is new: a first call, or a known call with a changed result. A
// new successful call to a tool that may change things, which is any call but
// a workspace read (AgentTool.ReadsWorkspace), is progress: it may have
// changed the workspace or the world, so every count starts over. A failed
// call is never progress (a failure seldom changes anything), nor is a repeat
// (doing the same thing again with the same answer changes nothing new).
//
// A call's count is its repeats since the last progress, each within
// maxLoopGap calls of the one before. Two patterns read it:
//
//   - repeated result: a failed call, or a workspace read, returned the same
//     result RepeatedResult times;
//   - repeated poll: any other successful call (a command, an MCP or
//     extension tool) returned the same result RepeatedPoll times. Such a
//     call may be polling something outside the workspace that has yet to
//     change, so it gets more room. A polling tool (AgentTool.Polls) is
//     never reported while it succeeds.
//
// A result the failure guard already put its notice on is not reported
// here: consecutive identical failures keep that guard's notice and stop.
// The batch's other calls are still checked.
//
// A third pattern, repeated text, is RepeatedText consecutive replies
// without tool calls, separated only by hook feedback that sent the model
// back to work, whose text is the same ignoring case and whitespace.
//
// Only a call of the newest batch (or the newest reply) is reported, so a
// pattern the model already left does not fire later. The first report is a
// nudge: a notice appended at the tail of the transcript, to the batch's last
// tool result or as a user message before the next model call. A nudge
// starts every count over, so the loop has to build up anew. The same
// pattern reported again after its nudge, for the same call or one that was
// repeating with it (the other leg of an A-B-A-B loop), with no progress
// since, stops the run with StopLoopDetected; progress after a nudge means
// the next report is a nudge again.
//
// Like the failure streak, everything is derived from the transcript each
// time, never kept in memory: a nudge is recognized by its prefix, and the
// loop it was about is recomputed from the transcript up to it. A resumed or
// replayed session therefore reaches the same decisions and never repeats a
// nudge. Counting starts over at any user message (a prompt, a steer, a job
// notice, a compaction summary) except a conversation reminder, which is the
// host restating instructions, not new input; Continue adds none, so it
// carries the count on. A stop notice is not a boundary: after a stop, the
// next repetition of the same loop stops the run again at once.

// LoopThresholds configures the loop guard. Each threshold is 0 (that
// pattern off) or at least 2.
type LoopThresholds struct {
	// RepeatedResult reports a failed call or a workspace read that returned
	// the same result this many times with no progress in between.
	RepeatedResult int
	// RepeatedPoll reports any other successful call that returned the same
	// result this many times with no progress in between.
	RepeatedPoll int
	// RepeatedText reports this many consecutive replies without tool calls
	// whose text is the same.
	RepeatedText int
}

// DefaultLoopThresholds: a read that returns the same thing a third time
// while nothing changed, or a call that keeps failing the same way between
// other calls, is not making progress. A command or remote tool polling for a
// change gets ten unchanged answers, a minute or two of model turns, before
// the notice, and ten more before the stop. The first report is only a
// notice.
var DefaultLoopThresholds = LoopThresholds{RepeatedResult: 3, RepeatedPoll: 10, RepeatedText: 3}

// maxLoopGap is how far apart, in calls, two runs of the same call may be
// and still continue its count: a model that makes ten other calls before
// coming back to a call went elsewhere in between.
const maxLoopGap = 10

// Validate rejects a threshold that is neither 0 nor at least 2: a single
// call or reply is not a repetition.
func (t LoopThresholds) Validate() error {
	for _, threshold := range []struct {
		name  string
		value int
	}{{"repeated_result", t.RepeatedResult}, {"repeated_poll", t.RepeatedPoll}, {"repeated_text", t.RepeatedText}} {
		if threshold.value != 0 && threshold.value < 2 {
			return fmt.Errorf("loop detection %s must be 0 (off) or at least 2, got %d", threshold.name, threshold.value)
		}
	}
	return nil
}

func (t LoopThresholds) callsOff() bool {
	return t.RepeatedResult == 0 && t.RepeatedPoll == 0
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
	LoopRepeatedResult LoopPattern = "repeated_result"
	LoopRepeatedPoll   LoopPattern = "repeated_poll"
	LoopRepeatedText   LoopPattern = "repeated_text"
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
	// Count is how many times the pattern repeated: results or replies.
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
	// key identifies the loop for escalation: its pattern and its call's
	// fingerprint, or its text.
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
	key string
	// digest is the result's fingerprint (resultDigest).
	digest string
	failed bool
	// polls marks a call to a polling tool, which is never the subject of a
	// report while it succeeds.
	polls bool
	// readsWorkspace marks a workspace read: never progress, and reported
	// as a repeated result rather than a repeated poll.
	readsWorkspace bool
	// noted marks a result the failure guard already put its notice on: that
	// repetition is the failure guard's to report.
	noted bool
}

func (call loopCall) describe() string {
	return call.tool + " " + quoted(string(call.args))
}

// pattern is the pattern a repeat of call counts toward and its threshold;
// a zero threshold means the call is never reported.
func (t LoopThresholds) pattern(call loopCall) (LoopPattern, int) {
	switch {
	case call.failed || call.readsWorkspace:
		return LoopRepeatedResult, t.RepeatedResult
	case call.polls:
		return "", 0
	default:
		return LoopRepeatedPoll, t.RepeatedPoll
	}
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
// loop_guard event; a stop also returns the run's error. After cancellation
// nothing is checked.
func noteLoop(ctx context.Context, config AgentLoopConfig, current AgentContext, earlier []types.ToolResultMessage, last *types.ToolResultMessage, emit AgentEventSink) (*LoopError, error) {
	thresholds := loopThresholds(config.LoopThresholds)
	if ctx.Err() != nil || thresholds.callsOff() {
		return nil, nil
	}
	batch := append(slices.Clip(earlier), *last)
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

// loopCounts is the episode's state, built by observing its batches in
// order.
type loopCounts struct {
	// calls is how many calls were observed; a call's position is the count
	// before it.
	calls int
	// last is each call's latest run and its position.
	last map[string]loopSeen
	// streak is each call's repeats since the last progress or nudge.
	streak map[string]int
	// nudged holds the loops nudged since the last progress.
	nudged map[string]bool
}

type loopSeen struct {
	call loopCall
	at   int
}

func newLoopCounts() *loopCounts {
	return &loopCounts{last: map[string]loopSeen{}, streak: map[string]int{}, nudged: map[string]bool{}}
}

// observe counts batch's calls in source order.
func (c *loopCounts) observe(batch loopBatch) {
	for _, call := range batch.calls {
		seen, known := c.last[call.key]
		c.last[call.key] = loopSeen{call: call, at: c.calls}
		repeat := known && seen.call.digest == call.digest
		switch {
		case repeat && c.calls-seen.at <= maxLoopGap:
			c.streak[call.key]++
		case !repeat && !call.failed && !call.readsWorkspace:
			// Progress: everything counted so far is behind it.
			clear(c.streak)
			clear(c.nudged)
			c.streak[call.key] = 1
		default:
			c.streak[call.key] = 1
		}
		c.calls++
	}
}

// nudge records the loop a nudge was about and starts every count over. The
// nudge covers its call and every other call of the same pattern that was
// repeating with it, such as the other leg of an A-B-A-B loop: the model was
// told about that stretch of repeated work, whichever call comes back first.
func (c *loopCounts) nudge(t LoopThresholds, loop *detectedLoop) {
	if loop != nil {
		c.nudged[loop.key] = true
		for key, count := range c.streak {
			call := c.last[key].call
			if pattern, _ := t.pattern(call); count >= 2 && pattern == loop.pattern {
				c.nudged[string(pattern)+"\x00"+key] = true
			}
		}
	}
	clear(c.streak)
}

// decideCalls reports the loop the newest batch of the episode extends and
// whether it is a nudge or a stop: a stop when the same loop was nudged
// earlier in the episode with no progress since.
func (t LoopThresholds) decideCalls(batches []loopBatch) (*detectedLoop, LoopAction) {
	if len(batches) == 0 {
		return nil, ""
	}
	counts := newLoopCounts()
	for _, batch := range batches[:len(batches)-1] {
		counts.observe(batch)
		if !batch.nudged {
			continue
		}
		counts.nudge(t, t.detectCalls(batch, counts))
	}
	newest := batches[len(batches)-1]
	counts.observe(newest)
	loop := t.detectCalls(newest, counts)
	if loop == nil {
		return nil, ""
	}
	if counts.nudged[loop.key] {
		return loop, LoopStop
	}
	return loop, LoopNudge
}

// detectCalls reports the first call of batch, in source order, whose count
// reached its pattern's threshold.
func (t LoopThresholds) detectCalls(batch loopBatch, counts *loopCounts) *detectedLoop {
	for _, call := range batch.calls {
		pattern, threshold := t.pattern(call)
		count := counts.streak[call.key]
		if call.noted || threshold == 0 || count < threshold {
			continue
		}
		loop := &detectedLoop{
			pattern: pattern, tools: []string{call.tool}, count: count, key: string(pattern) + "\x00" + call.key,
			message: fmt.Sprintf("Your last %d calls to `%s` used the same arguments and returned the same result, and nothing changed in between, so calling it again will not change the outcome.", count, call.tool),
			detail:  fmt.Sprintf("%s returned the same result %d times with no progress in between", call.describe(), count),
		}
		if pattern == LoopRepeatedPoll {
			loop.message = fmt.Sprintf("Your last %d calls to `%s` used the same arguments and returned the same result, and nothing changed in between. If you are waiting for something, wait in one call instead: a command that loops until it is ready under a timeout, such as `timeout 120 sh -c 'until CHECK; do sleep 2; done'`, or a background job and the wait tool.", count, call.tool)
		}
		return loop
	}
	return nil
}

// toolEpisode is the tool batches since the last user message other than a
// conversation reminder, oldest first. history is the transcript up to and
// including the assistant message whose calls produced batch, which is the
// last one. Errored or aborted replies carry no executed calls and are
// skipped.
func toolEpisode(history []types.Message, batch []types.ToolResultMessage, tools []AgentTool) []loopBatch {
	start := len(history)
	for start > 0 {
		message := history[start-1]
		if role := message.Role(); role != "assistant" && role != "toolResult" && !isReminder(message) {
			break
		}
		start--
	}
	byName := map[string]AgentTool{}
	for _, tool := range tools {
		byName[tool.Name] = tool
	}
	var batches []loopBatch
	var calls []types.ContentBlock
	var results []*types.ToolResultMessage
	// Identical calls in one reply are one decision, made before any of
	// their results were seen, so a batch holds each call once: a loop is
	// the model repeating itself across replies.
	flush := func() {
		var built loopBatch
		for index, call := range calls {
			result := results[index]
			if result == nil {
				continue
			}
			built.nudged = built.nudged || hasHostNotice(result.Content, loopNudgePrefix)
			args := canonicalArguments(call.Arguments)
			key := call.Name + "\x00" + string(args)
			if slices.ContainsFunc(built.calls, func(seen loopCall) bool { return seen.key == key }) {
				continue
			}
			tool := byName[call.Name]
			built.calls = append(built.calls, loopCall{
				tool: call.Name, args: args, key: key, digest: resultDigest(*result), failed: result.IsError,
				polls: tool.Polls, readsWorkspace: tool.ReadsWorkspace != nil && tool.ReadsWorkspace(call.Arguments),
				noted: hasHostNotice(result.Content, repeatedFailurePrefix),
			})
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

// isReminder reports whether message is a conversation reminder's firing:
// the host restating standing instructions, which is not new input, so it
// ends no loop episode or failure streak.
func isReminder(message types.Message) bool {
	user, ok := message.(types.UserMessage)
	return ok && user.Reminder != ""
}

// textItem is one element of a text episode: a reply without tool calls
// (its normalized text) or a nudge.
type textItem struct {
	text  string
	nudge bool
}

// textEpisode is the replies without tool calls at the end of messages,
// oldest first, with the nudges between them. Only hook feedback that sent
// the model back to work, conversation reminders and the guard's own notices
// may separate them; any other message ends the episode. Errored replies are
// skipped.
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
			case message.Reminder != "":
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
