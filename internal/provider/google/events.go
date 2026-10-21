package google

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"go.harness.dev/harness/internal/engine/jsonrepair"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/transport/sseparse"
)

// toolCallIDCounter is a monotonic counter for synthesizing tool call IDs.
// It uses a pure atomic counter for deterministic test behavior.
var toolCallIDCounter atomic.Int64

// googleStreamChunk is one decoded Gemini SSE chunk.
type googleStreamChunk struct {
	ResponseID    string               `json:"responseId"`
	Candidates    []googleCandidate    `json:"candidates"`
	UsageMetadata *googleUsageMetadata `json:"usageMetadata"`
	Error         json.RawMessage      `json:"error"`
}

// googleCandidate is a single generation candidate within a chunk.
type googleCandidate struct {
	Content      *googleContent `json:"content"`
	FinishReason string         `json:"finishReason"`
}

// googleContent holds the parts (text, thoughts, function calls) of a candidate.
type googleContent struct {
	Parts []googlePart `json:"parts"`
}

// googlePart is one piece of candidate content: text, a thought, or a call.
type googlePart struct {
	Text             string          `json:"text"`
	Thought          bool            `json:"thought"`
	ThoughtSignature string          `json:"thoughtSignature"`
	FunctionCall     *googleFuncCall `json:"functionCall"`
}

// googleFuncCall is a Gemini function (tool) call with its arguments.
type googleFuncCall struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// googleUsageMetadata carries token accounting reported by Gemini.
type googleUsageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
}

// googleFoldBlock tracks an in-progress output block during folding.
type googleFoldBlock struct {
	Type         string // "text", "thinking", "toolCall"
	ContentIndex int
	ToolCallName string
}

// googleFoldState accumulates the assistant message and per-block bookkeeping as
// chunks are folded into stream events.
type googleFoldState struct {
	Output       *types.AssistantMessage
	Blocks       []googleFoldBlock
	CurrentBlock *googleFoldBlock
	ToolCallIDs  map[string]bool
}

// iterateGoogleEvents reads the SSE body, repairs and decodes each data chunk,
// and invokes fn per chunk; onActivity fires on every event for idle tracking.
func iterateGoogleEvents(ctx context.Context, r io.Reader, onActivity func(), fn func(*googleStreamChunk) error) error {
	return sseparse.IterateSSE(ctx, r, func(event sseparse.ServerSentEvent) error {
		if onActivity != nil {
			onActivity()
		}
		data := strings.TrimSpace(event.Data)
		if data == "" {
			return nil
		}
		var chunk googleStreamChunk
		if err := jsonrepair.ParseJsonWithRepair(data, &chunk); err != nil {
			return fmt.Errorf("could not parse Gemini SSE chunk: %s; data=%s", err.Error(), data)
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return fmt.Errorf("Gemini stream error: %s", chunk.Error)
		}
		return fn(&chunk)
	})
}

// foldGoogleChunk folds one chunk into state, appending content and returning the
// stream events it produced, and resolving the stop reason on the final chunk.
func foldGoogleChunk(chunk *googleStreamChunk, state *googleFoldState, model types.Model) ([]types.StreamEvent, error) {
	output := state.Output
	output.ResponseID = firstNonEmptyStr(output.ResponseID, chunk.ResponseID)

	var events []types.StreamEvent

	if chunk.UsageMetadata != nil {
		applyGoogleUsage(output, chunk.UsageMetadata, model)
	}

	for _, candidate := range chunk.Candidates {
		if candidate.Content != nil {
			for _, part := range candidate.Content.Parts {
				if part.FunctionCall != nil {
					evs := foldGoogleFuncCall(part, state)
					events = append(events, evs...)
					continue
				}

				if part.Text != "" {
					evs := foldGoogleTextPart(part, state)
					events = append(events, evs...)
				}
			}
		}

		if candidate.FinishReason != "" {
			output.StopReason = mapStopReason(candidate.FinishReason)
			if hasToolCalls(output) {
				output.StopReason = types.StopToolUse
			}
		}
	}

	return events, nil
}

// hasToolCalls reports whether output already contains a tool-call block.
func hasToolCalls(output *types.AssistantMessage) bool {
	for _, block := range output.Content {
		if block.Type == types.BlockToolCall {
			return true
		}
	}
	return false
}

// foldGoogleTextPart appends a text delta, opening a new text block (or routing
// to the thinking folder) when the current block is not a matching text block.
func foldGoogleTextPart(part googlePart, state *googleFoldState) []types.StreamEvent {
	output := state.Output
	isThinking := part.Thought

	if isThinking {
		return foldGoogleThinkingText(part, state)
	}

	currentBlock := state.CurrentBlock
	if currentBlock == nil || currentBlock.Type != "text" {
		state.CurrentBlock = nil
		contentIndex := len(output.Content)
		output.Content = append(output.Content, types.ContentBlock{Type: types.BlockText, Text: part.Text})
		state.CurrentBlock = &googleFoldBlock{Type: "text", ContentIndex: contentIndex}
		return []types.StreamEvent{
			{Type: types.EvTextStart, ContentIndex: contentIndex, Partial: output},
			{Type: types.EvTextDelta, ContentIndex: contentIndex, Delta: part.Text, Partial: output},
		}
	}

	blk := &output.Content[currentBlock.ContentIndex]
	blk.Text += part.Text
	return []types.StreamEvent{{Type: types.EvTextDelta, ContentIndex: currentBlock.ContentIndex, Delta: part.Text, Partial: output}}
}

// foldGoogleThinkingText appends a thinking delta, opening a new thinking block
// (carrying its thought signature) when the current block is not one.
func foldGoogleThinkingText(part googlePart, state *googleFoldState) []types.StreamEvent {
	output := state.Output
	currentBlock := state.CurrentBlock
	if currentBlock == nil || currentBlock.Type != "thinking" {
		state.CurrentBlock = nil
		contentIndex := len(output.Content)
		sig := part.ThoughtSignature
		output.Content = append(output.Content, types.ContentBlock{Type: types.BlockThinking, Thinking: part.Text, ThinkingSignature: sig})
		state.CurrentBlock = &googleFoldBlock{Type: "thinking", ContentIndex: contentIndex}
		return []types.StreamEvent{
			{Type: types.EvThinkingStart, ContentIndex: contentIndex, Partial: output},
			{Type: types.EvThinkingDelta, ContentIndex: contentIndex, Delta: part.Text, Partial: output},
		}
	}

	blk := &output.Content[currentBlock.ContentIndex]
	blk.Thinking += part.Text
	return []types.StreamEvent{{Type: types.EvThinkingDelta, ContentIndex: currentBlock.ContentIndex, Delta: part.Text, Partial: output}}
}

// foldGoogleFuncCall appends a tool-call block, synthesizing a unique ID when
// the model omits one or reuses an ID, and emits its start/delta/end events.
func foldGoogleFuncCall(part googlePart, state *googleFoldState) []types.StreamEvent {
	output := state.Output
	state.CurrentBlock = nil

	fc := part.FunctionCall
	name := fc.Name
	providedID := fc.ID

	needsNewID := providedID == "" || state.ToolCallIDs[providedID]
	toolCallID := providedID
	if needsNewID {
		counter := toolCallIDCounter.Add(1)
		toolCallID = fmt.Sprintf("%s_%d", name, counter)
	}
	if toolCallID != "" {
		if state.ToolCallIDs == nil {
			state.ToolCallIDs = map[string]bool{}
		}
		state.ToolCallIDs[toolCallID] = true
	}

	args := json.RawMessage(`{}`)
	if fc.Args != nil {
		if raw, err := json.Marshal(fc.Args); err == nil {
			args = raw
		}
	}

	contentIndex := len(output.Content)
	block := types.ContentBlock{Type: types.BlockToolCall, ID: toolCallID, Name: name, Arguments: args}
	if part.ThoughtSignature != "" {
		block.ThoughtSignature = part.ThoughtSignature
	}
	output.Content = append(output.Content, block)

	return []types.StreamEvent{
		{Type: types.EvToolCallStart, ContentIndex: contentIndex, ToolCall: &block, Partial: output},
		{Type: types.EvToolCallDelta, ContentIndex: contentIndex, Delta: string(args), Partial: output},
		{Type: types.EvToolCallEnd, ContentIndex: contentIndex, ToolCall: &block, Partial: output},
	}
}

// foldGoogleFinalizeBlocks emits the closing end events for every text and
// thinking block once the stream completes.
func foldGoogleFinalizeBlocks(state *googleFoldState) []types.StreamEvent {
	var events []types.StreamEvent
	output := state.Output
	state.CurrentBlock = nil
	for i := range output.Content {
		blk := &output.Content[i]
		switch blk.Type {
		case types.BlockText:
			events = append(events, types.StreamEvent{Type: types.EvTextEnd, ContentIndex: i, Content: blk.Text, Partial: output})
		case types.BlockThinking:
			events = append(events, types.StreamEvent{Type: types.EvThinkingEnd, ContentIndex: i, Content: blk.Thinking, Partial: output})
		}
	}
	return events
}

// applyGoogleUsage maps Gemini usage metadata onto output.Usage (subtracting
// cached tokens from input, folding thoughts into output) and computes cost.
func applyGoogleUsage(output *types.AssistantMessage, usage *googleUsageMetadata, model types.Model) {
	output.Usage = types.Usage{
		Input:       usage.PromptTokenCount - usage.CachedContentTokenCount,
		Output:      usage.CandidatesTokenCount + usage.ThoughtsTokenCount,
		CacheRead:   usage.CachedContentTokenCount,
		CacheWrite:  0,
		TotalTokens: usage.TotalTokenCount,
		Cost:        types.Cost{},
	}
	if usage.ThoughtsTokenCount > 0 {
		reasoning := usage.ThoughtsTokenCount
		output.Usage.Reasoning = &reasoning
	}
	types.CalculateCost(model.Cost, &output.Usage)
}

// firstNonEmptyStr returns the first non-blank value, or "" if all are blank.
func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
