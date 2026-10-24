package anthropic

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/internal/document"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/base"
	"go.harness.dev/harness/internal/retry"
)

func TestMain(m *testing.M) {
	streamTimeouts = base.StreamTimeouts{FirstOutput: 50 * time.Millisecond, Idle: 50 * time.Millisecond}
	os.Exit(m.Run())
}

func baseModel() types.Model {
	return types.Model{
		ID:            "claude-test",
		Name:          "Claude Test",
		API:           "anthropic-messages",
		Provider:      "anthropic",
		ContextWindow: 200000,
		MaxTokens:     4096,
		Cost:          types.ModelCost{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	}
}

func TestBuildParamsOAuthSystemToolsThinkingAndMetadata(t *testing.T) {
	forceAdaptive := true
	model := baseModel()
	model.Reasoning = true
	model.Compat = &types.AnthropicCompat{ForceAdaptiveThinking: &forceAdaptive}
	temp := 0.7
	opts := &Options{
		StreamOptions: types.StreamOptions{
			MaxTokens:      2048,
			Temperature:    &temp,
			CacheRetention: cacheRetentionLong,
			Metadata:       map[string]any{"user_id": "user-1"},
		},
		ThinkingEnabled: boolPtr(true),
		Effort:          EffortXHigh,
		ToolChoice:      &ToolChoice{Type: "tool", Name: "Read"},
	}
	ctx := types.Context{
		SystemPrompt: "system prompt",
		Messages:     []types.Message{types.UserMessage{Content: types.StringContent("hello")}},
		Tools: []types.Tool{{
			Name:        "read",
			Description: "Read a file",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}},
	}

	params, err := buildParams(context.Background(), model, ctx, true, opts)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}

	if params["model"] != "claude-test" || params["max_tokens"] != 2048 || params["stream"] != true {
		t.Fatalf("basic params = %#v", params)
	}
	if _, ok := params["temperature"]; ok {
		t.Fatalf("temperature should be omitted while thinking is enabled: %#v", params["temperature"])
	}
	system := params["system"].([]map[string]any)
	if len(system) != 2 || system[0]["text"] != "You are Claude Code, Anthropic's official CLI for Claude." || system[1]["text"] != "system prompt" {
		t.Fatalf("system = %#v", system)
	}
	cache := system[0]["cache_control"].(*cacheControl)
	if cache.Type != "ephemeral" || cache.TTL != "1h" {
		t.Fatalf("cache_control = %#v", cache)
	}
	tools := params["tools"].([]map[string]any)
	if len(tools) != 1 || tools[0]["name"] != "Read" || tools[0]["eager_input_streaming"] != true {
		t.Fatalf("tools = %#v", tools)
	}
	schema := tools[0]["input_schema"].(map[string]any)
	if schema["type"] != "object" || schema["properties"] == nil || schema["required"] == nil {
		t.Fatalf("schema = %#v", schema)
	}
	thinking := params["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" || thinking["display"] != "summarized" {
		t.Fatalf("thinking = %#v", thinking)
	}
	if got := params["output_config"].(map[string]any)["effort"]; got != "xhigh" {
		t.Fatalf("effort = %v", got)
	}
	if got := params["metadata"].(map[string]any)["user_id"]; got != "user-1" {
		t.Fatalf("metadata = %#v", params["metadata"])
	}
}

func TestConvertMessagesGroupsToolResultsAndCachesLastUserBlock(t *testing.T) {
	model := baseModel()
	messages := []types.Message{
		types.AssistantMessage{API: "other", Provider: "other", Model: "other", Content: []types.ContentBlock{
			types.NewToolCall("bad|id", "read", json.RawMessage(`{"path":"x"}`)),
		}},
		types.ToolResultMessage{ToolCallID: "bad|id", ToolName: "read", Content: []types.ContentBlock{types.NewText("one")}},
		types.ToolResultMessage{ToolCallID: "second", ToolName: "read", Content: []types.ContentBlock{types.NewText("two")}, IsError: true},
	}

	got, err := convertMessages(context.Background(), messages, model, true, &cacheControl{Type: "ephemeral"}, false, nil)
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("messages len = %d: %#v", len(got), got)
	}
	assistantBlocks := got[0]["content"].([]map[string]any)
	if assistantBlocks[0]["name"] != "Read" || assistantBlocks[0]["id"] != "bad_id" {
		t.Fatalf("assistant blocks = %#v", assistantBlocks)
	}
	userBlocks := got[1]["content"].([]map[string]any)
	if len(userBlocks) != 2 || userBlocks[0]["tool_use_id"] != "bad_id" || userBlocks[1]["tool_use_id"] != "second" || userBlocks[1]["is_error"] != true {
		t.Fatalf("tool result grouping = %#v", userBlocks)
	}
	if _, ok := userBlocks[1]["cache_control"].(*cacheControl); !ok {
		t.Fatalf("last tool result missing cache_control: %#v", userBlocks[1])
	}
}

func TestConvertMessagesEncodesReferencedContent(t *testing.T) {
	tests := []struct {
		name     string
		block    types.ContentBlock
		wireType string
	}{
		{
			name:     "document",
			block:    types.NewDocumentRef("session-local", "document-key", "application/pdf", "report.pdf", 9, 3),
			wireType: "document",
		},
		{
			name:     "image",
			block:    types.NewImageRef("session-local", "image-key", "image/png", "chart.png", 9),
			wireType: "image",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &stubBlobReader{data: []byte("ref-bytes")}
			got, err := convertMessages(
				context.Background(),
				[]types.Message{types.UserMessage{Content: types.BlockContent(test.block)}},
				baseModel(),
				false,
				nil,
				false,
				reader,
			)
			if err != nil {
				t.Fatalf("convertMessages: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("messages = %#v, want one message", got)
			}
			content := got[0]["content"].([]map[string]any)
			if len(content) != 1 || content[0]["type"] != test.wireType {
				t.Fatalf("content = %#v, want one %s block", content, test.wireType)
			}
			source := content[0]["source"].(map[string]any)
			if source["type"] != "base64" || source["media_type"] != test.block.RefMediaType || source["data"] != base64.StdEncoding.EncodeToString(reader.data) {
				t.Fatalf("source = %#v", source)
			}
			if gotCalls := strings.Join(reader.calls, ","); gotCalls != "stat,open" {
				t.Fatalf("BlobReader calls = %q, want stat,open", gotCalls)
			}
		})
	}
}

func TestConvertMessagesReferencedContentFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		block      types.ContentBlock
		reader     types.BlobReader
		wantCode   document.ErrorCode
		wantNoOpen bool
	}{
		{
			name:     "nil reader",
			block:    types.NewDocumentRef("session-local", "document-key", "application/pdf", "report.pdf", 9, 3),
			wantCode: document.CodeUnsupportedForRoute,
		},
		{
			name:       "page limit",
			block:      types.NewDocumentRef("session-local", "document-key", "application/pdf", "report.pdf", 9, 101),
			reader:     &stubBlobReader{data: []byte("ref-bytes")},
			wantCode:   document.CodePageLimitExceeded,
			wantNoOpen: true,
		},
		{
			name:       "request size",
			block:      types.NewDocumentRef("session-local", "document-key", "application/pdf", "report.pdf", maxAnthropicPayloadBytes, 3),
			reader:     &stubBlobReader{statSize: maxAnthropicPayloadBytes},
			wantCode:   document.CodeRequestSizeExceeded,
			wantNoOpen: true,
		},
		{
			name:     "open failure",
			block:    types.NewDocumentRef("session-local", "document-key", "application/pdf", "report.pdf", 9, 3),
			reader:   &stubBlobReader{data: []byte("ref-bytes"), openErr: errors.New("storage unavailable")},
			wantCode: document.CodeDownloadFailed,
		},
		{
			name:       "unsupported media type",
			block:      types.NewImageRef("session-local", "image-key", "image/tiff", "scan.tiff", 9),
			reader:     &stubBlobReader{data: []byte("ref-bytes")},
			wantCode:   document.CodeUnsupportedForRoute,
			wantNoOpen: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := convertMessages(
				context.Background(),
				[]types.Message{types.UserMessage{Content: types.BlockContent(test.block)}},
				baseModel(),
				false,
				nil,
				false,
				test.reader,
			)
			var documentErr *document.DocumentError
			if !errors.As(err, &documentErr) {
				t.Fatalf("error = %v, want *document.DocumentError", err)
			}
			if documentErr.Code != test.wantCode {
				t.Fatalf("DocumentError.Code = %q, want %q", documentErr.Code, test.wantCode)
			}
			if test.wantNoOpen {
				if reader, ok := test.reader.(*stubBlobReader); ok && strings.Contains(strings.Join(reader.calls, ","), "open") {
					t.Fatalf("BlobReader calls = %v, must reject before open", reader.calls)
				}
			}
		})
	}
}

func TestStreamSimpleReferencedContentConversionErrorIsTerminal(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeMinimalAnthropicStream(w)
	}))
	defer server.Close()

	model := baseModel()
	model.BaseURL = server.URL
	block := types.NewDocumentRef("session-local", "document-key", "application/pdf", "report.pdf", 9, 3)
	result := drainAssistantStream(t, StreamSimple(
		context.Background(),
		model,
		types.Context{Messages: []types.Message{types.UserMessage{Content: types.BlockContent(block)}}},
		&types.SimpleStreamOptions{StreamOptions: types.StreamOptions{APIKey: "sk-test"}},
	))

	if result.StopReason != types.StopError || !strings.Contains(result.ErrorMessage, string(document.CodeUnsupportedForRoute)) {
		t.Fatalf("result = %+v, want terminal typed conversion error", result)
	}
	if result.ErrorCode != string(document.CodeUnsupportedForRoute) {
		t.Fatalf("result.ErrorCode = %q, want %q", result.ErrorCode, document.CodeUnsupportedForRoute)
	}
	if result.ErrorDetails["filename"] != "report.pdf" {
		t.Fatalf("result.ErrorDetails = %+v, want filename report.pdf", result.ErrorDetails)
	}
	if requests.Load() != 0 {
		t.Fatalf("HTTP requests = %d, want 0", requests.Load())
	}
}

type stubBlobReader struct {
	data     []byte
	statSize int64
	statErr  error
	openErr  error
	calls    []string
}

func (r *stubBlobReader) StatBlob(context.Context, string, string) (int64, error) {
	r.calls = append(r.calls, "stat")
	if r.statErr != nil {
		return 0, r.statErr
	}
	if r.statSize != 0 {
		return r.statSize, nil
	}
	return int64(len(r.data)), nil
}

func (r *stubBlobReader) OpenBlob(context.Context, string, string) (io.ReadCloser, error) {
	r.calls = append(r.calls, "open")
	if r.openErr != nil {
		return nil, r.openErr
	}
	return io.NopCloser(bytes.NewReader(r.data)), nil
}

func TestIterateAnthropicEventsAndErrors(t *testing.T) {
	var got []string
	activity := 0
	body := strings.NewReader(strings.Join([]string{
		"event: ping\ndata: {}\n",
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":1}}}\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n",
		"",
	}, "\n"))

	err := iterateAnthropicEvents(context.Background(), body, func() { activity++ }, func(ev rawAnthropicEvent) error {
		got = append(got, ev.Type)
		return nil
	})

	if err != nil {
		t.Fatalf("iterateAnthropicEvents: %v", err)
	}
	if strings.Join(got, ",") != "message_start,message_stop" {
		t.Fatalf("events = %v", got)
	}
	if activity != 3 {
		t.Fatalf("activity = %d, want all SSE events", activity)
	}

	err = iterateAnthropicEvents(context.Background(), strings.NewReader("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"), nil, func(rawAnthropicEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "message_stop") {
		t.Fatalf("unterminated err = %v", err)
	}
	err = iterateAnthropicEvents(context.Background(), strings.NewReader("event: error\ndata: boom\n\n"), nil, func(rawAnthropicEvent) error { return nil })
	if err == nil || err.Error() != "boom" {
		t.Fatalf("error event err = %v", err)
	}
}

func TestFoldAnthropicEventsBuildsMessage(t *testing.T) {
	input := 3
	output := 5
	thinking := 2
	msg := newOutputMessage(baseModel())
	state := &foldState{Output: msg}
	events := []rawAnthropicEvent{
		{Type: "message_start", Message: anthropicMessage{ID: "msg_1", Usage: anthropicUsage{InputTokens: &input}}},
		{Type: "content_block_start", Index: 7, ContentBlock: anthropicContentBlock{Type: "text"}},
		{Type: "content_block_delta", Index: 7, Delta: anthropicDelta{Type: "text_delta", Text: "Hi"}},
		{Type: "content_block_stop", Index: 7},
		{Type: "content_block_start", Index: 9, ContentBlock: anthropicContentBlock{Type: "tool_use", ID: "toolu_1", Name: "read"}},
		{Type: "content_block_delta", Index: 9, Delta: anthropicDelta{Type: "input_json_delta", PartialJSON: `{"path":"x"}`}},
		{Type: "content_block_stop", Index: 9},
		{Type: "message_delta", Delta: anthropicDelta{StopReason: "tool_use"}, Usage: anthropicUsage{OutputTokens: &output, OutputTokensDetails: &outputTokensDetailsUsage{ThinkingTokens: &thinking}}},
	}
	var streamTypes []types.StreamEventType
	for _, ev := range events {
		out, err := foldAnthropicEvent(ev, state, baseModel())
		if err != nil {
			t.Fatalf("fold %s: %v", ev.Type, err)
		}
		for _, streamEvent := range out {
			streamTypes = append(streamTypes, streamEvent.Type)
		}
	}

	if strings.Join(streamEventTypes(streamTypes), ",") != "text_start,text_delta,text_end,toolcall_start,toolcall_delta,toolcall_end" {
		t.Fatalf("stream event types = %v", streamTypes)
	}
	if msg.ResponseID != "msg_1" || msg.Usage.Input != 3 || msg.Usage.Output != 5 || msg.Usage.Reasoning == nil || *msg.Usage.Reasoning != 2 {
		t.Fatalf("message metadata/usage = %+v", msg)
	}
	if msg.StopReason != types.StopToolUse || len(msg.Content) != 2 || msg.Content[0].Text != "Hi" || string(msg.Content[1].Arguments) != `{"path":"x"}` {
		t.Fatalf("message content = %+v", msg)
	}
}

func TestStreamSimpleBudgetAndAdaptiveThinking(t *testing.T) {
	forceAdaptive := true
	var adaptiveBody map[string]any
	adaptiveServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&adaptiveBody); err != nil {
			t.Errorf("decode adaptive body: %v", err)
		}
		writeMinimalAnthropicStream(w)
	}))
	defer adaptiveServer.Close()
	adaptiveModel := baseModel()
	adaptiveModel.BaseURL = adaptiveServer.URL
	adaptiveModel.Reasoning = true
	adaptiveModel.Compat = &types.AnthropicCompat{ForceAdaptiveThinking: &forceAdaptive}
	drainAssistantStream(t, StreamSimple(context.Background(), adaptiveModel, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{APIKey: "sk-test"}, Reasoning: "xhigh"}))
	if adaptiveBody["thinking"].(map[string]any)["type"] != "adaptive" || adaptiveBody["output_config"].(map[string]any)["effort"] != "high" {
		t.Fatalf("adaptive body = %#v", adaptiveBody)
	}

	var budgetBody map[string]any
	budgetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&budgetBody); err != nil {
			t.Errorf("decode budget body: %v", err)
		}
		writeMinimalAnthropicStream(w)
	}))
	defer budgetServer.Close()
	budgetModel := baseModel()
	budgetModel.BaseURL = budgetServer.URL
	budgetModel.Reasoning = true
	drainAssistantStream(t, StreamSimple(context.Background(), budgetModel, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{APIKey: "sk-test", MaxTokens: 2000}, Reasoning: "low"}))
	thinkingMap := budgetBody["thinking"].(map[string]any)
	if thinkingMap["type"] != "enabled" || int(thinkingMap["budget_tokens"].(float64)) != 2048 {
		t.Fatalf("budget body = %#v", budgetBody)
	}
}

func TestStreamHTTPAndIdleWatchdog(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "sk-test" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("bad request path/headers: path=%s headers=%v", r.URL.Path, r.Header)
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writeTextAnthropicStream(w, "Hello")
	}))
	defer server.Close()
	model := baseModel()
	model.BaseURL = server.URL

	s := Stream(context.Background(), model, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}, &Options{StreamOptions: types.StreamOptions{APIKey: "sk-test"}})
	message := drainAssistantStream(t, s)

	if requestBody["model"] != "claude-test" {
		t.Fatalf("request body = %#v", requestBody)
	}
	if message.StopReason != types.StopStop || len(message.Content) != 1 || message.Content[0].Text != "Hello" {
		t.Fatalf("message = %+v", message)
	}

	idleServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	}))
	defer idleServer.Close()
	idleModel := baseModel()
	idleModel.BaseURL = idleServer.URL
	idleStream := Stream(context.Background(), idleModel, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}, &Options{StreamOptions: types.StreamOptions{APIKey: "sk-test"}})
	idleMessage := drainAssistantStream(t, idleStream)
	if idleMessage.StopReason != types.StopError || !strings.Contains(idleMessage.ErrorMessage, "stream idle") || idleMessage.ErrorCode != string(retry.CodeStreamInterrupted) {
		t.Fatalf("idle message = %+v", idleMessage)
	}
}

func drainAssistantStream(t *testing.T, s interface {
	Events() <-chan types.StreamEvent
	Result(context.Context) (*types.AssistantMessage, error)
}) types.AssistantMessage {
	t.Helper()
	for range s.Events() {
	}
	message, err := s.Result(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if message == nil {
		t.Fatal("Result message nil")
	}
	return *message
}

func writeMinimalAnthropicStream(w http.ResponseWriter) {
	writeTextAnthropicStream(w, "ok")
}

func writeTextAnthropicStream(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":1}}}",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}",
		fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}", text),
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}",
	} {
		_, _ = fmt.Fprint(w, event+"\n\n")
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func streamEventTypes(in []types.StreamEventType) []string {
	out := make([]string, 0, len(in))
	for _, typ := range in {
		out = append(out, string(typ))
	}
	return out
}
