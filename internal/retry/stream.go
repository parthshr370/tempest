package retry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
)

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

// Stream starts one logical stream over as many attempts as the policy allows.
func (r *Retrier) Stream(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
	output := stream.NewAssistantStream(model.API, model.Provider, model.ID)
	go r.produce(ctx, output, model, c, opts)
	return output
}

// produce sends the request until an attempt completes, fails terminally, runs
// out of attempts, or ctx ends. Every attempt's events are forwarded as they
// arrive. An attempt that fails transiently, before or after it produced
// output, is announced with a [types.EvRetry] event carrying the discarded
// attempt, and the same request (same context, same options) is sent again
// after the backoff. Re-sending after output was forwarded is safe because
// nothing acts on a response before its terminal event: the agent loop runs
// tool calls only after the stream reports done.
func (r *Retrier) produce(ctx context.Context, output *stream.AssistantStream, model types.Model, c types.Context, opts *types.SimpleStreamOptions) {
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
		failed, classified := r.forwardAttempt(ctx, output, model, logical, r.next.Stream(ctx, model, c, attemptOpts), &response)
		if classified == nil {
			return
		}
		discarded := attemptMessage(model, logical, failed.Err, classified)
		delay, terminal := r.plan(attempt, classified)
		if terminal != nil {
			if terminal == classified && failed.Err != nil {
				// A terminal provider failure reaches the caller as the provider sent it.
				output.Push(failed)
			} else {
				pushTerminal(output, terminal, discarded)
			}
			return
		}
		retry := types.StreamEvent{Type: types.EvRetry, Err: discarded, Attempt: attempt, RetryDelay: delay}
		logical.Fold(retry)
		output.Push(retry)
		if err := r.cfg.Sleeper.Sleep(ctx, delay); err != nil || ctx.Err() != nil {
			r.pushCanceled(output, model, ctx.Err())
			return
		}
	}
}

// forwardAttempt forwards one attempt's events. It returns a nil classification
// once a terminal event has been pushed downstream (success or cancellation);
// otherwise it returns the attempt's terminal error event, if the provider sent
// one, and the classified failure.
func (r *Retrier) forwardAttempt(ctx context.Context, output *stream.AssistantStream, model types.Model, logical *types.AssistantBuilder, attemptStream *stream.AssistantStream, response *types.ProviderResponse) (types.StreamEvent, *Error) {
	if attemptStream == nil {
		return types.StreamEvent{}, Classify(Failure{Message: "provider stream was nil", StopReason: types.StopError, Status: response.Status, Headers: response.Headers, ContextErr: ctx.Err()}, r.cfg.Clock.Now())
	}
	for {
		select {
		case <-ctx.Done():
			r.pushCanceled(output, model, ctx.Err())
			return types.StreamEvent{}, nil
		case event, ok := <-attemptStream.Events():
			if !ok {
				if ctx.Err() != nil {
					r.pushCanceled(output, model, ctx.Err())
					return types.StreamEvent{}, nil
				}
				cause := errors.New("provider stream ended without terminal event")
				return types.StreamEvent{}, &Error{Code: CodeStreamInterrupted, Message: cause.Error(), Retryable: true, RetryAfter: retryAfterOrZero(response.Headers, r.cfg.Clock.Now()), Cause: cause}
			}
			event = copyEvent(event)
			switch event.Type {
			case types.EvDone:
				output.Push(event)
				return types.StreamEvent{}, nil
			case types.EvError:
				failure := Failure{StopReason: event.Reason, Status: response.Status, Headers: response.Headers, ContextErr: ctx.Err()}
				if event.Err != nil {
					failure.Message, failure.ProviderCode = event.Err.ErrorMessage, event.Err.ErrorCode
				}
				if strings.TrimSpace(failure.Message) == "" {
					failure.Message = fmt.Sprintf("provider failure: %s", failure.ProviderCode)
				}
				return event, Classify(failure, r.cfg.Clock.Now())
			default:
				forward(output, logical, event)
			}
		}
	}
}

// plan returns the wait before the failed attempt is sent again, or the error
// that ends the stream instead.
func (r *Retrier) plan(attempt int, classified *Error) (time.Duration, *Error) {
	if !classified.Retryable {
		return 0, classified
	}
	if attempt >= r.cfg.MaxAttempts {
		return 0, &Error{Code: CodeAttemptsExhausted, Message: fmt.Sprintf("retry attempts exhausted after %d attempts: %s", attempt, classified.Error()), Attempts: attempt, Cause: classified.Cause}
	}
	delay := jitterDelay(nominalDelay(r.cfg.BaseDelay, r.cfg.BackoffCap, attempt), r.cfg.Jitter, r.cfg.Random)
	if classified.RetryAfter > delay {
		delay = classified.RetryAfter
	}
	if r.cfg.MaxDelay > 0 && delay > r.cfg.MaxDelay {
		return 0, &Error{Code: CodeRetryDelayExceeded, Message: fmt.Sprintf("retry delay %s exceeds the configured maximum %s: %s", delay, r.cfg.MaxDelay, classified.Error()), Attempts: attempt, Cause: classified.Cause}
	}
	return delay, nil
}

// attemptMessage is the record of a failed attempt: the provider's terminal
// message when it sent one (its partial content and any usage it reported),
// else the content forwarded before the stream broke.
func attemptMessage(model types.Model, logical *types.AssistantBuilder, providerMessage *types.AssistantMessage, classified *Error) *types.AssistantMessage {
	var message *types.AssistantMessage
	if providerMessage != nil {
		message = cloneMessage(providerMessage)
	} else {
		message = cloneMessagePtr(logical.Message())
	}
	message.StopReason = types.StopError
	if message.ErrorCode == "" {
		message.ErrorCode = string(classified.Code)
	}
	if message.ErrorMessage == "" {
		message.ErrorMessage = classified.Error()
	}
	message.Usage.Cost.Unknown = message.Usage.Cost.Unknown || model.Cost.Unknown
	return message
}

func (r *Retrier) pushCanceled(output *stream.AssistantStream, model types.Model, cause error) {
	pushTerminal(output, &Error{Code: CodeCanceled, Message: "retry canceled", Cause: cause}, &types.AssistantMessage{
		API: model.API, Provider: model.Provider, Model: model.ID,
		Usage: types.Usage{Cost: types.Cost{Unknown: model.Cost.Unknown}},
	})
}

// pushTerminal ends the stream with message carrying the retry outcome.
func pushTerminal(output *stream.AssistantStream, classified *Error, message *types.AssistantMessage) {
	reason := types.StopError
	if classified.Code == CodeCanceled {
		reason = types.StopAborted
	}
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
