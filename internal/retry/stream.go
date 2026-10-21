package retry

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
)

const preOutputLimit = 64

// Streamer is the provider boundary decorated by [Retrier].
type Streamer interface {
	Stream(context.Context, types.Model, types.Context, *types.SimpleStreamOptions) *stream.AssistantStream
}

// StreamFunc adapts a stream function to [Streamer].
type StreamFunc func(context.Context, types.Model, types.Context, *types.SimpleStreamOptions) *stream.AssistantStream

// Stream invokes f.
func (f StreamFunc) Stream(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
	return f(ctx, model, c, opts)
}

// Retrier owns immutable retry policy around a provider stream function.
type Retrier struct {
	next Streamer
	cfg  Config
}

// New validates a provider stream boundary and immutable retry configuration.
func New(next Streamer, cfg Config) (*Retrier, error) {
	if nilDependency(next) {
		return nil, errors.New("retry stream source is required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Retrier{next: next, cfg: cfg}, nil
}

// Stream starts one logical stream whose failed pre-output attempts remain hidden.
func (r *Retrier) Stream(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
	output := stream.NewAssistantStream(model.API, model.Provider, model.ID)
	go r.produce(ctx, output, model, c, opts)
	return output
}

// produce keeps each attempt private until it has observable output, since we can't retry after the caller sees a response.
func (r *Retrier) produce(ctx context.Context, output *stream.AssistantStream, model types.Model, c types.Context, opts *types.SimpleStreamOptions) {
	if ctx.Err() != nil {
		r.pushCanceled(output, model, ctx.Err())
		return
	}
	logical := types.NewAssistantBuilder(model.API, model.Provider, model.ID)
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			r.pushCanceled(output, model, ctx.Err())
			return
		}
		var response types.ProviderResponse
		attemptOpts := cloneOptions(opts, func(received types.ProviderResponse, receivedModel types.Model) error {
			response = copyResponse(received)
			if opts != nil && opts.OnResponse != nil {
				return opts.OnResponse(received, receivedModel)
			}
			return nil
		})
		attemptStream := r.next.Stream(ctx, model, c, attemptOpts)
		if attemptStream == nil {
			if r.retryFailure(ctx, output, model, attempt, response, nil, types.StopError, logical, nil, "provider stream was nil") {
				continue
			}
			return
		}
		buffer := make([]types.StreamEvent, 0, preOutputLimit)
		committed := false
		terminal := false
		for !terminal {
			select {
			case <-ctx.Done():
				r.pushCanceled(output, model, ctx.Err())
				return
			case event, ok := <-attemptStream.Events():
				if !ok {
					if committed {
						r.pushGeneratedWithMessage(output, model, &Error{Code: CodeStreamInterrupted, Message: "provider stream ended without terminal event", Retryable: false, Cause: errors.New("provider stream ended without terminal event")}, logical.Message())
						return
					}
					if ctx.Err() != nil {
						r.pushCanceled(output, model, ctx.Err())
						return
					}
					interrupted := &Error{Code: CodeStreamInterrupted, Message: "provider stream ended without terminal event", Retryable: true, RetryAfter: retryAfterOrZero(response.Headers, r.cfg.Clock.Now()), Cause: errors.New("provider stream ended without terminal event")}
					if r.retryClassified(ctx, output, model, attempt, interrupted, logical, buffer, nil, types.StopError) {
						terminal = true
						break
					}
					return
				}
				event = copyEvent(event)
				switch event.Type {
				case types.EvDone:
					flush(output, logical, buffer)
					output.Push(event)
					return
				case types.EvError:
					if committed || eventHasObservableOutput(event) {
						flush(output, logical, buffer)
						output.Push(event)
						return
					}
					message, code, reason := terminalFields(event)
					if r.retryFailure(ctx, output, model, attempt, response, event.Err, reason, logical, buffer, joinProviderMessage(code, message)) {
						terminal = true
						break
					}
					return
				default:
					if !committed && (eventObservable(event) || len(buffer)+1 >= preOutputLimit) {
						committed = true
						flush(output, logical, buffer)
						buffer = nil
					}
					if committed {
						forward(output, logical, event)
					} else {
						buffer = append(buffer, event)
					}
				}
			}
		}
	}
}

// retryFailure classifies a provider failure and applies the retry policy.
func (r *Retrier) retryFailure(ctx context.Context, output *stream.AssistantStream, model types.Model, attempt int, response types.ProviderResponse, errMessage *types.AssistantMessage, reason types.StopReason, logical *types.AssistantBuilder, buffer []types.StreamEvent, fallback string) bool {
	message, code := fallback, ""
	if errMessage != nil {
		message, code = errMessage.ErrorMessage, errMessage.ErrorCode
	}
	classified := Classify(Failure{Message: message, ProviderCode: code, StopReason: reason, Status: response.Status, Headers: response.Headers, ContextErr: ctx.Err()}, r.cfg.Clock.Now())
	return r.retryClassified(ctx, output, model, attempt, classified, logical, buffer, errMessage, reason)
}

// retryClassified applies the retry/backoff/exhaustion policy to an already
// classified failure. It returns true when the caller should start the next
// attempt. A non-nil errMessage preserves the original provider terminal event
// on a terminal outcome.
func (r *Retrier) retryClassified(ctx context.Context, output *stream.AssistantStream, model types.Model, attempt int, classified *Error, logical *types.AssistantBuilder, buffer []types.StreamEvent, errMessage *types.AssistantMessage, reason types.StopReason) bool {
	if !classified.Retryable {
		flush(output, logical, buffer)
		if errMessage != nil {
			output.Push(types.StreamEvent{Type: types.EvError, Reason: reason, Err: errMessage})
		} else {
			r.pushGenerated(output, model, classified)
		}
		return false
	}
	if attempt >= r.cfg.MaxAttempts {
		r.pushGenerated(output, model, &Error{Code: CodeAttemptsExhausted, Message: "retry attempts exhausted", Attempts: attempt, Cause: classified.Cause})
		return false
	}
	local := jitterDelay(nominalDelay(r.cfg.BaseDelay, r.cfg.BackoffCap, attempt), r.cfg.Jitter, r.cfg.Random)
	delay := local
	if classified.RetryAfter > delay {
		delay = classified.RetryAfter
	}
	if r.cfg.MaxDelay > 0 && delay > r.cfg.MaxDelay {
		r.pushGenerated(output, model, &Error{Code: CodeRetryDelayExceeded, Message: "retry delay exceeds configured maximum", Attempts: attempt, Cause: classified.Cause})
		return false
	}
	if err := r.cfg.Sleeper.Sleep(ctx, delay); err != nil || ctx.Err() != nil {
		r.pushCanceled(output, model, ctx.Err())
		return false
	}
	return true
}

func (r *Retrier) pushCanceled(output *stream.AssistantStream, model types.Model, cause error) {
	r.pushGenerated(output, model, &Error{Code: CodeCanceled, Message: "retry canceled", Cause: cause})
}

func (r *Retrier) pushGenerated(output *stream.AssistantStream, model types.Model, classified *Error) {
	reason := types.StopError
	if classified.Code == CodeCanceled {
		reason = types.StopAborted
	}
	output.Push(types.StreamEvent{Type: types.EvError, Reason: reason, Err: &types.AssistantMessage{
		API: model.API, Provider: model.Provider, Model: model.ID, StopReason: reason,
		ErrorCode: string(classified.Code), ErrorMessage: classified.Error(),
		Usage: types.Usage{Cost: types.Cost{Unknown: model.Cost.Unknown}},
	}})
}

// pushGeneratedWithMessage preserves committed output while replacing only its terminal fields with the retry outcome.
func (r *Retrier) pushGeneratedWithMessage(output *stream.AssistantStream, model types.Model, classified *Error, partial types.AssistantMessage) {
	reason := types.StopError
	if classified.Code == CodeCanceled {
		reason = types.StopAborted
	}
	message := cloneMessagePtr(partial)
	message.Usage.Cost.Unknown = message.Usage.Cost.Unknown || model.Cost.Unknown
	message.StopReason = reason
	message.ErrorCode = string(classified.Code)
	message.ErrorMessage = classified.Error()
	output.Push(types.StreamEvent{Type: types.EvError, Reason: reason, Err: message})
}

// cloneOptions gives each attempt private mutable options while wrapping the response hook to retain retry metadata.
func cloneOptions(opts *types.SimpleStreamOptions, onResponse func(types.ProviderResponse, types.Model) error) *types.SimpleStreamOptions {
	if opts == nil {
		return &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{OnResponse: onResponse}}
	}
	clone := *opts
	if opts.Headers != nil {
		clone.Headers = make(map[string]string, len(opts.Headers))
		for key, value := range opts.Headers {
			clone.Headers[key] = value
		}
	}
	if opts.ThinkingBudgets != nil {
		clone.ThinkingBudgets = make(map[string]int, len(opts.ThinkingBudgets))
		for key, value := range opts.ThinkingBudgets {
			clone.ThinkingBudgets[key] = value
		}
	}
	if opts.Metadata != nil {
		clone.Metadata = make(map[string]any, len(opts.Metadata))
		for key, value := range opts.Metadata {
			clone.Metadata[key] = value
		}
	}
	if opts.Env != nil {
		clone.Env = make(map[string]string, len(opts.Env))
		for key, value := range opts.Env {
			clone.Env[key] = value
		}
	}
	clone.OnResponse = onResponse
	return &clone
}

// copyResponse keeps retry hints stable after control returns to the provider.
func copyResponse(response types.ProviderResponse) types.ProviderResponse {
	copy := types.ProviderResponse{Status: response.Status}
	if response.Headers != nil {
		copy.Headers = make(map[string]string, len(response.Headers))
		for key, value := range response.Headers {
			copy.Headers[key] = value
		}
	}
	return copy
}

// copyEvent strips provider-owned live state and snapshots everything the retry loop can retain across attempts.
func copyEvent(event types.StreamEvent) types.StreamEvent {
	event.Partial = nil
	event.Message = cloneMessage(event.Message)
	event.Err = cloneMessage(event.Err)
	if event.ToolCall != nil {
		tool := cloneBlock(*event.ToolCall)
		event.ToolCall = &tool
	}
	return event
}

// cloneMessage takes ownership of nested response data before the provider can mutate its builder again.
func cloneMessage(message *types.AssistantMessage) *types.AssistantMessage {
	if message == nil {
		return nil
	}
	clone := *message
	clone.Content = cloneBlocks(message.Content)
	if message.ErrorDetails != nil {
		clone.ErrorDetails = make(map[string]any, len(message.ErrorDetails))
		for key, value := range message.ErrorDetails {
			clone.ErrorDetails[key] = value
		}
	}
	return &clone
}

func cloneBlocks(blocks []types.ContentBlock) []types.ContentBlock {
	if blocks == nil {
		return nil
	}
	clone := make([]types.ContentBlock, len(blocks))
	for i := range blocks {
		clone[i] = cloneBlock(blocks[i])
	}
	return clone
}

func cloneBlock(block types.ContentBlock) types.ContentBlock {
	clone := block
	clone.Arguments = append([]byte(nil), block.Arguments...)
	return clone
}

// flush commits buffered pre-output events only once this attempt has become visible.
func flush(output *stream.AssistantStream, logical *types.AssistantBuilder, events []types.StreamEvent) {
	for _, event := range events {
		forward(output, logical, event)
	}
}

// forward rebuilds each partial snapshot from retry-owned state before handing it to the caller.
func forward(output *stream.AssistantStream, logical *types.AssistantBuilder, event types.StreamEvent) {
	// event.Partial is a live pointer into the provider's builder, mutated
	// concurrently by the provider goroutine; never read it. Rebuild the
	// forwarded snapshot from the Retrier-owned logical builder, which is folded
	// only from this event's owned Delta/Content/ToolCall fields.
	event.Partial = nil
	logical.Fold(event)
	event.Partial = cloneMessagePtr(logical.Message())
	output.Push(event)
}

func cloneMessagePtr(message types.AssistantMessage) *types.AssistantMessage {
	return cloneMessage(&message)
}

// eventObservable reports whether an event makes this attempt visible and therefore ineligible for retry.
func eventObservable(event types.StreamEvent) bool {
	switch event.Type {
	case types.EvTextDelta, types.EvThinkingDelta:
		return event.Delta != ""
	case types.EvTextEnd, types.EvThinkingEnd:
		return event.Content != ""
	case types.EvToolCallStart, types.EvToolCallDelta, types.EvToolCallEnd:
		return true
	}
	return eventHasObservableOutput(event)
}

// eventHasObservableOutput reports whether a terminal event already contains output that must not be retried.
func eventHasObservableOutput(event types.StreamEvent) bool {
	// Only terminal events carry an owned, final message. A non-terminal event's
	// Partial is a live provider-owned pointer and must not be read; its content
	// is observed via the owned Delta/Content/ToolCall checks in eventObservable.
	var message *types.AssistantMessage
	switch event.Type {
	case types.EvError:
		message = event.Err
	case types.EvDone:
		message = event.Message
	}
	if message == nil {
		return false
	}
	for _, block := range message.Content {
		if block.Type == types.BlockToolCall || block.Text != "" || block.Thinking != "" {
			return true
		}
	}
	return false
}

func terminalFields(event types.StreamEvent) (string, string, types.StopReason) {
	if event.Err == nil {
		return "", "", event.Reason
	}
	return event.Err.ErrorMessage, event.Err.ErrorCode, event.Reason
}

func joinProviderMessage(code, message string) string {
	if strings.TrimSpace(message) != "" {
		return message
	}
	return fmt.Sprintf("provider failure: %s", code)
}
