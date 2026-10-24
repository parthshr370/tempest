// Package responses implements the OpenAI Responses streaming protocol.
package responses

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/transform"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/engine/util"
	"go.harness.dev/harness/internal/provider/base"
	"go.harness.dev/harness/internal/retry"
	"go.harness.dev/harness/internal/transport/sseparse"
)

// StreamSimple sends a stateless Responses request, retaining encrypted reasoning items in history.
func StreamSimple(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
	s := stream.NewAssistantStream(model.API, model.Provider, model.ID)
	var options types.SimpleStreamOptions
	if opts != nil {
		options = *opts
	}
	go run(ctx, s, model, c, &options)
	return s
}

func params(model types.Model, c types.Context, opts *types.SimpleStreamOptions) map[string]any {
	input := []any{}
	for _, message := range transform.TransformMessages(c.Messages, model, base.NormalizeToolCallID) {
		switch m := message.(type) {
		case *types.UserMessage:
			input = append(input, userItem(*m))
		case types.UserMessage:
			input = append(input, userItem(m))
		case *types.AssistantMessage:
			input = append(input, assistantItems(*m, model)...)
		case types.AssistantMessage:
			input = append(input, assistantItems(m, model)...)
		case *types.ToolResultMessage:
			input = append(input, resultItem(*m))
		case types.ToolResultMessage:
			input = append(input, resultItem(m))
		}
	}
	p := map[string]any{"model": model.ID, "input": input, "stream": true, "store": false}
	if supportsEncryptedReasoning(model) {
		p["include"] = []string{"reasoning.encrypted_content"}
	}
	if c.SystemPrompt != "" {
		p["instructions"] = c.SystemPrompt
	}
	if model.Provider != "openai-chatgpt" {
		maxTokens := base.ClampMaxTokens(model, c, opts.MaxTokens)
		if maxTokens > 0 {
			p["max_output_tokens"] = maxTokens
		}
		if opts.Temperature != nil {
			p["temperature"] = *opts.Temperature
		}
	}
	if opts.SessionID != "" && opts.CacheRetention != "none" && (model.OpenAICompat == nil || model.OpenAICompat.SupportsPromptCacheKey == nil || *model.OpenAICompat.SupportsPromptCacheKey) {
		p["prompt_cache_key"] = base.PromptCacheKey(opts.SessionID)
	}
	if model.Reasoning && opts.Reasoning != "" {
		p["reasoning"] = map[string]any{"effort": opts.Reasoning, "summary": "auto"}
	}
	tools := []any{}
	for _, tool := range c.Tools {
		tools = append(tools, map[string]any{"type": "function", "name": tool.Name, "description": tool.Description, "parameters": tool.Parameters, "strict": false})
	}
	if len(tools) > 0 {
		if model.Provider == "openai-chatgpt" {
			p["tools"] = []any{map[string]any{"type": "namespace", "name": "tempest", "description": "Tempest local tools", "tools": tools}}
		} else {
			p["tools"] = tools
		}
	}
	if len(opts.OutputSchema) > 0 {
		p["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "name": "response", "schema": opts.OutputSchema, "strict": true}}
	}
	return p
}

func content(blocks []types.ContentBlock) []any {
	out := []any{}
	for _, b := range blocks {
		switch b.Type {
		case types.BlockText:
			out = append(out, map[string]any{"type": "input_text", "text": b.Text})
		case types.BlockImage:
			out = append(out, map[string]any{"type": "input_image", "image_url": "data:" + b.MimeType + ";base64," + b.Data, "detail": "auto"})
		}
	}
	return out
}
func userItem(m types.UserMessage) any {
	if !m.Content.IsBlocks() {
		return map[string]any{"role": "user", "content": m.Content.Text}
	}
	return map[string]any{"role": "user", "content": content(m.Content.Blocks)}
}
func resultItem(m types.ToolResultMessage) any {
	return map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": content(m.Content)}
}
func assistantItems(m types.AssistantMessage, model types.Model) []any {
	out := []any{}
	for _, b := range m.Content {
		switch b.Type {
		case types.BlockText:
			out = append(out, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": b.Text, "annotations": []any{}}}})
		case types.BlockToolCall:
			args := string(b.Arguments)
			if args == "" {
				args = "{}"
			}
			call := map[string]any{"type": "function_call", "call_id": b.ID, "name": b.Name, "arguments": args}
			if model.Provider == "openai-chatgpt" {
				call["namespace"] = "tempest"
			}
			out = append(out, call)
		case types.BlockThinking:
			if supportsEncryptedReasoning(model) && m.API == model.API && m.Provider == model.Provider && m.Model == model.ID && b.ThinkingSignature != "" {
				var item map[string]any
				if json.Unmarshal([]byte(b.ThinkingSignature), &item) == nil && item["type"] == "reasoning" {
					if encrypted, ok := item["encrypted_content"].(string); ok && encrypted != "" {
						out = append(out, item)
					}
				}
			}
		}
	}
	return out
}

type item struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Namespace string `json:"namespace"`
	Summary   []struct {
		Text string `json:"text"`
	} `json:"summary"`
}
type event struct {
	Type         string          `json:"type"`
	OutputIndex  int             `json:"output_index"`
	ContentIndex int             `json:"content_index"`
	SummaryIndex int             `json:"summary_index"`
	Delta        string          `json:"delta"`
	Item         json.RawMessage `json:"item"`
	Error        json.RawMessage `json:"error"`
	Code         string          `json:"code"`
	Message      string          `json:"message"`
	Response     struct {
		ID         string          `json:"id"`
		Model      string          `json:"model"`
		Status     string          `json:"status"`
		Error      json.RawMessage `json:"error"`
		Incomplete struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Usage struct {
			Input        int `json:"input_tokens"`
			Output       int `json:"output_tokens"`
			Total        int `json:"total_tokens"`
			InputDetails struct {
				Cached int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputDetails struct {
				Reasoning *int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	} `json:"response"`
}

func run(parent context.Context, s *stream.AssistantStream, model types.Model, c types.Context, opts *types.SimpleStreamOptions) {
	output := &types.AssistantMessage{API: model.API, Provider: model.Provider, Model: model.ID, Content: []types.ContentBlock{}, StopReason: types.StopStop, Timestamp: time.Now().UnixMilli(), Usage: types.Usage{Cost: types.Cost{Unknown: model.Cost.Unknown}}}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	watchdog := base.WatchStream(cancel, base.StreamTimeoutsFromEnv())
	defer watchdog.Stop()
	fail := func(err error) {
		err = watchdog.Err(parent.Err(), err)
		output.StopReason = types.StopError
		if parent.Err() != nil {
			output.StopReason = types.StopAborted
		}
		base.ApplyError(output, err)
		s.Push(types.StreamEvent{Type: types.EvError, Reason: output.StopReason, Err: output})
	}
	if opts.TimeoutMs > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, time.Duration(opts.TimeoutMs)*time.Millisecond)
		defer stop()
	}
	key := opts.APIKey
	if key == "" && !opts.DisableEnvAuth {
		key = os.Getenv("OPENAI_API_KEY")
	}
	endpoint := strings.TrimRight(model.BaseURL, "/")
	if endpoint == "" {
		endpoint = "https://api.openai.com/v1"
	}
	endpoint += "/responses"
	payload := any(params(model, c, opts))
	if opts.OnPayload != nil {
		next, err := opts.OnPayload(payload, model)
		if err != nil {
			fail(err)
			return
		}
		if next != nil {
			payload = next
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		fail(err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.RequestURL(endpoint, opts.Query), bytes.NewReader(body))
	if err != nil {
		fail(err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if !opts.NoAuth && key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for _, headers := range []map[string]string{model.Headers, opts.Headers} {
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	}
	if !opts.NoAuth && req.Header.Get("Authorization") == "" {
		fail(fmt.Errorf("no API key for provider: %s", model.Provider))
		return
	}
	client := *http.DefaultClient
	if model.Provider == "openai-chatgpt" {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	resp, err := client.Do(req)
	if err != nil {
		fail(err)
		return
	}
	defer resp.Body.Close()
	if opts.OnResponse != nil {
		if err := opts.OnResponse(types.ProviderResponse{Status: resp.StatusCode, Headers: util.HeadersToRecord(resp.Header)}, model); err != nil {
			fail(err)
			return
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		failure := base.ResponseError(resp, raw)
		if model.Provider == "openai-chatgpt" && resp.StatusCode == http.StatusUnauthorized {
			failure.Message += "; OpenAI sign-in may be expired or revoked; run tempest login openai"
		}
		fail(failure)
		return
	}
	s.Push(types.StreamEvent{Type: types.EvStart, Partial: output})
	blocks := map[string]int{}
	toolArgs := map[int]string{}
	completed := false
	open := func(key string, b types.ContentBlock) int {
		if index, ok := blocks[key]; ok {
			return index
		}
		index := len(output.Content)
		blocks[key] = index
		output.Content = append(output.Content, b)
		kind := types.EvTextStart
		if b.Type == types.BlockThinking {
			kind = types.EvThinkingStart
		}
		if b.Type == types.BlockToolCall {
			kind = types.EvToolCallStart
		}
		s.Push(types.StreamEvent{Type: kind, ContentIndex: index, Partial: output, ToolCall: &b})
		return index
	}
	err = sseparse.IterateSSE(ctx, resp.Body, func(frame sseparse.ServerSentEvent) error {
		if strings.TrimSpace(frame.Data) == "" || frame.Data == "[DONE]" {
			return nil
		}
		watchdog.Event()
		var e event
		if err := json.Unmarshal([]byte(frame.Data), &e); err != nil {
			return fmt.Errorf("Responses event: %w", err)
		}
		if len(e.Error) > 0 && string(e.Error) != "null" {
			return responseError(e.Error, "", "")
		}
		switch e.Type {
		case "error":
			return responseError(nil, e.Code, e.Message)
		case "response.failed":
			return responseError(e.Response.Error, "", "response failed")
		case "response.created", "response.in_progress":
			output.ResponseID = e.Response.ID
			output.ResponseModel = e.Response.Model
		case "response.output_item.added", "response.output_item.done":
			var it item
			if err := json.Unmarshal(e.Item, &it); err != nil {
				return err
			}
			key := fmt.Sprintf("item:%d", e.OutputIndex)
			switch it.Type {
			case "function_call":
				if model.Provider == "openai-chatgpt" && it.Namespace != "tempest" {
					return fmt.Errorf("ChatGPT returned a function outside the Tempest namespace")
				}
				index := open(key, types.NewToolCall(it.CallID, it.Name, json.RawMessage(`{}`)))
				if e.Type == "response.output_item.done" {
					if !json.Valid([]byte(it.Arguments)) {
						return fmt.Errorf("invalid function arguments for %s", it.Name)
					}
					output.Content[index].Arguments = json.RawMessage(it.Arguments)
					toolArgs[index] = it.Arguments
					output.StopReason = types.StopToolUse
				}
				watchdog.Output()
			case "reasoning":
				index := open(key, types.NewThinking("", ""))
				if e.Type == "response.output_item.done" {
					output.Content[index].ThinkingSignature = string(e.Item)
					if output.Content[index].Thinking == "" {
						for _, v := range it.Summary {
							output.Content[index].Thinking += v.Text
						}
					}
				}
			}
		case "response.output_text.delta", "response.refusal.delta":
			index := open(fmt.Sprintf("text:%d:%d", e.OutputIndex, e.ContentIndex), types.NewText(""))
			output.Content[index].Text += e.Delta
			watchdog.Output()
			s.Push(types.StreamEvent{Type: types.EvTextDelta, ContentIndex: index, Delta: e.Delta, Partial: output})
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			index := open(fmt.Sprintf("item:%d", e.OutputIndex), types.NewThinking("", ""))
			output.Content[index].Thinking += e.Delta
			watchdog.Output()
			s.Push(types.StreamEvent{Type: types.EvThinkingDelta, ContentIndex: index, Delta: e.Delta, Partial: output})
		case "response.function_call_arguments.delta":
			index, ok := blocks[fmt.Sprintf("item:%d", e.OutputIndex)]
			if !ok {
				return fmt.Errorf("function arguments without output item")
			}
			toolArgs[index] += e.Delta
			watchdog.Output()
			s.Push(types.StreamEvent{Type: types.EvToolCallDelta, ContentIndex: index, Delta: e.Delta, Partial: output})
		case "response.completed", "response.incomplete":
			completed = true
			output.ResponseID = e.Response.ID
			output.ResponseModel = e.Response.Model
			u := e.Response.Usage
			output.Usage = types.Usage{Input: max(0, u.Input-u.InputDetails.Cached), CacheRead: u.InputDetails.Cached, Output: u.Output, TotalTokens: u.Total, Reasoning: u.OutputDetails.Reasoning}
			types.CalculateCost(model.Cost, &output.Usage)
			if e.Type == "response.incomplete" {
				if model.Provider == "openai-chatgpt" {
					return fmt.Errorf("ChatGPT response incomplete; no tools were executed")
				}
				if e.Response.Incomplete.Reason != "max_output_tokens" {
					return fmt.Errorf("Responses incomplete: %s", e.Response.Incomplete.Reason)
				}
				output.StopReason = types.StopLength
			}
		}
		return nil
	})
	if err != nil {
		fail(err)
		return
	}
	if parent.Err() != nil {
		fail(parent.Err())
		return
	}
	if !completed {
		fail(&retry.Error{Code: retry.CodeStreamInterrupted, Message: "Responses stream ended without a completed response", Retryable: true})
		return
	}
	for i := range output.Content {
		b := &output.Content[i]
		kind := types.EvTextEnd
		text := b.Text
		if b.Type == types.BlockThinking {
			kind = types.EvThinkingEnd
			text = b.Thinking
		}
		if b.Type == types.BlockToolCall {
			kind = types.EvToolCallEnd
			raw := toolArgs[i]
			if !json.Valid([]byte(raw)) {
				fail(fmt.Errorf("invalid completed function arguments"))
				return
			}
			b.Arguments = json.RawMessage(raw)
			output.StopReason = types.StopToolUse
		}
		s.Push(types.StreamEvent{Type: kind, ContentIndex: i, Content: text, Partial: output, ToolCall: b})
	}
	s.Push(types.StreamEvent{Type: types.EvDone, Reason: output.StopReason, Message: output})
}

func responseError(raw json.RawMessage, code, message string) error {
	errorType := ""
	if len(raw) > 0 && string(raw) != "null" {
		var details struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(raw, &details); err != nil {
			return fmt.Errorf("invalid Responses error: %w", err)
		}
		errorType = details.Type
		if details.Code != "" {
			code = details.Code
		}
		if details.Message != "" {
			message = details.Message
		}
	}
	if code == "rate_limit_exceeded" {
		code = "rate_limit_error"
	}
	now := time.Now()
	if errorType != "" && retry.Classify(retry.Failure{ProviderCode: code}, now).Code == retry.CodeProviderFailure {
		code = errorType
		if code == "rate_limit_exceeded" {
			code = "rate_limit_error"
		}
	}
	return retry.Classify(retry.Failure{ProviderCode: code, Message: message}, now)
}

func supportsEncryptedReasoning(model types.Model) bool {
	return model.Reasoning && (model.OpenAICompat == nil || model.OpenAICompat.SupportsEncryptedReasoning == nil || *model.OpenAICompat.SupportsEncryptedReasoning)
}
