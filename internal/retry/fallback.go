package retry

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
)

// Candidate is one model in a fallback chain and the stream that serves it,
// normally that model's own [Retrier].
type Candidate struct {
	Model  types.Model
	Stream Streamer
}

// Fallback sends a request to the next model in its chain when the current
// model is unavailable. It wraps whole retry chains: a model counts as
// unavailable only after its own retry policy gave up on an availability
// failure (see [Availability]). Configuration failures (400, 401, 403,
// context overflow) and cancellation end the stream as the model reported
// them, so they surface instead of being masked by a backup model.
//
// Fallback is stateless. A caller that wants the switch to stick passes the
// backup model it was told about (the [types.EvFallback] event's To) on its
// next request; the chain then starts at that model.
type Fallback struct {
	chain []Candidate
}

// NewFallback validates a chain whose first candidate is the primary model.
func NewFallback(chain []Candidate) (*Fallback, error) {
	if len(chain) == 0 {
		return nil, errors.New("fallback chain needs at least one model")
	}
	for _, candidate := range chain {
		if nilDependency(candidate.Stream) {
			return nil, errors.New("fallback chain stream is required")
		}
	}
	return &Fallback{chain: append([]Candidate(nil), chain...)}, nil
}

// Models returns the chain's models, primary first.
func (f *Fallback) Models() []types.Model {
	models := make([]types.Model, len(f.chain))
	for i, candidate := range f.chain {
		models[i] = candidate.Model
	}
	return models
}

// Stream sends the request to model and, while the answering model is
// unavailable, to each later model in the chain. A model that is not in the
// chain is served by the primary's stream, as a single-model route would.
// Every switch is announced with a [types.EvFallback] event, then admitted
// through opts.AdmitFallback when set, before the backup model is called; the
// backup's call is announced with a [types.EvAttempt] event. A caller that
// records switches uses admission so a crash can never leave a backup's
// request without its recorded switch.
func (f *Fallback) Stream(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
	output := stream.NewAssistantStream(model.API, model.Provider, model.ID)
	go f.produce(ctx, output, f.index(model), model, c, opts)
	return output
}

// index finds the chain position of model by its route identity.
func (f *Fallback) index(model types.Model) int {
	for i, candidate := range f.chain {
		if SameModel(candidate.Model, model) {
			return i
		}
	}
	return 0
}

// SameModel reports whether two models name the same route: provider, wire
// protocol, model id and endpoint.
func SameModel(a, b types.Model) bool {
	return a.Provider == b.Provider && a.API == b.API && a.ID == b.ID && a.BaseURL == b.BaseURL
}

func (f *Fallback) produce(ctx context.Context, output *stream.AssistantStream, index int, model types.Model, c types.Context, opts *types.SimpleStreamOptions) {
	// The caller's credentials were issued for the model it asked for; the
	// provider route of a backup drops them (see types.StreamOptions).
	issued := types.RouteOf(model)
	if opts != nil && opts.CredentialRoute != (types.Route{}) {
		issued = opts.CredentialRoute
	}
	for {
		var status int
		attemptOpts := cloneOptions(opts, func(received types.ProviderResponse, receivedModel types.Model) error {
			status = received.Status
			if opts != nil && opts.OnResponse != nil {
				return opts.OnResponse(received, receivedModel)
			}
			return nil
		})
		attemptOpts.CredentialRoute = issued
		// Admission belongs to this chain's switches, not to anything the
		// candidate's stream might chain itself.
		attemptOpts.AdmitFallback = nil
		terminal, attempts, done := forwardModel(ctx, output, model, f.chain[index].Stream.Stream(ctx, model, c, attemptOpts))
		if done {
			return
		}
		reason, available := Availability(terminal.Err, status, ctx.Err())
		if !available || index+1 >= len(f.chain) {
			output.Push(terminal)
			return
		}
		index++
		next := f.chain[index].Model
		discarded := terminal.Err
		if discarded == nil {
			discarded = &types.AssistantMessage{API: model.API, Provider: model.Provider, Model: model.ID, StopReason: types.StopError, ErrorCode: string(reason), Timestamp: time.Now().UnixMilli()}
		}
		fallback := types.ModelFallback{From: model, To: next, Reason: string(reason), Attempts: attempts, Index: index}
		output.Push(types.StreamEvent{Type: types.EvFallback, Err: discarded, Fallback: &fallback})
		model = next
		// The backup is called only once the caller has admitted the switch
		// (the agent loop journals it first) and nothing cancelled the request
		// meanwhile; only then is its first attempt announced.
		if opts != nil && opts.AdmitFallback != nil {
			if err := opts.AdmitFallback(ctx, fallback); err != nil && ctx.Err() == nil {
				output.Push(failedEvent(model, CodeProviderFailure, "switch to backup model "+next.Provider+":"+next.ID+" not admitted: "+err.Error()))
				return
			}
		}
		if ctx.Err() != nil {
			output.Push(failedEvent(model, CodeCanceled, "fallback canceled"))
			return
		}
		output.Push(types.StreamEvent{Type: types.EvAttempt, Attempt: 1})
	}
}

// forwardModel forwards one model's events until its terminal event. It
// reports done once a success or cancellation has been pushed; otherwise it
// returns the model's terminal error event, unforwarded, with the number of
// attempts the model got (its retries plus the final one).
func forwardModel(ctx context.Context, output *stream.AssistantStream, model types.Model, source *stream.AssistantStream) (types.StreamEvent, int, bool) {
	attempts := 1
	if source == nil {
		return failedEvent(model, CodeStreamInterrupted, "provider stream was nil"), attempts, false
	}
	for event := range source.Events() {
		switch event.Type {
		case types.EvDone:
			output.Push(event)
			return types.StreamEvent{}, attempts, true
		case types.EvError:
			if ctx.Err() != nil || event.Reason == types.StopAborted {
				output.Push(event)
				return types.StreamEvent{}, attempts, true
			}
			return event, attempts, false
		case types.EvRetry:
			attempts++
			output.Push(event)
		default:
			output.Push(event)
		}
	}
	if ctx.Err() != nil {
		output.Push(failedEvent(model, CodeCanceled, "retry canceled"))
		return types.StreamEvent{}, attempts, true
	}
	return failedEvent(model, CodeStreamInterrupted, "provider stream ended without terminal event"), attempts, false
}

func failedEvent(model types.Model, code Code, message string) types.StreamEvent {
	reason := types.StopError
	if code == CodeCanceled {
		reason = types.StopAborted
	}
	return types.StreamEvent{Type: types.EvError, Reason: reason, Err: &types.AssistantMessage{
		API: model.API, Provider: model.Provider, Model: model.ID, Content: []types.ContentBlock{},
		StopReason: reason, ErrorCode: string(code), ErrorMessage: message, Timestamp: time.Now().UnixMilli(),
		Usage: types.Usage{Cost: types.Cost{Unknown: model.Cost.Unknown}},
	}}
}

// Availability decides whether a model's final failure means the model is
// unavailable, so a backup model may answer the same request instead. It
// returns the failure's category either way.
//
// Unavailable: a retry chain that ran out of attempts or exceeded its delay
// cap on a transient failure (5xx, 529, 429, network error, timeout,
// including no first event before the stream watchdog fired), the same
// transient failures when the route has no retry policy, and an HTTP 404
// whose body is the provider's structured missing-model answer
// (model_not_found; see isModelNotFound). Never unavailable: cancellation,
// authentication (401, 403), a rejected request (400 and other 4xx, whatever
// the body says about models), context overflow, or an unknown failure.
// Those are configuration or input errors that a different model would hide.
func Availability(message *types.AssistantMessage, status int, ctxErr error) (Code, bool) {
	failure := Failure{Status: status, ContextErr: ctxErr, StopReason: types.StopError}
	if message != nil {
		failure.Message, failure.ProviderCode, failure.StopReason = message.ErrorMessage, message.ErrorCode, message.StopReason
	}
	switch Code(strings.TrimSpace(failure.ProviderCode)) {
	case CodeAttemptsExhausted, CodeRetryDelayExceeded:
		if ctxErr != nil {
			return CodeCanceled, false
		}
		// The retry chain only gives up on transient failures; name the
		// failure the last attempt saw, not the retry outcome.
		failure.ProviderCode = ""
		classified := Classify(failure, time.Time{})
		if !classified.Retryable {
			return Code(strings.TrimSpace(message.ErrorCode)), true
		}
		return classified.Code, true
	}
	classified := Classify(failure, time.Time{})
	if classified.Code == CodeModelNotFound {
		return classified.Code, true
	}
	return classified.Code, classified.Retryable
}
