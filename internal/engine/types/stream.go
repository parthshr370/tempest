package types

import "time"

// StreamEventType is the discriminant of a streaming event. Consumers switch on
// it, so a tagged struct (below) is used rather than an interface sum.
type StreamEventType string

const (
	EvStart         StreamEventType = "start"
	EvTextStart     StreamEventType = "text_start"
	EvTextDelta     StreamEventType = "text_delta"
	EvTextEnd       StreamEventType = "text_end"
	EvThinkingStart StreamEventType = "thinking_start"
	EvThinkingDelta StreamEventType = "thinking_delta"
	EvThinkingEnd   StreamEventType = "thinking_end"
	EvToolCallStart StreamEventType = "toolcall_start"
	EvToolCallDelta StreamEventType = "toolcall_delta"
	EvToolCallEnd   StreamEventType = "toolcall_end"
	EvDone          StreamEventType = "done"
	EvError         StreamEventType = "error"
	// EvRetry reports that the attempt streamed so far failed with a transient
	// error and the same request is being sent again. Everything streamed since
	// the request started is discarded; the events that follow belong to the
	// next attempt.
	EvRetry StreamEventType = "retry"
	// EvFallback reports that the model streamed so far is unavailable and the
	// same request goes to the next model in the fallback chain. Like EvRetry,
	// everything streamed since the request started is discarded; the events
	// that follow belong to the backup model named in Fallback.To.
	EvFallback StreamEventType = "fallback"
	// EvAttempt reports that the request is being sent again: a retry after
	// its backoff, or a backup model's first attempt after a switch. It is
	// pushed right before the provider is called, so an attempt cancelled
	// during its backoff, or between a switch and the backup's call, is never
	// announced. Attempt is its 1-based ordinal on the current model. A
	// request's first attempt is not announced: it starts with the request.
	EvAttempt StreamEventType = "attempt"
)

// ModelFallback describes one switch to a backup model: the model that failed,
// the model that gets the request next, the availability failure that caused
// the switch, how many attempts the failed model got, and the backup's
// position in the chain (0 is the primary).
type ModelFallback struct {
	From     Model
	To       Model
	Reason   string
	Attempts int
	Index    int
}

// StreamEvent is one streaming event. Content arms carry Partial (a live
// pointer to the builder's message — read-only until the next event).
// Terminal arms carry either a final message and stop reason or a final error
// message. `*_end` text/thinking events carry the full Content, while
// toolcall_end carries the assembled ToolCall. A retry carries the discarded
// attempt in Err (its partial content, usage and error), its ordinal in
// Attempt and the wait before the next attempt in RetryDelay. A fallback
// carries the discarded attempt in Err and the model switch in Fallback. An
// attempt carries the ordinal of the attempt being sent in Attempt.
type StreamEvent struct {
	Type         StreamEventType
	ContentIndex int
	Delta        string            // text_delta / thinking_delta / toolcall_delta
	Content      string            // text_end / thinking_end (full content)
	ToolCall     *ContentBlock     // toolcall_end (assembled tool call)
	Reason       StopReason        // done / error
	Partial      *AssistantMessage // content arms
	Message      *AssistantMessage // done
	Err          *AssistantMessage // error / retry / fallback (the discarded attempt)
	Attempt      int               // retry: 1-based ordinal of the failed attempt; attempt: of the one being sent
	RetryDelay   time.Duration     // retry: wait before the next attempt
	Fallback     *ModelFallback    // fallback: the switch to the next model
}

// AssistantBuilder folds a stream of StreamEvents into an AssistantMessage. The
// `*_end` events are authoritative (they overwrite the accumulated block) to
// match their final content / assembled tool call.
type AssistantBuilder struct {
	msg AssistantMessage
}

// NewAssistantBuilder starts a builder tagged with provenance metadata.
func NewAssistantBuilder(api, provider, model string) *AssistantBuilder {
	return &AssistantBuilder{msg: AssistantMessage{API: api, Provider: provider, Model: model}}
}

// block returns a pointer to the block at index i, growing Content (with the
// given type for new slots) so folds can arrive by content index.
func (b *AssistantBuilder) block(i int, typ BlockType) *ContentBlock {
	for len(b.msg.Content) <= i {
		b.msg.Content = append(b.msg.Content, ContentBlock{Type: typ})
	}
	return &b.msg.Content[i]
}

// Fold applies one event to the message.
func (b *AssistantBuilder) Fold(ev StreamEvent) {
	switch ev.Type {
	case EvTextStart:
		b.block(ev.ContentIndex, BlockText)
	case EvTextDelta:
		b.block(ev.ContentIndex, BlockText).Text += ev.Delta
	case EvTextEnd:
		b.block(ev.ContentIndex, BlockText).Text = ev.Content // authoritative
	case EvThinkingStart:
		b.block(ev.ContentIndex, BlockThinking)
	case EvThinkingDelta:
		b.block(ev.ContentIndex, BlockThinking).Thinking += ev.Delta
	case EvThinkingEnd:
		b.block(ev.ContentIndex, BlockThinking).Thinking = ev.Content // authoritative
	case EvToolCallStart:
		blk := b.block(ev.ContentIndex, BlockToolCall)
		if ev.ToolCall != nil {
			blk.ID, blk.Name = ev.ToolCall.ID, ev.ToolCall.Name
		}
	case EvToolCallDelta:
		blk := b.block(ev.ContentIndex, BlockToolCall)
		blk.Arguments = append(blk.Arguments, ev.Delta...)
	case EvToolCallEnd:
		blk := b.block(ev.ContentIndex, BlockToolCall)
		if ev.ToolCall != nil {
			*blk = *ev.ToolCall // assembled tool call is authoritative
		}
	case EvDone:
		b.msg.StopReason = ev.Reason
	case EvError:
		b.msg.StopReason = ev.Reason
		if ev.Err != nil {
			if ev.Err.ErrorMessage != "" {
				b.msg.ErrorMessage = ev.Err.ErrorMessage
			}
			if ev.Err.ErrorCode != "" {
				b.msg.ErrorCode = ev.Err.ErrorCode
			}
			if ev.Err.ErrorDetails != nil {
				b.msg.ErrorDetails = ev.Err.ErrorDetails
			}
		}
	case EvRetry:
		b.msg = AssistantMessage{API: b.msg.API, Provider: b.msg.Provider, Model: b.msg.Model}
	case EvFallback:
		b.msg = AssistantMessage{API: b.msg.API, Provider: b.msg.Provider, Model: b.msg.Model}
		if ev.Fallback != nil {
			to := ev.Fallback.To
			b.msg = AssistantMessage{API: to.API, Provider: to.Provider, Model: to.ID}
		}
	}
}

// Message returns the accumulated message (a copy of the header; the Content
// slice is shared).
func (b *AssistantBuilder) Message() AssistantMessage { return b.msg }
