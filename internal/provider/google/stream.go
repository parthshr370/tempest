// Package google implements the streaming client for Google's Gemini
// (generative language) API, translating harness message/context types to and
// from the Gemini wire format.
// Package google implements a native Gemini streaming client. It speaks the
// generateContent SSE format, folds text/functionCall parts, and maps Gemini
// usage/finish-reason fields to [types.Usage]/[types.StopReason]. Tool-call IDs
// are synthesized with a
// monotonic counter when the API omits them (not time-based, so tests are
// deterministic).
package google

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
	"go.harness.dev/harness/internal/retry"
)

// Stream starts a Gemini generation and returns an AssistantStream whose events
// are produced asynchronously; errors are delivered as an error event on the
// stream rather than returned to the caller.
func Stream(ctx context.Context, model types.Model, c types.Context, opts *Options) *stream.AssistantStream {
	s := stream.NewAssistantStream(model.API, model.Provider, model.ID)
	go runStream(ctx, s, model, c, opts)
	return s
}

// StreamSimple adapts SimpleStreamOptions into full Options (deriving token
// limits and enabling thinking when a reasoning level is requested) and streams.
func StreamSimple(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
	apiKey := ""
	if opts != nil {
		apiKey = opts.APIKey
	}
	base := buildBaseOptions(model, c, opts, apiKey)
	if opts == nil || opts.Reasoning == "" {
		return Stream(ctx, model, c, &Options{StreamOptions: base, Thinking: &thinkingConfig{Enabled: false}})
	}
	return Stream(ctx, model, c, &Options{StreamOptions: base, Thinking: &thinkingConfig{Enabled: true, Level: opts.Reasoning}})
}

// runStream drives the full request/response lifecycle on a goroutine: it builds
// the payload, issues the HTTP call, folds SSE chunks into stream events, and
// terminates the stream with either a done or an error event.
func runStream(parent context.Context, s *stream.AssistantStream, model types.Model, c types.Context, opts *Options) {
	output := newOutputMessage(model)
	defer func() {
		if recovered := recover(); recovered != nil {
			pushError(s, output, fmt.Errorf("%v", recovered), false)
		}
	}()

	streamCtx, cancel := context.WithCancel(parent)
	defer cancel()
	watchdog := base.WatchStream(cancel, streamTimeouts)
	defer watchdog.Stop()

	if opts == nil {
		opts = &Options{}
	}
	if strings.TrimSpace(opts.APIKey) == "" && !opts.DisableEnvAuth && !opts.NoAuth {
		if filled := base.WithEnvAPIKey(&opts.StreamOptions, model.Provider); filled != &opts.StreamOptions {
			opts.APIKey = filled.APIKey
		}
	}

	if !opts.NoAuth {
		if err := assertRequestAuth(model.Provider, opts.APIKey, mergeHeaders(model.Headers, opts.Headers)); err != nil {
			pushError(s, output, err, parent.Err() != nil)
			return
		}
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

	url := generateContentURL(model.BaseURL, model.ID)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, base.RequestURL(url, opts.Query), bytes.NewReader(body))
	if err != nil {
		pushError(s, output, err, parent.Err() != nil)
		return
	}
	headers := map[string]string{
		"accept":       "application/json",
		"content-type": "application/json",
	}
	if opts.APIKey != "" && !opts.NoAuth {
		headers["x-goog-api-key"] = opts.APIKey
	}
	for key, value := range mergeHeaders(headers, model.Headers, opts.Headers) {
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
	state := &googleFoldState{Output: output}
	hasFinishReason := false

	foldChunk := func(chunk *googleStreamChunk) error {
		if reason := chunk.PromptFeedback.BlockReason; reason != "" && reason != "BLOCK_REASON_UNSPECIFIED" {
			return &retry.Error{Code: retry.CodeInvalidRequest, Message: "Gemini prompt blocked by content filter: " + reason}
		}
		for _, candidate := range chunk.Candidates {
			if candidate.FinishReason != "" {
				hasFinishReason = true
			}
		}
		events, err := foldGoogleChunk(chunk, state, model)
		if err != nil {
			return err
		}
		if len(events) > 0 {
			watchdog.Output()
		}
		for _, ev := range events {
			s.Push(ev)
		}
		return nil
	}
	if strings.Contains(model.BaseURL, ":generateContent") {
		var chunk googleStreamChunk
		err = json.NewDecoder(resp.Body).Decode(&chunk)
		watchdog.Event()
		if err == nil && len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			err = googleAPIError(chunk.Error)
		}
		if err == nil {
			err = foldChunk(&chunk)
		}
	} else {
		err = iterateGoogleEvents(reqCtx, resp.Body, watchdog.Event, foldChunk)
	}
	if err != nil {
		pushError(s, output, watchdog.Err(parent.Err(), err), parent.Err() != nil)
		return
	}
	if !hasFinishReason && parent.Err() == nil {
		pushError(s, output, &retry.Error{Code: retry.CodeStreamInterrupted, Message: "Gemini stream ended without finishReason", Retryable: true}, false)
		return
	}

	finalEvents := foldGoogleFinalizeBlocks(state)
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

// newOutputMessage seeds the accumulating assistant message with model identity
// and a default STOP reason before any chunks are folded in.
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

// pushError finalizes output with an aborted or error stop reason and emits a
// single error event on the stream.
func pushError(s *stream.AssistantStream, output *types.AssistantMessage, err error, aborted bool) {
	if aborted {
		output.StopReason = types.StopAborted
	} else {
		output.StopReason = types.StopError
	}
	base.ApplyError(output, err)
	s.Push(types.StreamEvent{Type: types.EvError, Reason: output.StopReason, Err: output})
}

// generateContentURL builds the streamGenerateContent SSE endpoint for a model,
// defaulting the base URL when none is configured.
func generateContentURL(baseURL string, modelID string) string {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultGoogleBaseURL
	}
	if strings.Contains(baseURL, ":streamGenerateContent") || strings.Contains(baseURL, ":generateContent") {
		return baseURL
	}
	return strings.TrimRight(baseURL, "/") + "/v1beta/models/" + modelID + ":streamGenerateContent?alt=sse"
}

// mergeHeaders overlays header sources left-to-right, later sources winning.
func mergeHeaders(headerSources ...map[string]string) map[string]string {
	merged := map[string]string{}
	for _, headers := range headerSources {
		for key, value := range headers {
			merged[key] = value
		}
	}
	return merged
}

// assertRequestAuth fails fast when no API key is available for the request.
func assertRequestAuth(_ string, apiKey string, headers map[string]string) error {
	if apiKey != "" {
		return nil
	}
	for key, value := range headers {
		if strings.EqualFold(key, "authorization") && strings.TrimSpace(value) != "" {
			return nil
		}
	}
	return errors.New("no API key for provider: google")
}

// Register installs the google-generative-ai provider (id "google") into the
// registry, wiring up its models and StreamSimple entry point.
func Register(reg *base.Registry, models []types.Model) {
	reg.Register("google-generative-ai", base.Provider{
		ID:           "google",
		Models:       models,
		StreamSimple: StreamSimple,
	})
}
