package openai

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

func openRouterModel(id string) types.Model {
	model := baseModel()
	model.ID = id
	model.Provider = "openrouter"
	model.BaseURL = "https://openrouter.ai/api/v1"
	return model
}

func sessionOptions(sessionID string) *Options {
	return &Options{StreamOptions: types.StreamOptions{SessionID: sessionID}}
}

func TestPromptCacheFieldsPerRoute(t *testing.T) {
	longID := strings.Repeat("s", maxPromptCacheKeyLength+1)
	custom := baseModel()
	custom.BaseURL = "http://127.0.0.1:8080/v1"
	direct := baseModel()
	direct.BaseURL = "https://api.openai.com/v1"
	for _, test := range []struct {
		name           string
		model          types.Model
		opts           *Options
		wantKey        string
		wantSession    bool
		wantCacheBlock bool
	}{
		{name: "openrouter", model: openRouterModel("deepseek/deepseek-v4.1-flash"), opts: sessionOptions("sess1"), wantKey: "sess1", wantSession: true},
		{name: "openrouter claude", model: openRouterModel("anthropic/claude-haiku-4.5"), opts: sessionOptions("sess1"), wantKey: "sess1", wantSession: true, wantCacheBlock: true},
		{name: "openrouter claude alias", model: openRouterModel("~anthropic/claude-sonnet-latest"), opts: sessionOptions("sess1"), wantKey: "sess1", wantSession: true, wantCacheBlock: true},
		{name: "openrouter claude without session", model: openRouterModel("anthropic/claude-haiku-4.5"), opts: &Options{}, wantCacheBlock: true},
		{name: "openai default endpoint", model: baseModel(), opts: sessionOptions("sess1"), wantKey: "sess1"},
		{name: "openai explicit endpoint", model: direct, opts: sessionOptions("sess1"), wantKey: "sess1"},
		{name: "openai long session id", model: baseModel(), opts: sessionOptions(longID), wantKey: promptCacheKey(longID)},
		{name: "unknown compatible server", model: custom, opts: sessionOptions("sess1")},
		{name: "no session", model: baseModel(), opts: &Options{}},
		{name: "retention none", model: openRouterModel("anthropic/claude-haiku-4.5"), opts: &Options{StreamOptions: types.StreamOptions{SessionID: "sess1", CacheRetention: cacheRetentionNone}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			params := buildParams(test.model, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}, test.opts)
			key, hasKey := params["prompt_cache_key"]
			if test.wantKey == "" && hasKey || test.wantKey != "" && key != test.wantKey {
				t.Fatalf("prompt_cache_key = %v (present %v), want %q", key, hasKey, test.wantKey)
			}
			if len(test.wantKey) > maxPromptCacheKeyLength {
				t.Fatalf("key %q exceeds %d characters", test.wantKey, maxPromptCacheKeyLength)
			}
			session, hasSession := params["session_id"]
			if hasSession != test.wantSession || hasSession && session != test.wantKey {
				t.Fatalf("session_id = %v (present %v), want present=%v", session, hasSession, test.wantSession)
			}
			if _, hasCacheBlock := params["cache_control"]; hasCacheBlock != test.wantCacheBlock {
				t.Fatalf("cache_control present = %v, want %v", hasCacheBlock, test.wantCacheBlock)
			}
		})
	}
}

// Each turn's request must extend the previous one byte for byte: same tools,
// same cache key, and the previous messages unchanged in front of the new ones.
func TestRequestPrefixIsByteStableAcrossTurns(t *testing.T) {
	model := openRouterModel("deepseek/deepseek-v4.1-flash")
	tools := []types.Tool{
		{Name: "read", Description: "Read a file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer"}},"required":["path"]}`)},
		{Name: "bash", Description: "Run a command", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)},
	}
	messages := []types.Message{types.UserMessage{Content: types.StringContent("fix the bug")}}
	var previous []byte
	for turn := range 4 {
		callID := "call_" + string(rune('a'+turn))
		body, err := json.Marshal(buildParams(model, types.Context{SystemPrompt: "system", Messages: messages, Tools: tools}, sessionOptions("sess1")))
		if err != nil {
			t.Fatal(err)
		}
		if previous != nil {
			assertExtends(t, previous, body)
		}
		previous = body
		messages = append(messages,
			types.AssistantMessage{Content: []types.ContentBlock{types.NewThinking("plan", ""), types.NewToolCall(callID, "read", json.RawMessage(`{"path":"a.go","offset":1}`))}},
			types.ToolResultMessage{ToolCallID: callID, ToolName: "read", Content: []types.ContentBlock{types.NewText("package a")}},
		)
	}
}

// assertExtends fails unless next carries previous's tools and cache key
// verbatim and previous's messages as an exact prefix of its own.
func assertExtends(t *testing.T, previous, next []byte) {
	t.Helper()
	type request struct {
		Messages       []json.RawMessage `json:"messages"`
		Tools          json.RawMessage   `json:"tools"`
		PromptCacheKey string            `json:"prompt_cache_key"`
		SessionID      string            `json:"session_id"`
	}
	var before, after request
	if err := json.Unmarshal(previous, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(next, &after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Tools, after.Tools) || before.PromptCacheKey != after.PromptCacheKey || before.SessionID != after.SessionID || after.PromptCacheKey == "" {
		t.Fatalf("tools or cache key changed between turns:\n%s\n%s", previous, next)
	}
	if len(after.Messages) <= len(before.Messages) {
		t.Fatalf("next request has %d messages, previous %d", len(after.Messages), len(before.Messages))
	}
	for index, message := range before.Messages {
		if !bytes.Equal(message, after.Messages[index]) {
			t.Fatalf("message %d changed between turns:\n%s\n%s", index, message, after.Messages[index])
		}
	}
}

func TestUsageCostPrefersBilledCost(t *testing.T) {
	priced := openRouterModel("deepseek/deepseek-v4.1-flash")
	priced.Cost = types.ModelCost{Input: 0.30, Output: 1.20, CacheRead: 0.006}
	unpriced := openRouterModel("deepseek/deepseek-v4.1-flash")
	unpriced.Cost = types.ModelCost{Unknown: true}
	direct := baseModel()
	tokens := `"prompt_tokens":5000,"completion_tokens":100,"prompt_tokens_details":{"cached_tokens":4608}`
	for _, test := range []struct {
		name        string
		model       types.Model
		usage       string
		wantTotal   float64
		wantUnknown bool
	}{
		{name: "billed beats catalog", model: priced, usage: `{` + tokens + `,"cost":0.0042}`, wantTotal: 0.0042},
		{name: "billed prices unknown model", model: unpriced, usage: `{` + tokens + `,"cost":0.0042}`, wantTotal: 0.0042},
		{name: "byok adds upstream charge", model: unpriced, usage: `{` + tokens + `,"cost":0.0001,"is_byok":true,"cost_details":{"upstream_inference_cost":0.0002}}`, wantTotal: 0.0003},
		{name: "byok without upstream falls back to catalog", model: priced, usage: `{` + tokens + `,"cost":0,"is_byok":true}`, wantTotal: (392*0.30 + 4608*0.006 + 100*1.20) / 1e6},
		{name: "byok without upstream stays unknown", model: unpriced, usage: `{` + tokens + `,"cost":0,"is_byok":true}`, wantUnknown: true},
		{name: "no billed cost falls back to catalog", model: priced, usage: `{` + tokens + `}`, wantTotal: (392*0.30 + 4608*0.006 + 100*1.20) / 1e6},
		{name: "no billed cost stays unknown", model: unpriced, usage: `{` + tokens + `}`, wantUnknown: true},
		{name: "negative cost is not a charge", model: unpriced, usage: `{` + tokens + `,"cost":-1}`, wantUnknown: true},
		{name: "other routes ignore cost field", model: direct, usage: `{` + tokens + `,"cost":99}`, wantTotal: (392*2.5 + 4608*1.25 + 100*10) / 1e6},
	} {
		t.Run(test.name, func(t *testing.T) {
			var raw map[string]any
			if err := json.Unmarshal([]byte(test.usage), &raw); err != nil {
				t.Fatal(err)
			}
			usage := parseChunkUsage(raw, test.model)
			if usage.Input != 392 || usage.CacheRead != 4608 || usage.Output != 100 {
				t.Fatalf("tokens = %+v", usage)
			}
			if usage.Cost.Unknown != test.wantUnknown {
				t.Fatalf("cost unknown = %v, want %v (%+v)", usage.Cost.Unknown, test.wantUnknown, usage.Cost)
			}
			if diff := usage.Cost.Total - test.wantTotal; diff > 1e-12 || diff < -1e-12 {
				t.Fatalf("cost total = %v, want %v", usage.Cost.Total, test.wantTotal)
			}
		})
	}
}
