package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.harness.dev/harness/internal/engine/types"
)

// A tool call that failed, sent again unchanged, fails the same way: a tool's
// result depends on its arguments and on the workspace, and a failed call
// seldom changes the workspace. A model that keeps repeating one is stuck, and
// without a signal it can stay stuck until the turn cap (the benchmark's
// write_stdin {"job_id":"x"}, sent 30 times against "job_not_found").
//
// The loop therefore counts a failure streak: the tool results immediately
// before a failed result, in transcript order, that are the same failure,
// meaning the same tool, the same arguments after JSON canonicalization and
// the same result content. Any other tool result (a success, another call or
// another error) or any user message ends the streak. The count is derived
// from the transcript every time, never kept in memory, so it is the same
// after a resume or a replay of the journal.
const (
	// repeatedFailureNoticeAt is the streak at which a failed result starts
	// carrying the notice: the first unchanged retry already shows the model
	// did not act on the error, and the notice costs nothing on a result that
	// is already a failure.
	repeatedFailureNoticeAt = 2
	// RepeatedFailureLimit is the streak that ends the run with
	// StopRepeatedFailure. The model has then seen the notice three times,
	// which leaves room for one stubborn retry or a transient error that
	// persists, and a stuck run still ends after five wasted calls rather
	// than at the turn cap.
	RepeatedFailureLimit = 5
)

// repeatedFailurePrefix starts the notice block, which is how a later streak
// count recognizes it and compares the result content without it.
const repeatedFailurePrefix = "[Repeated failure notice from the host]"

// noteRepeatedFailure appends the repeated-failure notice to message when it
// fails the same way as the results just before it, and returns the streak
// including message (0 when message is not a failure). history is the
// transcript up to and including assistant, whose calls produced earlier and
// message; earlier holds the results of assistant's calls before message, in
// source order. After cancellation the failures are the abort's, not the
// model's, so nothing is noted.
func noteRepeatedFailure(ctx context.Context, history []types.Message, assistant types.AssistantMessage, earlier []types.ToolResultMessage, call types.ContentBlock, message *types.ToolResultMessage) int {
	if !message.IsError || ctx.Err() != nil {
		return 0
	}
	key := failureKey{tool: call.Name, args: canonicalArguments(call.Arguments), content: message.Content}
	streak := 1 + key.streak(history, assistant, earlier)
	if streak < repeatedFailureNoticeAt {
		return streak
	}
	notice := fmt.Sprintf("%s This exact %s call, with the same arguments, has now failed %d times in a row with the same error. Sending it again will not change the result: change the arguments or take a different approach. The run stops if it fails %d times in a row.", repeatedFailurePrefix, call.Name, streak, RepeatedFailureLimit)
	if streak >= RepeatedFailureLimit {
		notice = fmt.Sprintf("%s This exact %s call, with the same arguments, has failed %d times in a row with the same error, so the run has stopped.", repeatedFailurePrefix, call.Name, streak)
	}
	content := make([]types.ContentBlock, 0, len(message.Content)+1)
	message.Content = append(append(content, message.Content...), types.NewText(notice))
	return streak
}

// maxQuotedBytes bounds each value a repeated-failure run error quotes, so a
// large write or a long tool error cannot flood a one-line report.
const maxQuotedBytes = 200

// repeatedFailureError is the run error for a streak that reached the limit.
func repeatedFailureError(call types.ContentBlock, streak int, message types.ToolResultMessage) error {
	text := ""
	for _, block := range resultContent(message.Content) {
		if block.Type == types.BlockText {
			text = block.Text
			break
		}
	}
	return fmt.Errorf("%s %s failed %d times in a row with the same error: %s", call.Name, quoted(string(canonicalArguments(call.Arguments))), streak, quoted(text))
}

// quoted is the first line of text, cut to maxQuotedBytes.
func quoted(text string) string {
	if line, _, cut := strings.Cut(strings.TrimSpace(text), "\n"); cut {
		text = line
	}
	if len(text) > maxQuotedBytes {
		text = strings.ToValidUTF8(text[:maxQuotedBytes], "") + "…"
	}
	return text
}

// failureKey is what makes two failed results the same failure.
type failureKey struct {
	tool    string
	args    []byte
	content []types.ContentBlock
}

// streak counts the results before the keyed one that are the same failure,
// walking back through earlier, then history. Results are grouped under the
// assistant message that called them: walking back, a group's results come
// before its assistant message, so they are held until it is reached.
func (key failureKey) streak(history []types.Message, assistant types.AssistantMessage, earlier []types.ToolResultMessage) int {
	count := 0
	for index := len(earlier) - 1; index >= 0; index-- {
		if !key.matches(earlier[index], assistant) {
			return count
		}
		count++
	}
	var held []types.ToolResultMessage
	for index := len(history) - 1; index >= 0; index-- {
		switch message := history[index].(type) {
		case types.ToolResultMessage:
			held = append(held, message)
		case types.AssistantMessage:
			for _, result := range held {
				if !key.matches(result, message) {
					return count
				}
				count++
			}
			held = held[:0]
		default:
			return count
		}
	}
	return count
}

// matches reports whether result, called by caller, is the keyed failure.
func (key failureKey) matches(result types.ToolResultMessage, caller types.AssistantMessage) bool {
	if !result.IsError || result.ToolName != key.tool || !sameContent(resultContent(result.Content), key.content) {
		return false
	}
	for _, block := range caller.Content {
		if block.Type == types.BlockToolCall && block.ID == result.ToolCallID {
			return block.Name == key.tool && bytes.Equal(canonicalArguments(block.Arguments), key.args)
		}
	}
	return false
}

// resultContent is a result's content without the repeated-failure notice.
func resultContent(content []types.ContentBlock) []types.ContentBlock {
	if last := len(content) - 1; last >= 0 && content[last].Type == types.BlockText && strings.HasPrefix(content[last].Text, repeatedFailurePrefix) {
		return content[:last]
	}
	return content
}

func sameContent(a, b []types.ContentBlock) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		x, y := a[index], b[index]
		if x.Type != y.Type || x.Text != y.Text || x.Data != y.Data || x.MimeType != y.MimeType || x.RefStore != y.RefStore || x.RefKey != y.RefKey {
			return false
		}
	}
	return true
}

// canonicalArguments re-encodes tool-call arguments with sorted object keys
// and no insignificant whitespace, so formatting differences don't hide a
// repeat. Scalars keep their literal form. Arguments that are not valid JSON
// compare as their trimmed bytes.
func canonicalArguments(arguments json.RawMessage) []byte {
	trimmed := bytes.TrimSpace(arguments)
	if len(trimmed) == 0 {
		return []byte("{}")
	}
	var canonical bytes.Buffer
	if writeCanonicalJSON(&canonical, trimmed) != nil {
		return trimmed
	}
	return canonical.Bytes()
}

// writeCanonicalJSON writes value with objects' keys sorted, recursively.
func writeCanonicalJSON(out *bytes.Buffer, value json.RawMessage) error {
	value = bytes.TrimSpace(value)
	switch {
	case len(value) > 0 && value[0] == '{':
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(value, &fields); err != nil {
			return err
		}
		out.WriteByte('{')
		for index, name := range slices.Sorted(maps.Keys(fields)) {
			if index > 0 {
				out.WriteByte(',')
			}
			encodedName, err := json.Marshal(name)
			if err != nil {
				return err
			}
			out.Write(encodedName)
			out.WriteByte(':')
			if err := writeCanonicalJSON(out, fields[name]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
		return nil
	case len(value) > 0 && value[0] == '[':
		var items []json.RawMessage
		if err := json.Unmarshal(value, &items); err != nil {
			return err
		}
		out.WriteByte('[')
		for index, item := range items {
			if index > 0 {
				out.WriteByte(',')
			}
			if err := writeCanonicalJSON(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
		return nil
	default:
		return json.Compact(out, value)
	}
}
