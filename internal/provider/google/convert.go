package google

import (
	"encoding/json"
	"strconv"
	"strings"

	"go.harness.dev/harness/internal/engine/transform"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/engine/util"
)

// buildParams assembles the Gemini request body from a context and options,
// wiring in system instructions, generation config, tools, and thinking config.
func buildParams(model types.Model, c types.Context, opts *Options) map[string]any {
	params := map[string]any{
		"model":    model.ID,
		"contents": convertMessages(c.Messages, model),
	}

	if c.SystemPrompt != "" {
		params["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": util.SanitizeSurrogates(c.SystemPrompt)}},
		}
	}

	generationConfig := map[string]any{}
	if opts != nil {
		if opts.Temperature != nil {
			generationConfig["temperature"] = *opts.Temperature
		}
		if opts.MaxTokens > 0 {
			generationConfig["maxOutputTokens"] = opts.MaxTokens
		}
	}
	params["generationConfig"] = generationConfig

	if len(c.Tools) > 0 {
		params["tools"] = convertTools(c.Tools)
		if opts != nil && opts.ToolChoice != nil {
			params["toolConfig"] = map[string]any{
				"functionCallingConfig": map[string]any{
					"mode": mapToolChoice(opts.ToolChoice),
				},
			}
		}
	}

	if opts != nil && opts.Thinking != nil && opts.Thinking.Enabled && model.Reasoning {
		tc := map[string]any{"includeThoughts": true}
		if opts.Thinking.Level != "" {
			tc["thinkingLevel"] = opts.Thinking.Level
		} else if opts.Thinking.BudgetTokens > 0 {
			tc["thinkingBudget"] = opts.Thinking.BudgetTokens
		}
		generationConfig["thinkingConfig"] = tc
	} else if model.Reasoning && opts != nil && opts.Thinking != nil && !opts.Thinking.Enabled {
		generationConfig["thinkingConfig"] = map[string]any{"thinkingBudget": 0}
	}

	if opts != nil && len(opts.OutputSchema) > 0 {
		// responseJsonSchema takes the caller's JSON Schema as is;
		// responseSchema would need a lossy translation into Gemini's
		// OpenAPI subset. Gemini 3 keeps function calling available
		// alongside it, so only the final text answer is constrained.
		generationConfig["responseMimeType"] = "application/json"
		generationConfig["responseJsonSchema"] = json.RawMessage(opts.OutputSchema)
	}

	return params
}

// convertMessages transforms harness messages into Gemini "contents", mapping
// user/assistant/tool-result roles and coalescing tool responses into a single
// user turn where the wire format expects it.
func convertMessages(messages []types.Message, model types.Model) []map[string]any {
	normalizeID := func(id string, _ types.Model, _ types.AssistantMessage) string {
		return id
	}
	transformedMessages := transform.TransformMessages(messages, model, normalizeID)

	contents := []map[string]any{}
	var pendingImages []map[string]any
	for i := 0; i < len(transformedMessages); i++ {
		msg := transformedMessages[i]

		if user, ok := asUser(msg); ok {
			if !user.Content.IsBlocks() {
				if strings.TrimSpace(user.Content.Text) > "" {
					contents = append(contents, map[string]any{
						"role":  "user",
						"parts": []map[string]any{{"text": util.SanitizeSurrogates(user.Content.Text)}},
					})
				}
			} else {
				parts := []map[string]any{}
				for _, item := range user.Content.Blocks {
					if item.Type == types.BlockText {
						text := util.SanitizeSurrogates(item.Text)
						if strings.TrimSpace(text) != "" {
							parts = append(parts, map[string]any{"text": text})
						}
					} else if item.Type == types.BlockImage {
						parts = append(parts, map[string]any{
							"inlineData": map[string]any{
								"mimeType": item.MimeType,
								"data":     item.Data,
							},
						})
					}
				}
				if len(parts) > 0 {
					contents = append(contents, map[string]any{"role": "user", "parts": parts})
				}
			}
			continue
		}

		if assistant, ok := asAssistant(msg); ok {
			// TS (google-shared.ts:129) requires BOTH provider and model to match;
			// an `API == API` disjunction is always true within the google client and
			// would replay a *different* Gemini model's thought signatures on a model
			// switch, which Google rejects with a 400.
			isSameProviderAndModel := assistant.Provider == model.Provider && assistant.Model == model.ID
			parts := []map[string]any{}
			for _, block := range assistant.Content {
				switch block.Type {
				case types.BlockText:
					if strings.TrimSpace(block.Text) == "" {
						continue
					}
					p := map[string]any{"text": util.SanitizeSurrogates(block.Text)}
					parts = append(parts, p)
				case types.BlockThinking:
					if strings.TrimSpace(block.Thinking) == "" {
						continue
					}
					if isSameProviderAndModel {
						p := map[string]any{
							"thought": true,
							"text":    util.SanitizeSurrogates(block.Thinking),
						}
						if block.ThinkingSignature != "" {
							p["thoughtSignature"] = block.ThinkingSignature
						}
						parts = append(parts, p)
					} else {
						parts = append(parts, map[string]any{"text": util.SanitizeSurrogates(block.Thinking)})
					}
				case types.BlockToolCall:
					fc := map[string]any{
						"name": block.Name,
						"args": rawJSONToAny(block.Arguments),
					}
					if requiresToolCallID(model.ID) {
						fc["id"] = block.ID
					}
					p := map[string]any{"functionCall": fc}
					if block.ThoughtSignature != "" && isSameProviderAndModel {
						p["thoughtSignature"] = block.ThoughtSignature
					}
					parts = append(parts, p)
				}
			}
			if len(parts) == 0 {
				continue
			}
			contents = append(contents, map[string]any{"role": "model", "parts": parts})
			continue
		}

		if toolResult, ok := asToolResult(msg); ok {
			textParts := []string{}
			var imageParts []map[string]any
			for _, block := range toolResult.Content {
				if block.Type == types.BlockText {
					textParts = append(textParts, block.Text)
				}
				if block.Type == types.BlockImage {
					imageParts = append(imageParts, map[string]any{"inlineData": map[string]any{"mimeType": block.MimeType, "data": block.Data}})
				}
			}
			hasImages := len(imageParts) > 0
			text := strings.Join(textParts, "\n")
			if !hasImages {
				text = util.SanitizeSurrogates(text)
			} else if text == "" {
				text = "(see attached image)"
			}

			// TS (google-shared.ts:193,206) keeps the text regardless of error and
			// wraps it as {error: text} vs {output: text}. Blanking the value on error
			// hides *why* a tool failed, so the model can never recover.
			responseValue := text
			frp := map[string]any{
				"functionResponse": map[string]any{
					"name": toolResult.ToolName,
					"response": map[string]any{
						map[bool]string{true: "error", false: "output"}[toolResult.IsError]: responseValue,
					},
				},
			}
			if requiresToolCallID(model.ID) {
				frp["functionResponse"].(map[string]any)["id"] = toolResult.ToolCallID
			}
			// Gemini 3 takes images inside the function response; older
			// models need them in a user turn after the responses.
			if hasImages && multimodalFunctionResponse(model.ID) {
				frp["functionResponse"].(map[string]any)["parts"] = imageParts
			} else if hasImages {
				pendingImages = append(append(pendingImages, map[string]any{"text": "Tool result image:"}), imageParts...)
			}
			flushPending := func() {
				if len(pendingImages) == 0 {
					return
				}
				if i+1 < len(transformedMessages) {
					if _, next := asToolResult(transformedMessages[i+1]); next {
						return
					}
				}
				contents = append(contents, map[string]any{"role": "user", "parts": pendingImages})
				pendingImages = nil
			}

			lastContent := contents[len(contents)-1]
			if lastContent["role"] == "user" {
				parts := lastContent["parts"].([]map[string]any)
				hasFR := false
				for _, p := range parts {
					if _, ok := p["functionResponse"]; ok {
						hasFR = true
						break
					}
				}
				if hasFR {
					parts = append(parts, frp)
					lastContent["parts"] = parts
					flushPending()
					continue
				}
			}
			contents = append(contents, map[string]any{"role": "user", "parts": []map[string]any{frp}})
			flushPending()
		}
	}
	return contents
}

// multimodalFunctionResponse reports whether a Gemini model accepts image
// parts inside functionResponse: Gemini 3 and later do. Ids without a
// gemini-N version are assumed current.
func multimodalFunctionResponse(modelID string) bool {
	rest, ok := strings.CutPrefix(strings.ToLower(modelID), "gemini-")
	if !ok {
		return true
	}
	end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
	if end == -1 {
		end = len(rest)
	}
	major, err := strconv.Atoi(rest[:end])
	return err != nil || major >= 3
}

// requiresToolCallID reports whether a model needs explicit tool-call IDs on the
// wire (Claude and gpt-oss models routed through Gemini do).
func requiresToolCallID(modelID string) bool {
	lower := strings.ToLower(modelID)
	return strings.HasPrefix(lower, "claude-") || strings.HasPrefix(lower, "gpt-oss-")
}

// convertTools wraps harness tools as Gemini functionDeclarations.
func convertTools(tools []types.Tool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	declarations := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		declarations = append(declarations, map[string]any{
			"name":                 tool.Name,
			"description":          tool.Description,
			"parametersJsonSchema": rawJSONToAny(tool.Parameters),
		})
	}
	return []map[string]any{{"functionDeclarations": declarations}}
}

// mapStopReason maps Gemini FinishReason strings to our StopReason.
func mapStopReason(reason string) types.StopReason {
	switch reason {
	case "STOP":
		return types.StopStop
	case "MAX_TOKENS":
		return types.StopLength
	case "MALFORMED_FUNCTION_CALL", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "SAFETY",
		"IMAGE_SAFETY", "RECITATION", "FINISH_REASON_UNSPECIFIED", "OTHER",
		"LANGUAGE", "UNEXPECTED_TOOL_CALL", "NO_IMAGE", "IMAGE_PROHIBITED_CONTENT",
		"IMAGE_RECITATION", "IMAGE_OTHER":
		return types.StopError
	default:
		return types.StopError
	}
}

// mapToolChoice normalizes a tool-choice value to a Gemini functionCalling mode,
// defaulting to "AUTO".
func mapToolChoice(choice any) string {
	if s, ok := choice.(string); ok {
		switch strings.ToUpper(s) {
		case "AUTO":
			return "AUTO"
		case "NONE":
			return "NONE"
		case "ANY":
			return "ANY"
		default:
			return "AUTO"
		}
	}
	return "AUTO"
}

// rawJSONToAny decodes raw JSON into an any, returning an empty object on empty
// input or a decode failure.
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

// rawJSONOrEmpty returns raw, or "{}" when it is empty or JSON null.
func rawJSONOrEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(`{}`)
	}
	return raw
}

// asUser type-asserts msg to a UserMessage (value or non-nil pointer).
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

// asAssistant type-asserts msg to an AssistantMessage (value or non-nil pointer).
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

// asToolResult type-asserts msg to a ToolResultMessage (value or non-nil pointer).
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
