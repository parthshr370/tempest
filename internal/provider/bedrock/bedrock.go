package bedrock

import (
	"bytes"
	"context"
	"encoding/base64"
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
)

// StreamSimple calls ConverseStream, or Converse when Transport is "http".
func StreamSimple(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
	s := stream.NewAssistantStream(model.API, model.Provider, model.ID)
	var options types.SimpleStreamOptions
	if opts != nil {
		options = *opts
	}
	go run(ctx, s, model, c, &options)
	return s
}

func blocks(content []types.ContentBlock, same bool) []any {
	out := []any{}
	for _, b := range content {
		switch b.Type {
		case types.BlockText:
			out = append(out, map[string]any{"text": b.Text})
		case types.BlockImage:
			format := strings.TrimPrefix(b.MimeType, "image/")
			if format == "jpg" {
				format = "jpeg"
			}
			out = append(out, map[string]any{"image": map[string]any{"format": format, "source": map[string]any{"bytes": b.Data}}})
		case types.BlockToolCall:
			args := b.Arguments
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			out = append(out, map[string]any{"toolUse": map[string]any{"toolUseId": b.ID, "name": b.Name, "input": args}})
		case types.BlockThinking:
			if !same {
				continue
			}
			if b.Redacted {
				out = append(out, map[string]any{"reasoningContent": map[string]any{"redactedContent": b.ThinkingSignature}})
			} else if b.ThinkingSignature != "" {
				out = append(out, map[string]any{"reasoningContent": map[string]any{"reasoningText": map[string]any{"text": b.Thinking, "signature": b.ThinkingSignature}}})
			}
		}
	}
	return out
}
func params(model types.Model, c types.Context, opts *types.SimpleStreamOptions) map[string]any {
	messages := []any{}
	add := func(role string, content []any) {
		if len(content) == 0 {
			return
		}
		if len(messages) > 0 {
			last := messages[len(messages)-1].(map[string]any)
			if last["role"] == role {
				last["content"] = append(last["content"].([]any), content...)
				return
			}
		}
		messages = append(messages, map[string]any{"role": role, "content": content})
	}
	for _, msg := range transform.TransformMessages(c.Messages, model, base.NormalizeToolCallID) {
		if p, ok := msg.(*types.UserMessage); ok {
			msg = *p
		}
		if p, ok := msg.(*types.AssistantMessage); ok {
			msg = *p
		}
		if p, ok := msg.(*types.ToolResultMessage); ok {
			msg = *p
		}
		switch m := msg.(type) {
		case types.UserMessage:
			if m.Content.IsBlocks() {
				add("user", blocks(m.Content.Blocks, false))
			} else {
				add("user", []any{map[string]any{"text": m.Content.Text}})
			}
		case types.AssistantMessage:
			add("assistant", blocks(m.Content, m.API == model.API && m.Provider == model.Provider && m.Model == model.ID))
		case types.ToolResultMessage:
			result := map[string]any{"toolUseId": m.ToolCallID, "content": blocks(m.Content, false)}
			if strings.Contains(model.ID, "anthropic.claude-3") || strings.Contains(model.ID, "anthropic.claude-sonnet-4") || strings.Contains(model.ID, "anthropic.claude-opus-4") || strings.Contains(model.ID, "anthropic.claude-haiku-4") || strings.Contains(model.ID, "amazon.nova") {
				result["status"] = "success"
				if m.IsError {
					result["status"] = "error"
				}
			}
			add("user", []any{map[string]any{"toolResult": result}})
		}
	}
	maxTokens := base.ClampMaxTokens(model, c, opts.MaxTokens)
	inference := map[string]any{}
	if maxTokens > 0 {
		inference["maxTokens"] = maxTokens
	}
	if opts.Temperature != nil {
		inference["temperature"] = *opts.Temperature
	}
	p := map[string]any{"messages": messages, "inferenceConfig": inference}
	if c.SystemPrompt != "" {
		p["system"] = []any{map[string]any{"text": c.SystemPrompt}}
	}
	tools := []any{}
	for _, t := range c.Tools {
		tools = append(tools, map[string]any{"toolSpec": map[string]any{"name": t.Name, "description": t.Description, "inputSchema": map[string]any{"json": t.Parameters}}})
	}
	if len(tools) > 0 {
		p["toolConfig"] = map[string]any{"tools": tools}
	}
	if model.Reasoning && opts.Reasoning != "" && strings.Contains(model.ID, "anthropic") {
		adaptive := model.Compat != nil && model.Compat.ForceAdaptiveThinking != nil && *model.Compat.ForceAdaptiveThinking
		if adaptive {
			p["additionalModelRequestFields"] = map[string]any{"thinking": map[string]any{"type": "adaptive"}, "output_config": map[string]any{"effort": opts.Reasoning}}
		} else {
			budget := map[string]int{"minimal": 1024, "low": 2048, "medium": 8192, "high": 16384, "xhigh": 16384}[opts.Reasoning]
			if custom := opts.ThinkingBudgets[opts.Reasoning]; custom > 0 {
				budget = custom
			}
			budget = min(budget, maxTokens-1)
			if budget >= 1024 {
				p["additionalModelRequestFields"] = map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": budget}}
			}
		}
	}
	if len(opts.OutputSchema) > 0 {
		p["outputConfig"] = map[string]any{"textFormat": map[string]any{"type": "json_schema", "structure": map[string]any{"jsonSchema": map[string]any{"name": "response", "schema": string(opts.OutputSchema)}}}}
	}
	return p
}

type usage struct {
	Input        int `json:"inputTokens"`
	Output       int `json:"outputTokens"`
	Total        int `json:"totalTokens"`
	CacheRead    int `json:"cacheReadInputTokens"`
	CacheWrite   int `json:"cacheWriteInputTokens"`
	CacheDetails []struct {
		Tokens int    `json:"inputTokens"`
		TTL    string `json:"ttl"`
	} `json:"cacheDetails"`
}

func applyUsage(out *types.AssistantMessage, u usage, model types.Model) {
	out.Usage = types.Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, TotalTokens: u.Total}
	for _, detail := range u.CacheDetails {
		if detail.TTL == "1h" {
			tokens := detail.Tokens
			out.Usage.CacheWrite1h = &tokens
		}
	}
	types.CalculateCost(model.Cost, &out.Usage)
}
func stopReason(reason string) types.StopReason {
	switch reason {
	case "tool_use":
		return types.StopToolUse
	case "max_tokens", "model_context_window_exceeded":
		return types.StopLength
	case "end_turn", "stop_sequence":
		return types.StopStop
	default:
		return types.StopError
	}
}

type fold struct {
	out      *types.AssistantMessage
	s        *stream.AssistantStream
	model    types.Model
	indices  map[int]int
	args     map[int]string
	redacted map[int][]byte
	stopped  bool
}

func (f *fold) open(wire int, b types.ContentBlock) int {
	if i, ok := f.indices[wire]; ok {
		return i
	}
	i := len(f.out.Content)
	f.indices[wire] = i
	f.out.Content = append(f.out.Content, b)
	kind := types.EvTextStart
	if b.Type == types.BlockThinking {
		kind = types.EvThinkingStart
	}
	if b.Type == types.BlockToolCall {
		kind = types.EvToolCallStart
	}
	f.s.Push(types.StreamEvent{Type: kind, ContentIndex: i, Partial: f.out, ToolCall: &b})
	return i
}
func (f *fold) event(name string, raw []byte) error {
	var e struct {
		Index int `json:"contentBlockIndex"`
		Start struct {
			ToolUse *struct {
				ID   string `json:"toolUseId"`
				Name string `json:"name"`
			} `json:"toolUse"`
		} `json:"start"`
		Delta struct {
			Text    string `json:"text"`
			ToolUse *struct {
				Input string `json:"input"`
			} `json:"toolUse"`
			Reasoning *struct {
				Text      string `json:"text"`
				Signature string `json:"signature"`
				Redacted  string `json:"redactedContent"`
			} `json:"reasoningContent"`
		} `json:"delta"`
		Stop  string `json:"stopReason"`
		Usage usage  `json:"usage"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return err
	}
	switch name {
	case "contentBlockStart":
		if e.Start.ToolUse != nil {
			f.open(e.Index, types.NewToolCall(e.Start.ToolUse.ID, e.Start.ToolUse.Name, json.RawMessage(`{}`)))
		}
	case "contentBlockDelta":
		if e.Delta.ToolUse != nil {
			i, ok := f.indices[e.Index]
			if !ok {
				return fmt.Errorf("tool delta without start")
			}
			f.args[i] += e.Delta.ToolUse.Input
			f.s.Push(types.StreamEvent{Type: types.EvToolCallDelta, ContentIndex: i, Delta: e.Delta.ToolUse.Input, Partial: f.out})
		} else if e.Delta.Reasoning != nil {
			i := f.open(e.Index, types.NewThinking("", ""))
			b := &f.out.Content[i]
			b.Thinking += e.Delta.Reasoning.Text
			b.ThinkingSignature += e.Delta.Reasoning.Signature
			if e.Delta.Reasoning.Redacted != "" {
				decoded, err := base64.StdEncoding.DecodeString(e.Delta.Reasoning.Redacted)
				if err != nil {
					return fmt.Errorf("invalid redacted reasoning: %w", err)
				}
				if f.redacted == nil {
					f.redacted = map[int][]byte{}
				}
				f.redacted[i] = append(f.redacted[i], decoded...)
				b.Redacted = true
			}
			f.s.Push(types.StreamEvent{Type: types.EvThinkingDelta, ContentIndex: i, Delta: e.Delta.Reasoning.Text, Partial: f.out})
		} else if e.Delta.Text != "" {
			i := f.open(e.Index, types.NewText(""))
			f.out.Content[i].Text += e.Delta.Text
			f.s.Push(types.StreamEvent{Type: types.EvTextDelta, ContentIndex: i, Delta: e.Delta.Text, Partial: f.out})
		}
	case "contentBlockStop":
		i, ok := f.indices[e.Index]
		if !ok {
			return nil
		}
		b := &f.out.Content[i]
		kind := types.EvTextEnd
		text := b.Text
		if b.Type == types.BlockThinking {
			kind = types.EvThinkingEnd
			text = b.Thinking
			if b.Redacted {
				b.ThinkingSignature = base64.StdEncoding.EncodeToString(f.redacted[i])
				delete(f.redacted, i)
			}
		}
		if b.Type == types.BlockToolCall {
			kind = types.EvToolCallEnd
			args := f.args[i]
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return fmt.Errorf("invalid Bedrock tool input")
			}
			b.Arguments = json.RawMessage(args)
		}
		f.s.Push(types.StreamEvent{Type: kind, ContentIndex: i, Content: text, ToolCall: b, Partial: f.out})
	case "messageStop":
		f.stopped = true
		f.out.StopReason = stopReason(e.Stop)
		if f.out.StopReason == types.StopError {
			return fmt.Errorf("Bedrock stopped: %s", e.Stop)
		}
	case "metadata":
		applyUsage(f.out, e.Usage, f.model)
	default:
		if strings.HasSuffix(strings.ToLower(name), "exception") {
			status := 500
			if name == "throttlingException" {
				status = 429
			}
			if name == "validationException" {
				status = 400
			}
			return base.ResponseError(&http.Response{StatusCode: status, Header: http.Header{}}, raw)
		}
	}
	return nil
}

func run(parent context.Context, s *stream.AssistantStream, model types.Model, c types.Context, opts *types.SimpleStreamOptions) {
	out := &types.AssistantMessage{API: model.API, Provider: model.Provider, Model: model.ID, Content: []types.ContentBlock{}, StopReason: types.StopStop, Timestamp: time.Now().UnixMilli(), Usage: types.Usage{Cost: types.Cost{Unknown: model.Cost.Unknown}}}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	watchdog := base.WatchStream(cancel, base.StreamTimeoutsFromEnv())
	defer watchdog.Stop()
	fail := func(err error) {
		err = watchdog.Err(parent.Err(), err)
		out.StopReason = types.StopError
		if parent.Err() != nil {
			out.StopReason = types.StopAborted
		}
		base.ApplyError(out, err)
		s.Push(types.StreamEvent{Type: types.EvError, Reason: out.StopReason, Err: out})
	}
	if opts.TimeoutMs > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, time.Duration(opts.TimeoutMs)*time.Millisecond)
		defer stop()
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	endpoint := strings.TrimRight(model.BaseURL, "/")
	if endpoint == "" {
		if region == "" {
			fail(fmt.Errorf("AWS_REGION is required for Bedrock"))
			return
		}
		suffix := "amazonaws.com"
		if strings.HasPrefix(region, "cn-") {
			suffix = "amazonaws.com.cn"
		}
		endpoint = "https://bedrock-runtime." + region + "." + suffix
	}
	operation := "converse-stream"
	if opts.Transport == "http" {
		operation = "converse"
	}
	endpoint += "/model/" + awsEncode(model.ID, true) + "/" + operation
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
	req.Header.Set("Accept", "application/vnd.amazon.eventstream")
	if operation == "converse" {
		req.Header.Set("Accept", "application/json")
	}
	key := opts.APIKey
	if key == "" && !opts.DisableEnvAuth {
		key = os.Getenv("AWS_BEARER_TOKEN_BEDROCK")
	}
	if key != "" && !opts.NoAuth {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for _, headers := range []map[string]string{model.Headers, opts.Headers} {
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	}
	if !opts.NoAuth && req.Header.Get("Authorization") == "" {
		if opts.DisableEnvAuth {
			fail(fmt.Errorf("no credentials for Bedrock"))
			return
		}
		if region == "" {
			fail(fmt.Errorf("AWS_REGION is required for SigV4"))
			return
		}
		creds, err := loadCredentials()
		if err != nil {
			fail(err)
			return
		}
		sign(req, body, creds, region, "bedrock", time.Now())
	}
	resp, err := http.DefaultClient.Do(req)
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
		fail(base.ResponseError(resp, raw))
		return
	}
	s.Push(types.StreamEvent{Type: types.EvStart, Partial: out})
	f := fold{out: out, s: s, model: model, indices: map[int]int{}, args: map[int]string{}}
	if operation == "converse" {
		var reply struct {
			Output struct {
				Message struct {
					Content []json.RawMessage `json:"content"`
				} `json:"message"`
			} `json:"output"`
			Stop  string `json:"stopReason"`
			Usage usage  `json:"usage"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
			fail(err)
			return
		}
		watchdog.Event()
		for i, raw := range reply.Output.Message.Content {
			var b struct {
				Text string `json:"text"`
				Tool *struct {
					ID    string          `json:"toolUseId"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				} `json:"toolUse"`
				Reasoning *struct {
					Text struct {
						Text      string `json:"text"`
						Signature string `json:"signature"`
					} `json:"reasoningText"`
					Redacted string `json:"redactedContent"`
				} `json:"reasoningContent"`
			}
			if err := json.Unmarshal(raw, &b); err != nil {
				fail(err)
				return
			}
			block := types.NewText(b.Text)
			if b.Tool != nil {
				block = types.NewToolCall(b.Tool.ID, b.Tool.Name, b.Tool.Input)
			}
			if b.Reasoning != nil {
				block = types.NewThinking(b.Reasoning.Text.Text, b.Reasoning.Text.Signature)
				if b.Reasoning.Redacted != "" {
					block.Redacted = true
					block.ThinkingSignature = b.Reasoning.Redacted
				}
			}
			index := f.open(i, block)
			delta, end := types.EvTextDelta, types.EvTextEnd
			text := block.Text
			switch block.Type {
			case types.BlockThinking:
				delta, end, text = types.EvThinkingDelta, types.EvThinkingEnd, block.Thinking
			case types.BlockToolCall:
				delta, end, text = types.EvToolCallDelta, types.EvToolCallEnd, string(block.Arguments)
			}
			s.Push(types.StreamEvent{Type: delta, ContentIndex: index, Delta: text, Partial: out})
			s.Push(types.StreamEvent{Type: end, ContentIndex: index, Content: text, ToolCall: &block, Partial: out})
			watchdog.Output()
		}
		out.StopReason = stopReason(reply.Stop)
		applyUsage(out, reply.Usage, model)
		f.stopped = true
		if out.StopReason == types.StopError {
			fail(fmt.Errorf("Bedrock stopped: %s", reply.Stop))
			return
		}
	} else {
		for {
			headers, raw, err := readFrame(resp.Body)
			if err == io.EOF {
				break
			}
			if err != nil {
				fail(&retry.Error{Code: retry.CodeStreamInterrupted, Message: "Bedrock event stream: " + err.Error(), Retryable: true})
				return
			}
			watchdog.Event()
			name := headers[":event-type"]
			if name == "" {
				name = headers[":exception-type"]
			}
			if headers[":message-type"] == "error" {
				fail(fmt.Errorf("Bedrock stream error %s: %s", headers[":error-code"], headers[":error-message"]))
				return
			}
			if err := f.event(name, raw); err != nil {
				fail(err)
				return
			}
			if len(out.Content) > 0 {
				watchdog.Output()
			}
		}
	}
	if parent.Err() != nil {
		fail(parent.Err())
		return
	}
	if !f.stopped {
		fail(&retry.Error{Code: retry.CodeStreamInterrupted, Message: "Bedrock stream ended without messageStop", Retryable: true})
		return
	}
	if out.StopReason == types.StopError {
		fail(fmt.Errorf("Bedrock response stopped unexpectedly"))
		return
	}
	s.Push(types.StreamEvent{Type: types.EvDone, Reason: out.StopReason, Message: out})
}
