package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"go.harness.dev/harness/internal/engine/jsonrepair"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/transport/sseparse"
)

// openAIStreamChunk is a single decoded chunk from the chat-completions SSE stream.
type openAIStreamChunk struct {
	ID      string               `json:"id"`
	Model   string               `json:"model"`
	Choices []openAIStreamChoice `json:"choices"`
	Usage   json.RawMessage      `json:"usage"`
	Error   json.RawMessage      `json:"error"`
}

// openAIStreamChoice keeps each delta beside its finish reason, which the stream
// sends through the same choice envelope.
type openAIStreamChoice struct {
	Index        int         `json:"index"`
	Delta        openAIDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

// openAIDelta keeps both reasoning spellings because compatible endpoints don't
// agree on one field name.
type openAIDelta struct {
	Content          string                `json:"content"`
	ToolCalls        []openAIToolCallDelta `json:"tool_calls"`
	Reasoning        string                `json:"reasoning"`         // llama.cpp / chutes
	ReasoningContent string                `json:"reasoning_content"` // standard
}

// openAIToolCallDelta carries whichever tool-call identity a chunk includes;
// later chunks often only include the index.
type openAIToolCallDelta struct {
	Index    *int             `json:"index"`
	ID       string           `json:"id"`
	Function *openAIFuncDelta `json:"function"`
}

// openAIFuncDelta holds fragments rather than a final function payload, so
// folding has to concatenate Arguments.
type openAIFuncDelta struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// openAIFoldBlock tracks one in-progress tool call: where it lives in the output
// content and the accumulated argument fragments.
type openAIFoldBlock struct {
	StreamIndex  int
	ContentIndex int
	PartialArgs  string
}

// openAIFoldState accumulates streamed chunks into a single assistant message,
// indexing open tool-call blocks by both their stream index and id.
type openAIFoldState struct {
	Output          *types.AssistantMessage
	ToolCallByIndex map[int]*openAIFoldBlock
	ToolCallByID    map[string]*openAIFoldBlock
	HasFinishReason bool
}

// iterateOpenAIEvents reads the SSE stream, skipping keep-alives and [DONE], and
// invokes fn for each parsed chunk while pinging onActivity on every event.
func iterateOpenAIEvents(ctx context.Context, r io.Reader, onActivity func(), fn func(*openAIStreamChunk) error) error {
	return sseparse.IterateSSE(ctx, r, func(event sseparse.ServerSentEvent) error {
		if onActivity != nil {
			onActivity()
		}
		data := strings.TrimSpace(event.Data)
		if data == "" || data == "[DONE]" {
			return nil
		}
		var chunk openAIStreamChunk
		if err := jsonrepair.ParseJsonWithRepair(data, &chunk); err != nil {
			return fmt.Errorf("could not parse OpenAI SSE chunk: %s; data=%s", err.Error(), data)
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return fmt.Errorf("OpenAI stream error: %s", chunk.Error)
		}
		return fn(&chunk)
	})
}

// foldOpenAIChunk merges one chunk into state and returns the stream events it
// produces (text, thinking, tool-call deltas) along with any finish reason.
func foldOpenAIChunk(chunk *openAIStreamChunk, state *openAIFoldState, model types.Model) ([]types.StreamEvent, error) {
	output := state.Output

	output.ResponseID = firstNonEmptyStr(output.ResponseID, chunk.ID)
	if chunk.Model != "" && chunk.Model != model.ID {
		output.ResponseModel = firstNonEmptyStr(output.ResponseModel, chunk.Model)
	}

	if len(chunk.Usage) > 0 {
		var rawUsage map[string]any
		if err := json.Unmarshal(chunk.Usage, &rawUsage); err == nil {
			output.Usage = parseChunkUsage(rawUsage, model)
		}
	}

	var streamEvents []types.StreamEvent
	if len(chunk.Choices) > 0 {
		choice := chunk.Choices[0]

		if choice.FinishReason != nil && *choice.FinishReason != "" {
			stopReason, message, err := mapOpenAIStopReason(*choice.FinishReason)
			if err != nil {
				return nil, err
			}
			output.StopReason = stopReason
			if message != "" {
				output.ErrorMessage = message
			}
			state.HasFinishReason = true
		}

		delta := choice.Delta

		// TS gates on content.length > 0 (openai-completions.ts:349-353), not on
		// trimmed non-empty — a whitespace-only delta ("\n", "  ") is real text
		// (blank lines, indentation) and must not be dropped.
		if delta.Content != "" {
			evs := foldOpenAITextDelta(state, delta.Content)
			streamEvents = append(streamEvents, evs...)
		}

		if reasonDelta := reasonDeltaStr(delta); reasonDelta != "" {
			evs := foldOpenAIThinkingDelta(state, reasonDelta)
			streamEvents = append(streamEvents, evs...)
		}

		for _, tc := range delta.ToolCalls {
			evs := foldOpenAIToolCallDelta(state, tc)
			streamEvents = append(streamEvents, evs...)
		}
	}

	return streamEvents, nil
}

// reasonDeltaStr returns the reasoning text from a delta, preferring the standard
// reasoning_content field over the vendor reasoning field.
func reasonDeltaStr(delta openAIDelta) string {
	// TS gates on value.length > 0 (openai-completions.ts:360-372); mirror the
	// non-empty (not trimmed-non-empty) check so whitespace reasoning survives.
	if delta.ReasoningContent != "" {
		return delta.ReasoningContent
	}
	if delta.Reasoning != "" {
		return delta.Reasoning
	}
	return ""
}

// foldOpenAITextDelta appends text to the current text block, opening a new block
// (and emitting a start event) when none is active.
func foldOpenAITextDelta(state *openAIFoldState, text string) []types.StreamEvent {
	output := state.Output
	if !hasOpenAITextBlock(output) {
		contentIndex := len(output.Content)
		output.Content = append(output.Content, types.ContentBlock{Type: types.BlockText, Text: text})
		return []types.StreamEvent{
			{Type: types.EvTextStart, ContentIndex: contentIndex, Partial: output},
			{Type: types.EvTextDelta, ContentIndex: contentIndex, Delta: text, Partial: output},
		}
	}
	contentIndex := lastTextBlockIndex(output)
	if contentIndex < 0 {
		return nil
	}
	blk := &output.Content[contentIndex]
	blk.Text += text
	return []types.StreamEvent{{Type: types.EvTextDelta, ContentIndex: contentIndex, Delta: text, Partial: output}}
}

// lastTextBlockIndex finds the latest text block so text can resume after an
// interleaved tool call.
func lastTextBlockIndex(output *types.AssistantMessage) int {
	for i := len(output.Content) - 1; i >= 0; i-- {
		if output.Content[i].Type == types.BlockText {
			return i
		}
	}
	return -1
}

// hasOpenAITextBlock reports whether text can resume even when another block now
// follows it.
func hasOpenAITextBlock(output *types.AssistantMessage) bool {
	for i := len(output.Content) - 1; i >= 0; i-- {
		if output.Content[i].Type == types.BlockText {
			return true
		}
	}
	return false
}

// foldOpenAIThinkingDelta appends delta to the current thinking block, opening a
// new block (and emitting a start event) when none is active.
func foldOpenAIThinkingDelta(state *openAIFoldState, delta string) []types.StreamEvent {
	output := state.Output
	if !hasOpenAIThinkingBlock(output) {
		contentIndex := len(output.Content)
		output.Content = append(output.Content, types.ContentBlock{Type: types.BlockThinking, Thinking: delta})
		return []types.StreamEvent{
			{Type: types.EvThinkingStart, ContentIndex: contentIndex, Partial: output},
			{Type: types.EvThinkingDelta, ContentIndex: contentIndex, Delta: delta, Partial: output},
		}
	}
	contentIndex := lastThinkingBlockIndex(output)
	if contentIndex < 0 {
		return nil
	}
	blk := &output.Content[contentIndex]
	blk.Thinking += delta
	return []types.StreamEvent{{Type: types.EvThinkingDelta, ContentIndex: contentIndex, Delta: delta, Partial: output}}
}

// lastThinkingBlockIndex finds the latest thinking block so reasoning can resume
// after an interleaved tool call.
func lastThinkingBlockIndex(output *types.AssistantMessage) int {
	for i := len(output.Content) - 1; i >= 0; i-- {
		if output.Content[i].Type == types.BlockThinking {
			return i
		}
	}
	return -1
}

// hasOpenAIThinkingBlock reports whether reasoning can resume even when another
// block now follows it.
func hasOpenAIThinkingBlock(output *types.AssistantMessage) bool {
	for i := len(output.Content) - 1; i >= 0; i-- {
		if output.Content[i].Type == types.BlockThinking {
			return true
		}
	}
	return false
}

// foldOpenAIToolCallDelta resolves the tool-call block by stream index or id,
// creating it on first sight, and accumulates its name and argument fragments.
func foldOpenAIToolCallDelta(state *openAIFoldState, tc openAIToolCallDelta) []types.StreamEvent {
	output := state.Output
	streamIndex := -1
	if tc.Index != nil {
		streamIndex = *tc.Index
	}

	var block *openAIFoldBlock
	if streamIndex >= 0 {
		if b, ok := state.ToolCallByIndex[streamIndex]; ok {
			block = b
		}
	}
	if block == nil && tc.ID != "" {
		if b, ok := state.ToolCallByID[tc.ID]; ok {
			block = b
		}
	}

	var events []types.StreamEvent
	if block == nil {
		contentIndex := len(output.Content)
		name := ""
		if tc.Function != nil {
			name = tc.Function.Name
		}
		newBlk := types.ContentBlock{Type: types.BlockToolCall, ID: tc.ID, Name: name}
		output.Content = append(output.Content, newBlk)

		block = &openAIFoldBlock{StreamIndex: streamIndex, ContentIndex: contentIndex}
		if streamIndex >= 0 {
			state.ToolCallByIndex[streamIndex] = block
		}
		if tc.ID != "" {
			state.ToolCallByID[tc.ID] = block
		}
		events = append(events, types.StreamEvent{Type: types.EvToolCallStart, ContentIndex: contentIndex, ToolCall: &newBlk, Partial: output})
		// FALL THROUGH: the same chunk that creates the block may also carry
		// arguments/name (TS accumulates on the creating chunk, openai-completions.ts:397-421).
		// Returning here would drop a single-delta tool call's arguments entirely.
	}

	if block.ContentIndex >= 0 && block.ContentIndex < len(output.Content) {
		blk := &output.Content[block.ContentIndex]
		if blk.Type == types.BlockToolCall {
			if tc.ID != "" && blk.ID == "" {
				blk.ID = tc.ID
				state.ToolCallByID[tc.ID] = block
			}
			if blk.Name == "" && tc.Function != nil && tc.Function.Name != "" {
				blk.Name = tc.Function.Name
			}
			if tc.Function != nil && tc.Function.Arguments != "" {
				delta := tc.Function.Arguments
				block.PartialArgs += delta
				blk.Arguments = streamingJSONRaw(block.PartialArgs)
				events = append(events, types.StreamEvent{Type: types.EvToolCallDelta, ContentIndex: block.ContentIndex, Delta: delta, Partial: output})
			}
		}
	}
	return events
}

// foldOpenAIFinalizeBlocks emits the closing end event for every content block,
// re-parsing each tool call's full argument buffer into final JSON.
func foldOpenAIFinalizeBlocks(state *openAIFoldState) []types.StreamEvent {
	var events []types.StreamEvent
	output := state.Output
	for i := range output.Content {
		blk := &output.Content[i]
		switch blk.Type {
		case types.BlockText:
			events = append(events, types.StreamEvent{Type: types.EvTextEnd, ContentIndex: i, Content: blk.Text, Partial: output})
		case types.BlockThinking:
			events = append(events, types.StreamEvent{Type: types.EvThinkingEnd, ContentIndex: i, Content: blk.Thinking, Partial: output})
		case types.BlockToolCall:
			if b := state.toolCallBlockByContentIndex(i); b != nil {
				blk.Arguments = finalJSONRaw(b.PartialArgs)
			}
			assembled := *blk
			events = append(events, types.StreamEvent{Type: types.EvToolCallEnd, ContentIndex: i, ToolCall: &assembled, Partial: output})
		}
	}
	return events
}

// toolCallBlockByContentIndex finds the fold block whose output lives at contentIndex.
func (s *openAIFoldState) toolCallBlockByContentIndex(contentIndex int) *openAIFoldBlock {
	for _, b := range s.ToolCallByIndex {
		if b.ContentIndex == contentIndex {
			return b
		}
	}
	for _, b := range s.ToolCallByID {
		if b.ContentIndex == contentIndex {
			return b
		}
	}
	return nil
}

// streamingJSONRaw repairs a partial argument buffer into best-effort JSON for
// incremental delivery, returning an empty object on failure.
func streamingJSONRaw(partial string) json.RawMessage {
	parsed := jsonrepair.ParseStreamingJSON(partial)
	raw, _ := json.Marshal(parsed)
	if raw == nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// finalJSONRaw parses the complete argument buffer into final JSON, returning an
// empty object when empty or unparseable.
func finalJSONRaw(partial string) json.RawMessage {
	if strings.TrimSpace(partial) == "" {
		return json.RawMessage(`{}`)
	}
	var parsed map[string]any
	if err := jsonrepair.ParseJsonWithRepair(partial, &parsed); err != nil || parsed == nil {
		return json.RawMessage(`{}`)
	}
	raw, _ := json.Marshal(parsed)
	if raw == nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// firstNonEmptyStr returns the first value that is not blank after trimming.
func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
