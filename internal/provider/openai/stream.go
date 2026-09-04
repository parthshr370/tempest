// Package openai implements the OpenAI chat-completions streaming client,
// folding SSE chunks into assistant messages and provider stream events.
// Package openai implements the OpenAI chat-completions streaming client. It
// builds the request, iterates SSE data lines, folds deltas (text plus tool-call
// argument accumulation by index), and surfaces the result on an
// [stream.AssistantStream]. The configurable baseURL reaches
// OpenRouter, Together, Groq, and Gemini's OpenAI-compat endpoint.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/engine/util"
	"go.harness.dev/harness/internal/provider/base"
)

// Stream starts an OpenAI chat-completions request and returns an AssistantStream
// that is fed asynchronously. Errors are never returned; they arrive as an error
// event on the stream.
func Stream(ctx context.Context, model types.Model, c types.Context, opts *Options) *stream.AssistantStream {
	s := stream.NewAssistantStream(model.API, model.Provider, model.ID)
	go runStream(ctx, s, model, c, opts)
	return s
}

// StreamSimple wraps Stream with the simplified option surface: it clamps
// max tokens to the context window and maps the reasoning field before streaming.
func StreamSimple(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
	apiKey := ""
	if opts != nil {
		apiKey = opts.APIKey
	}
	base := buildBaseOptions(model, c, opts, apiKey)
	effort := ""
	if opts != nil {
		effort = opts.Reasoning
	}
	// The catalog decides how "thinking off" reaches the wire for this model.
	// Effort-SKU models like gpt-5.6-luna reject function tools on chat
	// completions unless reasoning_effort is explicitly "none" (mirrors omp's
	// reasoningDisableMode: "none-effort"); everything else just omits the field.
	if effort == "" && model.OpenAICompat != nil && model.OpenAICompat.ReasoningDisableMode == types.ReasoningDisableNoneEffort {
		effort = "none"
	}
	if effort != "" {
		return Stream(ctx, model, c, &Options{StreamOptions: base, ReasoningEffort: effort})
	}
	return Stream(ctx, model, c, &Options{StreamOptions: base})
}

// runStream drives the full request lifecycle on its own goroutine: build payload,
// issue the HTTP request, fold the SSE stream, and push the terminal event.
func runStream(parent context.Context, s *stream.AssistantStream, model types.Model, c types.Context, opts *Options) {
	output := newOutputMessage(model)
	defer func() {
		if recovered := recover(); recovered != nil {
			pushError(s, output, fmt.Errorf("%v", recovered), false)
		}
	}()

	streamCtx, cancel := context.WithCancel(parent)
	defer cancel()
	activity := make(chan struct{}, 1)
	done := make(chan struct{})
	var idleFired atomic.Bool
	go watchIdle(done, activity, cancel, &idleFired)
	defer close(done)

	if opts == nil {
		opts = &Options{}
	}
	if strings.TrimSpace(opts.APIKey) == "" {
		if filled := base.WithEnvAPIKey(&opts.StreamOptions, model.Provider); filled != &opts.StreamOptions {
			opts.APIKey = filled.APIKey
		}
	}

	if err := assertRequestAuth(model.Provider, opts.APIKey, opts.Headers); err != nil {
		pushError(s, output, err, parent.Err() != nil)
		return
	}

	payload := any(buildParams(model, c, opts))
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

	reqCtx := streamCtx
	if opts.TimeoutMs > 0 {
		var timeoutCancel context.CancelFunc
		reqCtx, timeoutCancel = context.WithTimeout(streamCtx, time.Duration(opts.TimeoutMs)*time.Millisecond)
		defer timeoutCancel()
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, chatCompletionsURL(model.BaseURL), bytes.NewReader(body))
	if err != nil {
		pushError(s, output, err, parent.Err() != nil)
		return
	}
	headers := map[string]string{
		"accept":        "application/json",
		"content-type":  "application/json",
		"authorization": "Bearer " + opts.APIKey,
	}
	for key, value := range mergeHeaders(headers, model.Headers, opts.Headers) {
		req.Header.Set(key, value)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		pushError(s, output, streamError(parent.Err(), err, idleFired.Load()), parent.Err() != nil)
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
		pushError(s, output, fmt.Errorf("OpenAI API error: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(responseBody))), parent.Err() != nil)
		return
	}

	s.Push(types.StreamEvent{Type: types.EvStart, Partial: output})
	state := &openAIFoldState{
		Output:          output,
		ToolCallByIndex: map[int]*openAIFoldBlock{},
		ToolCallByID:    map[string]*openAIFoldBlock{},
	}

	err = iterateOpenAIEvents(reqCtx, resp.Body, func() {
		select {
		case activity <- struct{}{}:
		default:
		}
	}, func(chunk *openAIStreamChunk) error {
		events, err := foldOpenAIChunk(chunk, state, model)
		if err != nil {
			return err
		}
		for _, ev := range events {
			s.Push(ev)
		}
		return nil
	})
	if err != nil {
		pushError(s, output, streamError(parent.Err(), err, idleFired.Load()), parent.Err() != nil)
		return
	}

	// A clean EOF that never delivered a finish_reason is a truncated response
	// (TS throws "Stream ended without finish_reason", openai-completions.ts:453-455).
	// Reporting it as a successful done would hide the truncation.
	if !state.HasFinishReason && parent.Err() == nil && !idleFired.Load() {
		pushError(s, output, StreamTruncated(errors.New("stream ended without finish_reason")), false)
		return
	}

	finalEvents := foldOpenAIFinalizeBlocks(state)
	for _, ev := range finalEvents {
		s.Push(ev)
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

// newOutputMessage initializes fields stream consumers expect before the first
// chunk arrives.
func newOutputMessage(model types.Model) *types.AssistantMessage {
	return &types.AssistantMessage{
		Content:    []types.ContentBlock{},
		API:        model.API,
		Provider:   model.Provider,
		Model:      model.ID,
		Usage:      types.Usage{Cost: types.Cost{}},
		StopReason: types.StopStop,
		Timestamp:  time.Now().UnixMilli(),
	}
}

// pushError finalizes output with an aborted or error stop reason and pushes a
// single error event onto the stream.
func pushError(s *stream.AssistantStream, output *types.AssistantMessage, err error, aborted bool) {
	if aborted {
		output.StopReason = types.StopAborted
	} else {
		output.StopReason = types.StopError
	}
	if err != nil {
		output.ErrorMessage = err.Error()
	}
	s.Push(types.StreamEvent{Type: types.EvError, Reason: output.StopReason, Err: output})
}

// streamError picks the most meaningful error: a parent cancellation wins, an idle
// timeout is wrapped as truncated, otherwise the raw transport error is returned.
func streamError(parentErr error, err error, idle bool) error {
	if parentErr != nil {
		return parentErr
	}
	if idle {
		return StreamTruncated(fmt.Errorf("stream idle for %s (no events); aborted to avoid a wedge: %w", streamIdleTimeout, err))
	}
	return err
}

// watchIdle cancels the request if no activity arrives within streamIdleTimeout,
// setting idleFired so the caller can distinguish an idle abort from a real error.
func watchIdle(done <-chan struct{}, activity <-chan struct{}, cancel context.CancelFunc, idleFired *atomic.Bool) {
	idle := time.NewTimer(streamIdleTimeout)
	defer idle.Stop()
	for {
		select {
		case <-activity:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(streamIdleTimeout)
		case <-idle.C:
			idleFired.Store(true)
			cancel()
			return
		case <-done:
			return
		}
	}
}

func chatCompletionsURL(baseURL string) string {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultOpenAIBaseURL
	}
	return strings.TrimRight(baseURL, "/") + "/chat/completions"
}

// mergeHeaders overlays the given header maps in order, later sources winning.
func mergeHeaders(headerSources ...map[string]string) map[string]string {
	merged := map[string]string{}
	for _, headers := range headerSources {
		for key, value := range headers {
			merged[key] = value
		}
	}
	return merged
}

// assertRequestAuth returns an error unless an API key or an authorization header
// is present.
func assertRequestAuth(provider string, apiKey string, headers map[string]string) error {
	if apiKey != "" {
		return nil
	}
	if hasHeader(headers, "authorization") {
		return nil
	}
	return fmt.Errorf("no API key for provider: %s", provider)
}

// hasHeader reports whether headers contains a nonblank match without relying on
// caller casing.
func hasHeader(headers map[string]string, name string) bool {
	expected := strings.ToLower(name)
	for key, value := range headers {
		if strings.ToLower(key) == expected && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

// Register installs the openai-completions provider (id "openai") into reg with
// the supplied models.
func Register(reg *base.Registry, models []types.Model) {
	reg.Register("openai-completions", base.Provider{
		ID:           "openai",
		Models:       models,
		StreamSimple: StreamSimple,
	})
}
