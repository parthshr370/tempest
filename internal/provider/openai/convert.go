package openai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strings"

	"go.harness.dev/harness/internal/engine/transform"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/engine/util"
)

// buildParams assembles the OpenAI chat-completions request body from the model,
// context, and options, always enabling streaming with usage.
func buildParams(model types.Model, c types.Context, opts *Options) map[string]any {
	maxTokens := model.MaxTokens
	if opts != nil && opts.MaxTokens != 0 {
		maxTokens = opts.MaxTokens
	}

	params := map[string]any{
		"model":    model.ID,
		"messages": convertMessages(model, c),
		"stream":   true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
	}

	if maxTokens > 0 {
		params["max_completion_tokens"] = maxTokens
	}

	if opts != nil && opts.Temperature != nil {
		params["temperature"] = *opts.Temperature
	}

	if len(c.Tools) > 0 {
		params["tools"] = convertTools(c.Tools)
	} else if hasToolHistory(c.Messages) {
		params["tools"] = []map[string]any{}
	}

	if opts != nil && opts.ToolChoice != nil {
		params["tool_choice"] = opts.ToolChoice
	}

	if opts != nil && opts.ReasoningEffort != "" {
		if model.Provider == "openrouter" {
			// OpenRouter's canonical form; reasoning_effort is its alias.
			params["reasoning"] = map[string]any{"effort": opts.ReasoningEffort}
		} else {
			params["reasoning_effort"] = opts.ReasoningEffort
		}
	}

	if opts != nil && len(opts.OutputSchema) > 0 {
		// Strict mode makes the endpoint constrain every text answer to the
		// schema; tool calls are unaffected, so the agent can still act
		// before answering.
		params["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   outputSchemaName,
				"schema": json.RawMessage(opts.OutputSchema),
				"strict": true,
			},
		}
	}

	applyPromptCache(params, model, opts)
	return params
}

// maxPromptCacheKeyLength is the longest prompt_cache_key OpenAI accepts.
const maxPromptCacheKeyLength = 64

// applyPromptCache asks the endpoint to keep one session's requests on one
// prompt cache, so each turn reuses the previous turn's prefix.
//
// OpenAI and OpenRouter accept prompt_cache_key: OpenAI routes requests that
// share a key and prefix to the same cache, and OpenRouter forwards the key
// upstream. OpenRouter's own sticky-routing key is session_id; with it set,
// every turn after the first reaches the provider endpoint that already holds
// the cache, instead of only after a cache hit has been observed. Anthropic
// models behind OpenRouter cache only with an explicit cache_control; the
// top-level form puts the breakpoint on the last block and moves it forward as
// the conversation grows. Other OpenAI-compatible servers get none of these
// fields, because their dialect is unknown and strict servers reject unknown
// fields. CacheRetention "none" turns all of it off.
func applyPromptCache(params map[string]any, model types.Model, opts *Options) {
	if opts != nil && opts.CacheRetention == cacheRetentionNone {
		return
	}
	isOpenRouter := model.Provider == "openrouter"
	if !isOpenRouter && !(model.Provider == "openai" && isOpenAIEndpoint(model.BaseURL)) {
		return
	}
	if isOpenRouter && isAnthropicSlug(model.ID) {
		params["cache_control"] = map[string]string{"type": "ephemeral"}
	}
	if opts == nil || opts.SessionID == "" {
		return
	}
	key := promptCacheKey(opts.SessionID)
	params["prompt_cache_key"] = key
	if isOpenRouter {
		params["session_id"] = key
	}
}

// promptCacheKey returns the session id, or its SHA-256 hex digest when the id
// is longer than OpenAI accepts. Either way the key is stable for the session.
func promptCacheKey(sessionID string) string {
	if len(sessionID) <= maxPromptCacheKeyLength {
		return sessionID
	}
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:])
}

// isOpenAIEndpoint reports whether baseURL is OpenAI's own API; an empty base
// URL means the default endpoint.
func isOpenAIEndpoint(baseURL string) bool {
	if strings.TrimSpace(baseURL) == "" {
		return true
	}
	parsed, err := url.Parse(baseURL)
	return err == nil && parsed.Hostname() == "api.openai.com"
}

// isAnthropicSlug reports whether an OpenRouter model id names a Claude model,
// including the "~anthropic/...-latest" aliases.
func isAnthropicSlug(modelID string) bool {
	return strings.HasPrefix(strings.TrimPrefix(modelID, "~"), "anthropic/")
}

// hasToolHistory reports whether the messages contain any tool result or tool call,
// used to keep the tools field present so the model can interpret prior tool turns.
func hasToolHistory(messages []types.Message) bool {
	for _, msg := range messages {
		if _, ok := asToolResult(msg); ok {
			return true
		}
		if assistant, ok := asAssistant(msg); ok {
			for _, block := range assistant.Content {
				if block.Type == types.BlockToolCall {
					return true
				}
			}
		}
	}
	return false
}

// convertMessages transforms the context into OpenAI wire-format messages,
// prepending the system prompt and coalescing consecutive tool results.
func convertMessages(model types.Model, c types.Context) []map[string]any {
	params := []map[string]any{}

	normalizeID := func(id string, _ types.Model, _ types.AssistantMessage) string {
		return id
	}
	transformedMessages := transform.TransformMessages(c.Messages, model, normalizeID)

	if c.SystemPrompt != "" {
		params = append(params, map[string]any{
			"role":    "system",
			"content": util.SanitizeSurrogates(c.SystemPrompt),
		})
	}

	for i := 0; i < len(transformedMessages); i++ {
		msg := transformedMessages[i]

		if user, ok := asUser(msg); ok {
			if !user.Content.IsBlocks() {
				if strings.TrimSpace(user.Content.Text) > "" {
					params = append(params, map[string]any{"role": "user", "content": util.SanitizeSurrogates(user.Content.Text)})
				}
			} else {
				parts := []map[string]any{}
				for _, item := range user.Content.Blocks {
					if item.Type == types.BlockText {
						text := util.SanitizeSurrogates(item.Text)
						if strings.TrimSpace(text) != "" {
							parts = append(parts, map[string]any{"type": "text", "text": text})
						}
					} else if item.Type == types.BlockImage {
						parts = append(parts, imageURLPart(item))
					}
				}
				if len(parts) > 0 {
					params = append(params, map[string]any{"role": "user", "content": parts})
				}
			}
			continue
		}

		if assistant, ok := asAssistant(msg); ok {
			assistantMsg := map[string]any{"role": "assistant"}
			textParts := []string{}
			for _, block := range assistant.Content {
				if block.Type == types.BlockText && strings.TrimSpace(block.Text) != "" {
					textParts = append(textParts, util.SanitizeSurrogates(block.Text))
				}
			}
			assistantText := strings.Join(textParts, "")

			toolCalls := []map[string]any{}
			for _, block := range assistant.Content {
				if block.Type == types.BlockToolCall {
					toolCalls = append(toolCalls, map[string]any{
						"id":   block.ID,
						"type": "function",
						"function": map[string]any{
							"name":      block.Name,
							"arguments": string(rawJSONOrEmpty(block.Arguments)),
						},
					})
				}
			}

			if len(assistantText) > 0 || len(toolCalls) > 0 {
				if len(assistantText) > 0 {
					assistantMsg["content"] = assistantText
				} else {
					assistantMsg["content"] = ""
				}
				if len(toolCalls) > 0 {
					assistantMsg["tool_calls"] = toolCalls
				}
				params = append(params, assistantMsg)
			}
			continue
		}

		if _, ok := asToolResult(msg); ok {
			// Chat-completions tool messages carry text only, so the
			// batch's images follow it as one user message.
			var images []map[string]any
			j := i
			for j < len(transformedMessages) {
				next, ok := asToolResult(transformedMessages[j])
				if !ok {
					break
				}
				params = append(params, toolResultBlock(next))
				for _, block := range next.Content {
					if block.Type == types.BlockImage {
						images = append(images, imageURLPart(block))
					}
				}
				j++
			}
			i = j - 1
			if len(images) > 0 {
				parts := append([]map[string]any{{"type": "text", "text": "Attached image(s) from tool result:"}}, images...)
				params = append(params, map[string]any{"role": "user", "content": parts})
			}
		}
	}
	return params
}

// toolResultBlock renders a tool result message as an OpenAI "tool" role message.
func toolResultBlock(msg types.ToolResultMessage) map[string]any {
	textParts := []string{}
	for _, block := range msg.Content {
		if block.Type == types.BlockText {
			textParts = append(textParts, block.Text)
		}
	}
	text := strings.Join(textParts, "\n")
	return map[string]any{
		"role":         "tool",
		"content":      util.SanitizeSurrogates(text),
		"tool_call_id": msg.ToolCallID,
	}
}

// outputSchemaName names the response_format schema; the wire requires one.
const outputSchemaName = "final_answer"

// imageURLPart renders an image block as an inline data-URL content part.
func imageURLPart(block types.ContentBlock) map[string]any {
	return map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": fmt.Sprintf("data:%s;base64,%s", block.MimeType, block.Data)},
	}
}

// convertTools renders tools as OpenAI function-tool definitions.
func convertTools(tools []types.Tool) []map[string]any {
	if tools == nil {
		return []map[string]any{}
	}
	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		result = append(result, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  rawJSONToAny(tool.Parameters),
			},
		})
	}
	return result
}

// mapOpenAIStopReason maps an OpenAI finish_reason to a StopReason and optional
// error message, returning an error for unrecognized values.
func mapOpenAIStopReason(reason string) (types.StopReason, string, error) {
	if reason == "" {
		return types.StopStop, "", nil
	}
	switch reason {
	case "stop", "end":
		return types.StopStop, "", nil
	case "length":
		return types.StopLength, "", nil
	case "function_call", "tool_calls":
		return types.StopToolUse, "", nil
	case "content_filter":
		return types.StopError, "Provider finish_reason: content_filter", nil
	case "network_error":
		return types.StopError, "Provider finish_reason: network_error", nil
	default:
		return "", "", fmt.Errorf("unhandled stop reason: %s", reason)
	}
}

// parseChunkUsage builds a Usage from a raw usage object, subtracting cached and
// cache-write tokens from the input count. Cost is the provider-billed amount
// when the route reports one, else the price-list estimate (unknown when the
// model has no price).
func parseChunkUsage(raw map[string]any, model types.Model) types.Usage {
	promptTokens := intValue(extractInt(raw, "prompt_tokens"))
	completionTokens := intValue(extractInt(raw, "completion_tokens"))

	cacheRead := 0
	cacheWrite := 0
	if details, ok := raw["prompt_tokens_details"].(map[string]any); ok {
		if v := extractInt(details, "cached_tokens"); v != nil {
			cacheRead = *v
		}
		if v := extractInt(details, "cache_write_tokens"); v != nil {
			cacheWrite = *v
		}
	}

	reasoning := 0
	if details, ok := raw["completion_tokens_details"].(map[string]any); ok {
		if v := extractInt(details, "reasoning_tokens"); v != nil {
			reasoning = *v
		}
	}

	input := maxInt(0, promptTokens-cacheRead-cacheWrite)
	usage := types.Usage{
		Input:       input,
		Output:      completionTokens,
		CacheRead:   cacheRead,
		CacheWrite:  cacheWrite,
		Reasoning:   &reasoning,
		TotalTokens: input + completionTokens + cacheRead + cacheWrite,
		Cost:        types.Cost{},
	}
	types.CalculateCost(model.Cost, &usage)
	if model.ReportsBilledCost() {
		if billed, ok := billedCost(raw); ok {
			usage.Cost = types.Cost{Total: billed}
		}
	}
	return usage
}

// billedCost reads the USD cost OpenRouter billed for one request. usage.cost
// is what OpenRouter charged the account. With a bring-your-own-key upstream
// (is_byok), the inference itself is billed by the upstream to the user's own
// key and reported separately as cost_details.upstream_inference_cost, so the
// request's cost is the sum. A BYOK usage without the upstream amount does not
// say what the request cost, so it reports nothing rather than the fee alone.
func billedCost(raw map[string]any) (float64, bool) {
	charged, ok := nonNegativeNumber(raw["cost"])
	if !ok {
		return 0, false
	}
	if isBYOK, _ := raw["is_byok"].(bool); !isBYOK {
		return charged, true
	}
	details, _ := raw["cost_details"].(map[string]any)
	upstream, ok := nonNegativeNumber(details["upstream_inference_cost"])
	if !ok {
		return 0, false
	}
	return charged + upstream, true
}

// nonNegativeNumber accepts a decoded JSON number that is a valid charge.
func nonNegativeNumber(value any) (float64, bool) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
		return 0, false
	}
	return number, true
}

// extractInt reads key from raw as an int, tolerating float64/int/*int JSON shapes,
// returning nil when absent or not numeric.
func extractInt(raw map[string]any, key string) *int {
	if v, ok := raw[key]; ok {
		switch n := v.(type) {
		case float64:
			i := int(n)
			return &i
		case int:
			return &n
		case *int:
			return n
		}
	}
	return nil
}

// intValue keeps optional usage fields from turning an incomplete provider
// response into an error.
func intValue(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// rawJSONToAny unmarshals raw into an any, falling back to an empty object on
// empty or invalid input.
func rawJSONToAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return map[string]any{}
	}
	return value
}

// rawJSONOrEmpty returns raw, or an empty JSON object for empty or null input.
func rawJSONOrEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(`{}`)
	}
	return raw
}

// asUser normalizes the value and pointer forms that can live in [types.Message]
// and reports whether it found a user message.
func asUser(msg types.Message) (types.UserMessage, bool) {
	switch m := msg.(type) {
	case types.UserMessage:
		return m, true
	case *types.UserMessage:
		if m != nil {
			return *m, true
		}
	}
	return types.UserMessage{}, false
}

// asAssistant normalizes the value and pointer forms that can live in [types.Message]
// and reports whether it found an assistant message.
func asAssistant(msg types.Message) (types.AssistantMessage, bool) {
	switch m := msg.(type) {
	case types.AssistantMessage:
		return m, true
	case *types.AssistantMessage:
		if m != nil {
			return *m, true
		}
	}
	return types.AssistantMessage{}, false
}

// asToolResult normalizes the value and pointer forms that can live in [types.Message]
// and reports whether it found a tool result.
func asToolResult(msg types.Message) (types.ToolResultMessage, bool) {
	switch m := msg.(type) {
	case types.ToolResultMessage:
		return m, true
	case *types.ToolResultMessage:
		if m != nil {
			return *m, true
		}
	}
	return types.ToolResultMessage{}, false
}
