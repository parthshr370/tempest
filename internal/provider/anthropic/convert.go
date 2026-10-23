package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"go.harness.dev/harness/internal/document"
	"go.harness.dev/harness/internal/engine/transform"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/engine/util"
)

const (
	cacheRetentionNone  = "none"
	cacheRetentionShort = "short"
	cacheRetentionLong  = "long"

	claudeCodeVersion             = "2.1.75"
	fineGrainedToolStreamingBeta  = "fine-grained-tool-streaming-2025-05-14"
	interleavedThinkingBeta       = "interleaved-thinking-2025-05-14"
	defaultAnthropicBaseURL       = "https://api.anthropic.com"
	defaultAnthropicVersionHeader = "2023-06-01"

	maxAnthropicAttachmentPages = 100
	maxAnthropicPayloadBytes    = int64(32 * 1024 * 1024)
	// maxAnthropicRequestBytes bounds the ENTIRE marshaled request body (system
	// prompt, tools, transcript, and attachments plus JSON overhead), not just
	// the attachment blocks. Anthropic rejects requests over 32 MiB; guarding a
	// little under that makes an over-budget turn fail locally with a typed
	// CodeRequestSizeExceeded instead of a provider 413. attachmentPayloadBudget
	// is the earlier per-block guard; this is the whole-body backstop.
	maxAnthropicRequestBytes = maxAnthropicPayloadBytes - int64(512*1024)
)

var claudeCodeTools = map[string]string{
	"read":            "Read",
	"write":           "Write",
	"edit":            "Edit",
	"bash":            "Bash",
	"grep":            "Grep",
	"glob":            "Glob",
	"askuserquestion": "AskUserQuestion",
	"enterplanmode":   "EnterPlanMode",
	"exitplanmode":    "ExitPlanMode",
	"killshell":       "KillShell",
	"notebookedit":    "NotebookEdit",
	"skill":           "Skill",
	"task":            "Task",
	"taskoutput":      "TaskOutput",
	"todowrite":       "TodoWrite",
	"webfetch":        "WebFetch",
	"websearch":       "WebSearch",
}

// cacheControl mirrors Anthropic's cache_control object so we can attach one to different request blocks.
type cacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

// cacheControlResult keeps the resolved policy even when "none" means there isn't a wire object to send.
type cacheControlResult struct {
	Retention    string
	CacheControl *cacheControl
}

// anthropicCompat collects route-specific switches so request building doesn't guess at each feature.
type anthropicCompat struct {
	SupportsEagerToolInputStreaming bool
	SupportsLongCacheRetention      bool
	SendSessionAffinityHeaders      bool
	SupportsCacheControlOnTools     bool
	SupportsTemperature             bool
	AllowEmptySignature             bool
}

// resolveCacheRetention picks the cache-retention level. Priority: explicit
// option → HARNESS_CACHE_RETENTION env → default "short". "none" disables cache
// control entirely.
func resolveCacheRetention(cacheRetention string, env map[string]string) string {
	if cacheRetention != "" {
		return cacheRetention
	}
	if getProviderEnvValue("HARNESS_CACHE_RETENTION", env) == cacheRetentionLong {
		return cacheRetentionLong
	}
	return cacheRetentionShort
}

// getCacheControl builds the Anthropic cache_control block for the model's
// compatibility level and the resolved retention policy.
func getCacheControl(model types.Model, cacheRetention string, env map[string]string) cacheControlResult {
	retention := resolveCacheRetention(cacheRetention, env)
	if retention == cacheRetentionNone {
		return cacheControlResult{Retention: retention}
	}
	cc := &cacheControl{Type: "ephemeral"}
	if retention == cacheRetentionLong && getAnthropicCompat(model).SupportsLongCacheRetention {
		cc.TTL = "1h"
	}
	return cacheControlResult{Retention: retention, CacheControl: cc}
}

// getProviderEnvValue looks up name in the provided env map first, then falls
// back to [os.Getenv].
func getProviderEnvValue(name string, env map[string]string) string {
	if env != nil {
		if value := env[name]; value != "" {
			return value
		}
	}
	return os.Getenv(name)
}

// getAnthropicCompat builds a resolved anthropicCompat from model.Compat with
// defaults for nil fields.
func getAnthropicCompat(model types.Model) anthropicCompat {
	compat := anthropicCompat{
		SupportsEagerToolInputStreaming: true,
		SupportsLongCacheRetention:      true,
		SupportsCacheControlOnTools:     true,
		SupportsTemperature:             true,
	}
	if model.Compat == nil {
		return compat
	}
	if model.Compat.SupportsEagerToolInputStreaming != nil {
		compat.SupportsEagerToolInputStreaming = *model.Compat.SupportsEagerToolInputStreaming
	}
	if model.Compat.SupportsLongCacheRetention != nil {
		compat.SupportsLongCacheRetention = *model.Compat.SupportsLongCacheRetention
	}
	if model.Compat.SendSessionAffinityHeaders != nil {
		compat.SendSessionAffinityHeaders = *model.Compat.SendSessionAffinityHeaders
	}
	if model.Compat.SupportsCacheControlOnTools != nil {
		compat.SupportsCacheControlOnTools = *model.Compat.SupportsCacheControlOnTools
	}
	if model.Compat.SupportsTemperature != nil {
		compat.SupportsTemperature = *model.Compat.SupportsTemperature
	}
	if model.Compat.AllowEmptySignature != nil {
		compat.AllowEmptySignature = *model.Compat.AllowEmptySignature
	}
	return compat
}

// toClaudeCodeName maps our lowercase tool names to Claude-Code-cased names
// (for OAuth requests where Anthropic expects the Claude Code name set).
func toClaudeCodeName(name string) string {
	if matched, ok := claudeCodeTools[strings.ToLower(name)]; ok {
		return matched
	}
	return name
}

// fromClaudeCodeName maps backwards: given a Claude-Code-cased name, find the
// matching tool in our registry by case-insensitive name match (for OAuth
// responses where Anthropic delivers Claude-Code names).
func fromClaudeCodeName(name string, tools []types.Tool) string {
	if len(tools) > 0 {
		lowerName := strings.ToLower(name)
		for _, tool := range tools {
			if strings.ToLower(tool.Name) == lowerName {
				return tool.Name
			}
		}
	}
	return name
}

// mergeHeaders merges multiple header maps. Later sources override earlier
// ones for duplicate keys (matching JS Object.assign/spread).
func mergeHeaders(headerSources ...map[string]string) map[string]string {
	merged := map[string]string{}
	for _, headers := range headerSources {
		for key, value := range headers {
			merged[key] = value
		}
	}
	return merged
}

// hasHeader checks for a header by case-insensitive name, skipping
// whitespace-only values.
func hasHeader(headers map[string]string, name string) bool {
	if len(headers) == 0 {
		return false
	}
	expected := strings.ToLower(name)
	for key, value := range headers {
		if strings.ToLower(key) == expected && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

// assertRequestAuth rejects the request if no auth material is present
// (neither an API key, OAuth token, nor a pre-existing authorization header).
func assertRequestAuth(provider string, apiKey string, headers map[string]string) error {
	if apiKey != "" {
		return nil
	}
	if hasHeader(headers, "authorization") || hasHeader(headers, "x-api-key") || hasHeader(headers, "cf-aig-authorization") {
		return nil
	}
	return fmt.Errorf("No API key for provider: %s", provider)
}

// isOAuthToken reports whether apiKey is an OAuth token (the "sk-ant-oat"
// prefix distinguishes OAuth from API-key auth).
func isOAuthToken(apiKey string) bool { return strings.Contains(apiKey, "sk-ant-oat") }

// shouldUseFineGrainedToolStreamingBeta reports whether the request needs the
// fine-grained-tool-streaming beta — true when the model has tools but the
// compatibility layer says it can't eagerly stream tool input.
func shouldUseFineGrainedToolStreamingBeta(model types.Model, c types.Context) bool {
	return len(c.Tools) > 0 && !getAnthropicCompat(model).SupportsEagerToolInputStreaming
}

// buildRequestHeaders assembles the HTTP headers for an Anthropic request. It
// returns the header map and whether the auth path is OAuth (which affects
// tool-name casing and the system prompt shape).
func buildRequestHeaders(model types.Model, apiKey string, interleavedThinking bool, useFineGrainedToolStreamingBeta bool, optionsHeaders map[string]string, sessionID string) (map[string]string, bool) {
	needsInterleavedBeta := interleavedThinking
	if model.Compat != nil && model.Compat.ForceAdaptiveThinking != nil && *model.Compat.ForceAdaptiveThinking {
		needsInterleavedBeta = false
	}
	betaFeatures := []string{}
	if useFineGrainedToolStreamingBeta {
		betaFeatures = append(betaFeatures, fineGrainedToolStreamingBeta)
	}
	if needsInterleavedBeta {
		betaFeatures = append(betaFeatures, interleavedThinkingBeta)
	}

	base := map[string]string{
		"accept":            "application/json",
		"content-type":      "application/json",
		"anthropic-version": defaultAnthropicVersionHeader,
		"anthropic-dangerous-direct-browser-access": "true",
	}

	if model.Provider == "github-copilot" {
		if apiKey != "" {
			base["authorization"] = "Bearer " + apiKey
		}
		if len(betaFeatures) > 0 {
			base["anthropic-beta"] = strings.Join(betaFeatures, ",")
		}
		return mergeHeaders(base, model.Headers, optionsHeaders), false
	}

	if apiKey != "" && isOAuthToken(apiKey) {
		base["authorization"] = "Bearer " + apiKey
		base["anthropic-beta"] = strings.Join(append([]string{"claude-code-20250219", "oauth-2025-04-20"}, betaFeatures...), ",")
		base["user-agent"] = "claude-cli/" + claudeCodeVersion
		base["x-app"] = "cli"
		return mergeHeaders(base, model.Headers, optionsHeaders), true
	}

	if apiKey != "" {
		base["x-api-key"] = apiKey
	}
	if len(betaFeatures) > 0 {
		base["anthropic-beta"] = strings.Join(betaFeatures, ",")
	}
	sessionHeaders := map[string]string{}
	if sessionID != "" && getAnthropicCompat(model).SendSessionAffinityHeaders {
		sessionHeaders["x-session-affinity"] = sessionID
	}
	return mergeHeaders(base, sessionHeaders, model.Headers, optionsHeaders), false
}

// buildParams assembles the JSON body for POST /v1/messages. It layers:
// model, messages (converted), max_tokens, stream:true, system prompt
// (with OAuth identity block when using an OAuth key), temperature
// (omitted when thinking is enabled or unsupported), tools, thinking
// config, metadata, and tool_choice.
func buildParams(ctx context.Context, model types.Model, c types.Context, isOAuth bool, opts *Options) (map[string]any, error) {
	cacheRetention := ""
	env := map[string]string(nil)
	maxTokens := model.MaxTokens
	if opts != nil {
		cacheRetention = opts.CacheRetention
		env = opts.Env
		if opts.MaxTokens != 0 {
			maxTokens = opts.MaxTokens
		}
	}
	ccResult := getCacheControl(model, cacheRetention, env)
	compat := getAnthropicCompat(model)
	var blobReader types.BlobReader
	if opts != nil {
		blobReader = opts.BlobReader
	}
	messages, err := convertMessages(ctx, c.Messages, model, isOAuth, ccResult.CacheControl, compat.AllowEmptySignature, blobReader)
	if err != nil {
		return nil, err
	}
	params := map[string]any{
		"model":      model.ID,
		"messages":   messages,
		"max_tokens": maxTokens,
		"stream":     true,
	}

	if isOAuth {
		system := []map[string]any{withCacheControl(map[string]any{
			"type": "text",
			"text": "You are Claude Code, Anthropic's official CLI for Claude.",
		}, ccResult.CacheControl)}
		if c.SystemPrompt != "" {
			system = append(system, withCacheControl(map[string]any{
				"type": "text",
				"text": util.SanitizeSurrogates(c.SystemPrompt),
			}, ccResult.CacheControl))
		}
		params["system"] = system
	} else if c.SystemPrompt != "" {
		params["system"] = []map[string]any{withCacheControl(map[string]any{
			"type": "text",
			"text": util.SanitizeSurrogates(c.SystemPrompt),
		}, ccResult.CacheControl)}
	}

	if opts != nil && opts.Temperature != nil && (opts.ThinkingEnabled == nil || !*opts.ThinkingEnabled) && compat.SupportsTemperature {
		params["temperature"] = *opts.Temperature
	}
	if len(c.Tools) > 0 {
		var toolCache *cacheControl
		if compat.SupportsCacheControlOnTools {
			toolCache = ccResult.CacheControl
		}
		params["tools"] = convertTools(c.Tools, isOAuth, compat.SupportsEagerToolInputStreaming, toolCache)
	}

	if model.Reasoning {
		if opts != nil && opts.ThinkingEnabled != nil && *opts.ThinkingEnabled {
			display := string(opts.ThinkingDisplay)
			if display == "" {
				display = string(ThinkingSummarized)
			}
			if model.Compat != nil && model.Compat.ForceAdaptiveThinking != nil && *model.Compat.ForceAdaptiveThinking {
				params["thinking"] = map[string]any{"type": "adaptive", "display": display}
				if opts.Effort != "" {
					params["output_config"] = map[string]any{"effort": string(opts.Effort)}
				}
			} else {
				budget := opts.ThinkingBudgetTokens
				if budget == 0 {
					budget = 1024
				}
				params["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget, "display": display}
			}
		} else if opts != nil && opts.ThinkingEnabled != nil && !*opts.ThinkingEnabled && thinkingOffSupported(model) {
			params["thinking"] = map[string]any{"type": "disabled"}
		}
	}

	if opts != nil && len(opts.OutputSchema) > 0 {
		// output_config.format constrains the final text answer; tool_use
		// turns stay free, so the agent can act before it answers.
		outputConfig, _ := params["output_config"].(map[string]any)
		if outputConfig == nil {
			outputConfig = map[string]any{}
		}
		outputConfig["format"] = map[string]any{"type": "json_schema", "schema": json.RawMessage(opts.OutputSchema)}
		params["output_config"] = outputConfig
	}

	if opts != nil {
		if userID, ok := opts.Metadata["user_id"].(string); ok {
			params["metadata"] = map[string]any{"user_id": userID}
		}
		if opts.ToolChoice != nil {
			params["tool_choice"] = opts.ToolChoice
		}
	}
	return params, nil
}

// thinkingOffSupported reports whether the model hasn't explicitly disallowed disabling thinking.
func thinkingOffSupported(model types.Model) bool {
	if model.ThinkingLevelMap == nil {
		return true
	}
	value, ok := model.ThinkingLevelMap["off"]
	return !ok || value != nil
}

func withCacheControl(block map[string]any, cc *cacheControl) map[string]any {
	if cc != nil {
		block["cache_control"] = cc
	}
	return block
}

// normalizeToolCallID replaces non-alphanumeric chars with underscores and
// truncates at 64 characters. Anthropic IDs must match [a-zA-Z0-9_-]{1,64};
// our internal IDs may include arbitrary characters (e.g. UUIDs with hyphens
// are fine, but brackets/apostrophes from transformed messages are not).
var invalidToolCallIDChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func normalizeToolCallID(id string) string {
	return invalidToolCallIDChars.ReplaceAllString(id, "_")[:minInt(len(invalidToolCallIDChars.ReplaceAllString(id, "_")), 64)]
}

// convertMessages transforms our internal message types into Anthropic Messages
// API format. It runs [transform.TransformMessages] for normalization, then
// walks the result:
//   - user messages: text-only → string content; multi-block content → array
//   - assistant messages: text/thinking/tool_use blocks, with thinking
//     blocks falling back to text when signature is empty (unless AllowEmptySignature)
//   - consecutive tool_result messages are grouped into one user message
//     (Anthropic requires all tool results in a single turn)
//
// The last user block gets cache_control attached for prompt caching.
func convertMessages(ctx context.Context, messages []types.Message, model types.Model, isOAuth bool, cc *cacheControl, allowEmptySignature bool, blobReader types.BlobReader) ([]map[string]any, error) {
	params := []map[string]any{}
	budget := &attachmentPayloadBudget{}
	transformedMessages := transform.TransformMessages(messages, model, func(id string, _ types.Model, _ types.AssistantMessage) string {
		return normalizeToolCallID(id)
	})

	for i := 0; i < len(transformedMessages); i++ {
		msg := transformedMessages[i]
		if user, ok := asUser(msg); ok {
			if !user.Content.IsBlocks() {
				if strings.TrimSpace(user.Content.Text) > "" {
					params = append(params, map[string]any{"role": "user", "content": util.SanitizeSurrogates(user.Content.Text)})
				}
			} else {
				blocks := []map[string]any{}
				for _, item := range user.Content.Blocks {
					switch item.Type {
					case types.BlockText:
						text := util.SanitizeSurrogates(item.Text)
						if strings.TrimSpace(text) != "" {
							blocks = append(blocks, map[string]any{"type": "text", "text": text})
						}
					case types.BlockImage:
						if err := budget.reserveEncoded(int64(len(item.Data)), item); err != nil {
							return nil, err
						}
						blocks = append(blocks, imageBlock(item))
					case types.BlockDocumentRef, types.BlockImageRef:
						block, err := referencedContentBlock(ctx, item, blobReader, budget)
						if err != nil {
							return nil, err
						}
						blocks = append(blocks, block)
					}
				}
				if len(blocks) > 0 {
					params = append(params, map[string]any{"role": "user", "content": blocks})
				}
			}
			continue
		}

		if assistant, ok := asAssistant(msg); ok {
			blocks := []map[string]any{}
			for _, block := range assistant.Content {
				switch block.Type {
				case types.BlockText:
					if strings.TrimSpace(block.Text) != "" {
						blocks = append(blocks, map[string]any{"type": "text", "text": util.SanitizeSurrogates(block.Text)})
					}
				case types.BlockThinking:
					if block.Redacted {
						blocks = append(blocks, map[string]any{"type": "redacted_thinking", "data": block.ThinkingSignature})
						continue
					}
					if strings.TrimSpace(block.Thinking) == "" {
						continue
					}
					if strings.TrimSpace(block.ThinkingSignature) == "" {
						if allowEmptySignature {
							blocks = append(blocks, map[string]any{"type": "thinking", "thinking": util.SanitizeSurrogates(block.Thinking), "signature": ""})
						} else {
							blocks = append(blocks, map[string]any{"type": "text", "text": util.SanitizeSurrogates(block.Thinking)})
						}
					} else {
						blocks = append(blocks, map[string]any{"type": "thinking", "thinking": util.SanitizeSurrogates(block.Thinking), "signature": block.ThinkingSignature})
					}
				case types.BlockToolCall:
					name := block.Name
					if isOAuth {
						name = toClaudeCodeName(name)
					}
					blocks = append(blocks, map[string]any{"type": "tool_use", "id": block.ID, "name": name, "input": rawJSONToAny(block.Arguments)})
				case types.BlockDocumentRef, types.BlockImageRef:
					return nil, unsupportedRefError(block)
				}
			}
			if len(blocks) > 0 {
				params = append(params, map[string]any{"role": "assistant", "content": blocks})
			}
			continue
		}

		if toolResult, ok := asToolResult(msg); ok {
			first, err := toolResultBlock(ctx, toolResult, blobReader, budget)
			if err != nil {
				return nil, err
			}
			toolResults := []map[string]any{first}
			j := i + 1
			for j < len(transformedMessages) {
				nextMsg, ok := asToolResult(transformedMessages[j])
				if !ok {
					break
				}
				next, err := toolResultBlock(ctx, nextMsg, blobReader, budget)
				if err != nil {
					return nil, err
				}
				toolResults = append(toolResults, next)
				j++
			}
			i = j - 1
			params = append(params, map[string]any{"role": "user", "content": toolResults})
		}
	}

	if cc != nil && len(params) > 0 {
		last := params[len(params)-1]
		if last["role"] == "user" {
			switch content := last["content"].(type) {
			case []map[string]any:
				if len(content) > 0 {
					lastBlock := content[len(content)-1]
					if typ, _ := lastBlock["type"].(string); typ == "text" || typ == "image" || typ == "document" || typ == "tool_result" {
						lastBlock["cache_control"] = cc
					}
				}
			case string:
				last["content"] = []map[string]any{{"type": "text", "text": content, "cache_control": cc}}
			}
		}
	}
	return params, nil
}

// convertContentBlocks preserves plain text as Anthropic's compact string form, but switches to blocks when an attachment needs structure.
func convertContentBlocks(ctx context.Context, content []types.ContentBlock, blobReader types.BlobReader, budget *attachmentPayloadBudget) (any, error) {
	hasMultimodal := false
	hasDocument := false
	for _, block := range content {
		switch block.Type {
		case types.BlockImage, types.BlockImageRef:
			hasMultimodal = true
		case types.BlockDocumentRef:
			hasMultimodal = true
			hasDocument = true
		}
	}
	if !hasMultimodal {
		parts := make([]string, 0, len(content))
		for _, block := range content {
			parts = append(parts, block.Text)
		}
		return util.SanitizeSurrogates(strings.Join(parts, "\n")), nil
	}

	blocks := []map[string]any{}
	hasText := false
	for _, block := range content {
		switch block.Type {
		case types.BlockText:
			hasText = true
			blocks = append(blocks, map[string]any{"type": "text", "text": util.SanitizeSurrogates(block.Text)})
		case types.BlockImage:
			if err := budget.reserveEncoded(int64(len(block.Data)), block); err != nil {
				return nil, err
			}
			blocks = append(blocks, imageBlock(block))
		case types.BlockDocumentRef, types.BlockImageRef:
			converted, err := referencedContentBlock(ctx, block, blobReader, budget)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, converted)
		}
	}
	if !hasText {
		placeholder := "(see attached image)"
		if hasDocument {
			placeholder = "(see attached document)"
		}
		blocks = append([]map[string]any{{"type": "text", "text": placeholder}}, blocks...)
	}
	return blocks, nil
}

func imageBlock(block types.ContentBlock) map[string]any {
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": block.MimeType,
			"data":       block.Data,
		},
	}
}

// referencedContentBlock reads a stored attachment only after its type and provider limits are safe to send.
func referencedContentBlock(ctx context.Context, block types.ContentBlock, blobReader types.BlobReader, budget *attachmentPayloadBudget) (map[string]any, error) {
	wireType := ""
	isTextDocument := false
	switch block.Type {
	case types.BlockDocumentRef:
		switch {
		case block.RefMediaType == "application/pdf":
			if block.RefPageCount > maxAnthropicAttachmentPages {
				return nil, document.NewDocumentError(
					document.CodePageLimitExceeded,
					"The document exceeds the 100-page limit.",
					map[string]any{
						"filename":     block.RefFilename,
						"actual_pages": block.RefPageCount,
						"max_pages":    maxAnthropicAttachmentPages,
					},
					nil,
				)
			}
			wireType = "document"
		case anthropicTextDocumentMediaType(block.RefMediaType):
			isTextDocument = true
		default:
			return nil, unsupportedRefError(block)
		}
	case types.BlockImageRef:
		if !anthropicImageMediaType(block.RefMediaType) {
			return nil, unsupportedRefError(block)
		}
		wireType = "image"
	default:
		return nil, unsupportedRefError(block)
	}

	if blobReader == nil {
		return nil, unsupportedRefError(block)
	}
	size, err := blobReader.StatBlob(ctx, block.RefStore, block.RefKey)
	if err != nil {
		return nil, attachmentReadError(block, err)
	}
	if err := budget.reserveRaw(size, block); err != nil {
		return nil, err
	}

	blob, err := blobReader.OpenBlob(ctx, block.RefStore, block.RefKey)
	if err != nil {
		return nil, attachmentReadError(block, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(blob, size+1))
	closeErr := blob.Close()
	if readErr != nil {
		return nil, attachmentReadError(block, readErr)
	}
	if closeErr != nil {
		return nil, attachmentReadError(block, closeErr)
	}
	if int64(len(data)) != size {
		return nil, document.NewDocumentError(
			document.CodeIntegrityMismatch,
			"The attachment size changed while it was being read.",
			safeAttachmentDetails(block),
			nil,
		)
	}

	if isTextDocument {
		if !utf8.Valid(data) {
			return nil, document.NewDocumentError(
				document.CodeCorruptOrEncrypted,
				"The document text is not valid UTF-8.",
				safeAttachmentDetails(block),
				nil,
			)
		}
		return map[string]any{
			"type": "document",
			"source": map[string]any{
				"type":       "text",
				"media_type": "text/plain",
				"data":       string(data),
			},
		}, nil
	}

	return map[string]any{
		"type": wireType,
		"source": map[string]any{
			"type":       "base64",
			"media_type": block.RefMediaType,
			"data":       base64.StdEncoding.EncodeToString(data),
		},
	}, nil
}

func anthropicTextDocumentMediaType(mediaType string) bool {
	switch mediaType {
	case "text/plain", "text/markdown", "text/csv":
		return true
	default:
		return false
	}
}

func anthropicImageMediaType(mediaType string) bool {
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

// attachmentPayloadBudget tracks encoded bytes across a turn because Anthropic's limit covers every attachment together.
type attachmentPayloadBudget struct {
	encodedBytes int64
}

func (b *attachmentPayloadBudget) reserveRaw(size int64, block types.ContentBlock) error {
	if size < 0 || size > maxAnthropicPayloadBytes {
		return requestSizeError(block)
	}
	encodedBytes := int64(base64.StdEncoding.EncodedLen(int(size)))
	return b.reserveEncoded(encodedBytes, block)
}

func (b *attachmentPayloadBudget) reserveEncoded(size int64, block types.ContentBlock) error {
	if size < 0 || size > maxAnthropicPayloadBytes-b.encodedBytes {
		return requestSizeError(block)
	}
	b.encodedBytes += size
	return nil
}

func unsupportedRefError(block types.ContentBlock) *document.DocumentError {
	return document.NewDocumentError(
		document.CodeUnsupportedForRoute,
		"The attachment is not supported for this model route.",
		safeAttachmentDetails(block),
		nil,
	)
}

func attachmentReadError(block types.ContentBlock, cause error) *document.DocumentError {
	return document.NewDocumentError(
		document.CodeDownloadFailed,
		"The attachment could not be read.",
		safeAttachmentDetails(block),
		cause,
	)
}

func requestSizeError(block types.ContentBlock) *document.DocumentError {
	details := safeAttachmentDetails(block)
	details["max_encoded_bytes"] = maxAnthropicPayloadBytes
	return document.NewDocumentError(
		document.CodeRequestSizeExceeded,
		"The attachments exceed the 32 MB request limit.",
		details,
		nil,
	)
}

// wholeRequestSizeError reports that the entire marshaled request body exceeds
// the provider limit. It carries no attachment block because the overflow is
// the whole request, not one attachment.
func wholeRequestSizeError(size int) *document.DocumentError {
	return document.NewDocumentError(
		document.CodeRequestSizeExceeded,
		"The request exceeds the 32 MB limit.",
		map[string]any{"request_bytes": size, "max_request_bytes": maxAnthropicRequestBytes},
		nil,
	)
}

func safeAttachmentDetails(block types.ContentBlock) map[string]any {
	details := map[string]any{}
	if block.RefFilename != "" {
		details["filename"] = block.RefFilename
	}
	if block.RefMediaType != "" {
		details["media_type"] = block.RefMediaType
	}
	return details
}

func toolResultBlock(ctx context.Context, msg types.ToolResultMessage, blobReader types.BlobReader, budget *attachmentPayloadBudget) (map[string]any, error) {
	content, err := convertContentBlocks(ctx, msg.Content, blobReader, budget)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"type":        "tool_result",
		"tool_use_id": msg.ToolCallID,
		"content":     content,
		"is_error":    msg.IsError,
	}, nil
}

// convertTools maps our [types.Tool] list into Anthropic's tools[] array.
// Each tool's Parameters JSON Schema becomes an input_schema; eager_input_streaming
// is set when the compatibility layer supports it; the last tool gets cache_control
// for prompt caching.
func convertTools(tools []types.Tool, isOAuth bool, supportsEagerToolInputStreaming bool, cc *cacheControl) []map[string]any {
	if tools == nil {
		return []map[string]any{}
	}
	result := make([]map[string]any, 0, len(tools))
	for index, tool := range tools {
		name := tool.Name
		if isOAuth {
			name = toClaudeCodeName(name)
		}
		converted := map[string]any{
			"name":        name,
			"description": tool.Description,
			"input_schema": map[string]any{
				"type":       "object",
				"properties": schemaProperties(tool.Parameters),
				"required":   schemaRequired(tool.Parameters),
			},
		}
		if supportsEagerToolInputStreaming {
			converted["eager_input_streaming"] = true
		}
		if cc != nil && index == len(tools)-1 {
			converted["cache_control"] = cc
		}
		result = append(result, converted)
	}
	return result
}

// mapStopReason maps Anthropic's stop_reason string to our [types.StopReason].
// "refusal" is an error (returns non-empty message); other reasons are terminal
// with no error message.
func mapStopReason(reason string, stopDetails *stopDetails) (types.StopReason, string, error) {
	switch reason {
	case "end_turn":
		return types.StopStop, "", nil
	case "max_tokens":
		return types.StopLength, "", nil
	case "tool_use":
		return types.StopToolUse, "", nil
	case "refusal":
		message := "The model refused to complete the request"
		if stopDetails != nil && stopDetails.Explanation != "" {
			message = stopDetails.Explanation
		}
		return types.StopError, message, nil
	case "pause_turn", "stop_sequence":
		return types.StopStop, "", nil
	case "sensitive":
		return types.StopError, "", nil
	default:
		return "", "", fmt.Errorf("Unhandled stop reason: %s", reason)
	}
}

// rawJSONToAny unmarshals raw JSON bytes to an any value, defaulting to empty map.
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

// rawJSONToObject is [rawJSONToAny] cast to map[string]any, defaulting to empty map.
func rawJSONToObject(raw json.RawMessage) map[string]any {
	if value, ok := rawJSONToAny(raw).(map[string]any); ok {
		return value
	}
	return map[string]any{}
}

// schemaProperties extracts the "properties" key from a JSON Schema.
func schemaProperties(raw json.RawMessage) any {
	if properties, ok := rawJSONToObject(raw)["properties"]; ok {
		return properties
	}
	return map[string]any{}
}

// schemaRequired extracts the "required" key from a JSON Schema.
func schemaRequired(raw json.RawMessage) any {
	if required, ok := rawJSONToObject(raw)["required"]; ok {
		return required
	}
	return []string{}
}

// asUser type-asserts an [types.Message] to [types.UserMessage] (value or pointer).
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

// asAssistant type-asserts an [types.Message] to [types.AssistantMessage] (value or pointer).
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

// asToolResult type-asserts an [types.Message] to [types.ToolResultMessage] (value or pointer).
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
