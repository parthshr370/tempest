// Package anthropic implements the Anthropic Messages API stream client. It
// uses the existing [internal/transport/sseparse] decoder, folds Anthropic's six
// SSE event types (message_start, content_block_*, message_delta, message_stop)
// into [types.StreamEvent]s, and surfaces them on an [stream.AssistantStream].
//
// The stream watchdog ([base.WatchStream]) cancels the request when no SSE
// event arrives within [streamTimeouts]. This prevents the silent-but-open-stream
// wedge that would pin 102% CPU on a truncated Anthropic response.
//
// [Stream] handles the full raw request lifecycle. Callers typically use
// [StreamSimple] which maps a reasoning level to Anthropic thinking config
// before delegating to Stream.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/engine/util"
	"go.harness.dev/harness/internal/provider/base"
)

// Stream sends a streaming request to the Anthropic Messages API and returns an
// [stream.AssistantStream] that the caller can range over. The request runs on
// its own goroutine; errors are pushed as terminal stream events via [pushError].
func Stream(ctx context.Context, model types.Model, c types.Context, opts *Options) *stream.AssistantStream {
	s := stream.NewAssistantStream(model.API, model.Provider, model.ID)
	go runStream(ctx, s, model, c, opts)
	return s
}

// StreamSimple maps a reasoning level to Anthropic thinking configuration,
// then delegates to [Stream]. When reasoning is empty, thinking is disabled.
// Otherwise ForceAdaptiveThinking models get the effort-mapped level and other
// models get a fixed thinking budget derived from the level and adjusted for
// context window limits.
func StreamSimple(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
	apiKey := ""
	var blobReader types.BlobReader
	if opts != nil {
		apiKey = opts.APIKey
		blobReader = opts.BlobReader
	}
	base := buildBaseOptions(model, c, opts, apiKey)
	if opts == nil || opts.Reasoning == "" || !model.Reasoning {
		return Stream(ctx, model, c, &Options{
			StreamOptions:   base,
			BlobReader:      blobReader,
			ThinkingEnabled: boolPtr(false),
		})
	}

	if model.Compat != nil && model.Compat.ForceAdaptiveThinking != nil && *model.Compat.ForceAdaptiveThinking {
		effort := mapThinkingLevelToEffort(model, opts.Reasoning)
		return Stream(ctx, model, c, &Options{
			StreamOptions:   base,
			BlobReader:      blobReader,
			ThinkingEnabled: boolPtr(true),
			Effort:          effort,
		})
	}

	maxTokens, thinkingBudget := adjustMaxTokensForThinking(base.MaxTokens, model.MaxTokens, opts.Reasoning, opts.ThinkingBudgets)
	maxTokens = clampMaxTokensToContext(model, c, maxTokens)
	return Stream(ctx, model, c, &Options{
		StreamOptions:        withMaxTokens(base, maxTokens),
		BlobReader:           blobReader,
		ThinkingEnabled:      boolPtr(true),
		ThinkingBudgetTokens: minInt(thinkingBudget, maxInt(0, maxTokens-1024)),
	})
}

// withMaxTokens returns a shallow copy of opts with MaxTokens set.
func withMaxTokens(opts types.StreamOptions, maxTokens int) types.StreamOptions {
	opts.MaxTokens = maxTokens
	return opts
}

// runStream is the goroutine body behind [Stream]. It manages the full HTTP
// lifecycle: auth checks, header assembly, JSON marshaling, the POST call,
// SSE iteration/folding, and the idle watchdog.
//
// The stream watchdog cancels the request context if no SSE event arrives
// within [streamTimeouts]. This is the fix for the 102%-CPU-wedge: a
// silently-hung Anthropic stream that never sends data and never errors. The
// watchdog reports a hung stream as a retryable stream interruption and leaves
// a canceled stream (parent ctx done) as the caller's cancellation.
func runStream(parent context.Context, s *stream.AssistantStream, model types.Model, c types.Context, opts *Options) {
	output := newOutputMessage(model)
	defer func() {
		if recovered := recover(); recovered != nil {
			pushError(s, output, fmt.Errorf("%v", recovered), false)
		}
	}()

	// streamCtx is a child of parent: the watchdog cancels streamCtx, but the
	// response SSE handler still checks parent so a parent cancel (e.g. /cancel)
	// is distinguishable from a watchdog cancel (stalled vs aborted).
	streamCtx, cancel := context.WithCancel(parent)
	defer cancel()
	watchdog := base.WatchStream(cancel, streamTimeouts)
	defer watchdog.Stop()

	if opts == nil {
		opts = &Options{}
	}
	if err := assertRequestAuth(model.Provider, opts.APIKey, opts.Headers); err != nil {
		pushError(s, output, err, parent.Err() != nil)
		return
	}
	interleavedThinking := true
	if opts.InterleavedThinking != nil {
		interleavedThinking = *opts.InterleavedThinking
	}
	cacheRetention := resolveCacheRetention(opts.CacheRetention, opts.Env)
	cacheSessionID := opts.SessionID
	if cacheRetention == cacheRetentionNone {
		cacheSessionID = ""
	}
	headers, isOAuth := buildRequestHeaders(model, opts.APIKey, interleavedThinking, shouldUseFineGrainedToolStreamingBeta(model, c), opts.Headers, cacheSessionID)
	params, err := buildParams(streamCtx, model, c, isOAuth, opts)
	if err != nil {
		pushError(s, output, err, parent.Err() != nil)
		return
	}
	payload := any(params)
	if opts.OnPayload != nil {
		if next, err := opts.OnPayload(payload, model); err != nil {
			pushError(s, output, err, parent.Err() != nil)
			return
		} else if next != nil {
			payload = next
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		pushError(s, output, err, parent.Err() != nil)
		return
	}
	if int64(len(body)) > maxAnthropicRequestBytes {
		pushError(s, output, wholeRequestSizeError(len(body)), parent.Err() != nil)
		return
	}
	reqCtx := streamCtx
	if opts.TimeoutMs > 0 {
		var timeoutCancel context.CancelFunc
		reqCtx, timeoutCancel = context.WithTimeout(streamCtx, time.Duration(opts.TimeoutMs)*time.Millisecond)
		defer timeoutCancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, messagesURL(model.BaseURL), bytes.NewReader(body))
	if err != nil {
		pushError(s, output, err, parent.Err() != nil)
		return
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		pushError(s, output, watchdog.Err(parent.Err(), err), parent.Err() != nil)
		return
	}
	defer resp.Body.Close()
	if opts.OnResponse != nil {
		if err := opts.OnResponse(types.ProviderResponse{Status: resp.StatusCode, Headers: util.HeadersToRecord(resp.Header)}, model); err != nil {
			pushError(s, output, err, parent.Err() != nil)
			return
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(resp.Body)
		pushError(s, output, base.ResponseError(resp, responseBody), parent.Err() != nil)
		return
	}

	s.Push(types.StreamEvent{Type: types.EvStart, Partial: output})
	state := &foldState{Output: output, IsOAuth: isOAuth, Tools: c.Tools}
	err = iterateAnthropicEvents(reqCtx, resp.Body, watchdog.Event, func(event rawAnthropicEvent) error {
		events, err := foldAnthropicEvent(event, state, model)
		if err != nil {
			return err
		}
		if len(events) > 0 {
			watchdog.Output()
		}
		for _, event := range events {
			s.Push(event)
		}
		return nil
	})
	if err != nil {
		pushError(s, output, watchdog.Err(parent.Err(), err), parent.Err() != nil)
		return
	}
	if parent.Err() != nil {
		pushError(s, output, parent.Err(), true)
		return
	}
	if output.StopReason == types.StopAborted || output.StopReason == types.StopError {
		message := output.ErrorMessage
		if message == "" {
			message = "An unknown error occurred"
		}
		pushError(s, output, errors.New(message), output.StopReason == types.StopAborted)
		return
	}
	s.Push(types.StreamEvent{Type: types.EvDone, Reason: output.StopReason, Message: output})
}

// newOutputMessage creates the [types.AssistantMessage] that accumulates
// stream events. It initializes an empty Content slice so foldContentBlockStart
// can append; Timestamp is set at creation.
func newOutputMessage(model types.Model) *types.AssistantMessage {
	return &types.AssistantMessage{
		Content:    []types.ContentBlock{},
		API:        model.API,
		Provider:   model.Provider,
		Model:      model.ID,
		Usage:      types.Usage{Cost: types.Cost{Unknown: model.Cost.Unknown}},
		StopReason: types.StopStop,
		Timestamp:  time.Now().UnixMilli(),
	}
}

// pushError emits a terminal error event on the stream. aborted=true means the
// error was caused by context cancellation (user-initiated /cancel); aborted=false
// means a genuine stream error.
func pushError(s *stream.AssistantStream, output *types.AssistantMessage, err error, aborted bool) {
	if aborted {
		output.StopReason = types.StopAborted
	} else {
		output.StopReason = types.StopError
	}
	base.ApplyError(output, err)
	s.Push(types.StreamEvent{Type: types.EvError, Reason: output.StopReason, Err: output})
}

// messagesURL returns the full Messages API endpoint for the given base URL.
func messagesURL(baseURL string) string {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultAnthropicBaseURL
	}
	return strings.TrimRight(baseURL, "/") + "/v1/messages"
}
