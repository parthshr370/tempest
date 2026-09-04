package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/types"
)

func TestMain(m *testing.M) {
	streamIdleTimeout = 50 * time.Millisecond
	os.Exit(m.Run())
}

func baseModel() types.Model {
	return types.Model{
		ID:            "gpt-4o",
		Name:          "GPT-4o",
		API:           "openai-completions",
		Provider:      "openai",
		ContextWindow: 128000,
		MaxTokens:     4096,
		Cost:          types.ModelCost{Input: 2.5, Output: 10, CacheRead: 1.25, CacheWrite: 2.5},
	}
}

func TestBuildParamsBasics(t *testing.T) {
	model := baseModel()
	temp := 0.7
	opts := &Options{
		StreamOptions: types.StreamOptions{
			MaxTokens:   2048,
			Temperature: &temp,
		},
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

	params := buildParams(model, ctx, opts)

	if params["model"] != "gpt-4o" || params["stream"] != true {
		t.Fatalf("basic params = %#v", params)
	}
	if _, ok := params["max_completion_tokens"]; !ok || params["max_completion_tokens"].(int) != 2048 {
		t.Fatalf("max_completion_tokens = %v", params["max_completion_tokens"])
	}
	if params["temperature"].(float64) != 0.7 {
		t.Fatalf("temperature = %v", params["temperature"])
	}
	tools := params["tools"].([]map[string]any)
	if len(tools) != 1 || tools[0]["type"] != "function" {
		t.Fatalf("tools = %#v", tools)
	}
	fn := tools[0]["function"].(map[string]any)
	if fn["name"] != "read" || fn["description"] != "Read a file" {
		t.Fatalf("function = %#v", fn)
	}

	messages := params["messages"].([]map[string]any)
	if len(messages) < 2 {
		t.Fatalf("messages len = %d", len(messages))
	}
	if messages[0]["role"] != "system" || messages[0]["content"] != "system prompt" {
		t.Fatalf("system message = %#v", messages[0])
	}
}

func TestConvertMessagesGroupsToolResults(t *testing.T) {
	model := baseModel()
	ctx := types.Context{
		Messages: []types.Message{
			types.AssistantMessage{API: "other", Provider: "other", Model: "other", Content: []types.ContentBlock{
				types.NewToolCall("tc1", "read", json.RawMessage(`{"path":"x"}`)),
			}},
			types.ToolResultMessage{ToolCallID: "tc1", ToolName: "read", Content: []types.ContentBlock{types.NewText("result one")}},
			types.ToolResultMessage{ToolCallID: "tc2", ToolName: "grep", Content: []types.ContentBlock{types.NewText("result two")}},
		},
	}

	messages := convertMessages(model, ctx)
	if len(messages) < 2 {
		t.Fatalf("messages len = %d: %#v", len(messages), messages)
	}
	assistantMsg := messages[0]
	if assistantMsg["role"] != "assistant" {
		t.Fatalf("first msg role = %v", assistantMsg["role"])
	}
	toolCalls := assistantMsg["tool_calls"].([]map[string]any)
	if len(toolCalls) != 1 || toolCalls[0]["id"] != "tc1" {
		t.Fatalf("tool_calls = %#v", toolCalls)
	}

	foundToolOne := false
	foundToolTwo := false
	for _, msg := range messages {
		if msg["role"] == "tool" {
			tr := msg
			if tr["tool_call_id"] == "tc1" {
				foundToolOne = true
			}
			if tr["tool_call_id"] == "tc2" {
				foundToolTwo = true
			}
		}
	}
	if !foundToolOne || !foundToolTwo {
		t.Fatalf("missing tool results in %#v", messages)
	}
}

func TestMapOpenAIStopReason(t *testing.T) {
	cases := []struct {
		reason     string
		wantReason types.StopReason
		wantErr    bool
	}{
		{"stop", types.StopStop, false},
		{"end", types.StopStop, false},
		{"length", types.StopLength, false},
		{"function_call", types.StopToolUse, false},
		{"tool_calls", types.StopToolUse, false},
		{"content_filter", types.StopError, false},
		{"network_error", types.StopError, false},
		{"unknown", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			got, _, err := mapOpenAIStopReason(tc.reason)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %s", tc.reason)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.wantErr && got != tc.wantReason {
				t.Fatalf("got %s, want %s", got, tc.wantReason)
			}
		})
	}
}

func TestFoldOpenAIChunksBuildsMessage(t *testing.T) {
	model := baseModel()
	state := &openAIFoldState{
		Output:          newOutputMessage(model),
		ToolCallByIndex: map[int]*openAIFoldBlock{},
		ToolCallByID:    map[string]*openAIFoldBlock{},
	}

	chunks := []openAIStreamChunk{
		{ID: "chat123", Choices: []openAIStreamChoice{{Delta: openAIDelta{Content: "Hello "}}}},
		{Choices: []openAIStreamChoice{{Delta: openAIDelta{Content: "world"}}}},
		{Choices: []openAIStreamChoice{{Delta: openAIDelta{ToolCalls: []openAIToolCallDelta{
			{Index: intPtr(0), ID: "call_1", Function: &openAIFuncDelta{Name: "read"}},
		}}}}},
		{Choices: []openAIStreamChoice{{Delta: openAIDelta{ToolCalls: []openAIToolCallDelta{
			{Index: intPtr(0), Function: &openAIFuncDelta{Arguments: `{"path`}},
		}}}}},
		{Choices: []openAIStreamChoice{{Delta: openAIDelta{ToolCalls: []openAIToolCallDelta{
			{Index: intPtr(0), Function: &openAIFuncDelta{Arguments: `":"x"}`}},
		}}}}},
		{Choices: []openAIStreamChoice{{FinishReason: strPtr("tool_calls")}}, Usage: json.RawMessage(`{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}`)},
	}

	var streamTypes []types.StreamEventType
	for _, chunk := range chunks {
		out, err := foldOpenAIChunk(&chunk, state, model)
		if err != nil {
			t.Fatalf("fold error: %v", err)
		}
		for _, ev := range out {
			streamTypes = append(streamTypes, ev.Type)
		}
	}
	finalEvents := foldOpenAIFinalizeBlocks(state)
	for _, ev := range finalEvents {
		streamTypes = append(streamTypes, ev.Type)
	}

	got := streamEventTypes(streamTypes)
	want := "text_start,text_delta,text_delta,toolcall_start,toolcall_delta,toolcall_delta,text_end,toolcall_end"
	if strings.Join(got, ",") != want {
		t.Fatalf("got stream events:  %v\nwant: %v", strings.Join(got, ","), want)
	}
	if state.Output.StopReason != types.StopToolUse {
		t.Fatalf("stop reason = %s", state.Output.StopReason)
	}
	if len(state.Output.Content) != 2 {
		t.Fatalf("content len = %d: %+v", len(state.Output.Content), state.Output.Content)
	}
	if state.Output.Content[0].Text != "Hello world" {
		t.Fatalf("text = %q", state.Output.Content[0].Text)
	}
	if string(state.Output.Content[1].Arguments) != `{"path":"x"}` {
		t.Fatalf("args = %s", state.Output.Content[1].Arguments)
	}
	if state.Output.Usage.Input != 10 || state.Output.Usage.Output != 20 {
		t.Fatalf("usage = %+v", state.Output.Usage)
	}
}

func TestStreamHTTPAndIdleWatchdog(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("authorization") != "Bearer sk-test" {
			t.Errorf("bad request path/headers: path=%s headers=%v", r.URL.Path, r.Header)
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writeTextOpenAIStream(w, "Hello")
	}))
	defer server.Close()
	model := baseModel()
	model.BaseURL = server.URL

	s := Stream(context.Background(), model, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}, &Options{StreamOptions: types.StreamOptions{APIKey: "sk-test"}})
	message := drainAssistantStream(t, s)

	if requestBody["model"] != "gpt-4o" {
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
	if idleMessage.StopReason != types.StopError || !strings.Contains(idleMessage.ErrorMessage, "stream idle") {
		t.Fatalf("idle message = %+v", idleMessage)
	}
	if !IsStreamTruncated(streamError(nil, errors.New("context canceled"), true)) {
		t.Fatal("idle streamError should be StreamTruncated")
	}
}

func TestStreamSimple(t *testing.T) {
	var reqBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writeTextOpenAIStream(w, "ok")
	}))
	defer server.Close()

	model := baseModel()
	model.BaseURL = server.URL
	drainAssistantStream(t, StreamSimple(context.Background(), model, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{APIKey: "sk-test"}, Reasoning: "high"}))

	if reqBody["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v", reqBody["reasoning_effort"])
	}
}

// Regression: a single delta carrying id+name+arguments together must not drop
// its arguments (the block-creation path used to return before folding args).
func TestFoldOpenAISingleDeltaToolCallArgs(t *testing.T) {
	model := baseModel()
	state := &openAIFoldState{
		Output:          newOutputMessage(model),
		ToolCallByIndex: map[int]*openAIFoldBlock{},
		ToolCallByID:    map[string]*openAIFoldBlock{},
	}
	chunk := openAIStreamChunk{Choices: []openAIStreamChoice{{Delta: openAIDelta{ToolCalls: []openAIToolCallDelta{
		{Index: intPtr(0), ID: "call_1", Function: &openAIFuncDelta{Name: "read", Arguments: `{"path":"x"}`}},
	}}}}}
	if _, err := foldOpenAIChunk(&chunk, state, model); err != nil {
		t.Fatalf("fold error: %v", err)
	}
	foldOpenAIFinalizeBlocks(state)
	if len(state.Output.Content) != 1 || state.Output.Content[0].Type != types.BlockToolCall {
		t.Fatalf("content = %+v", state.Output.Content)
	}
	if got := string(state.Output.Content[0].Arguments); got != `{"path":"x"}` {
		t.Fatalf("single-delta tool call args dropped: got %s", got)
	}
	if state.Output.Content[0].Name != "read" {
		t.Fatalf("name = %q", state.Output.Content[0].Name)
	}
}

// Regression: whitespace-only content deltas (blank lines/indentation) must be
// preserved, not dropped by a trimmed-non-empty gate.
func TestFoldOpenAIWhitespaceDeltaPreserved(t *testing.T) {
	model := baseModel()
	state := &openAIFoldState{
		Output:          newOutputMessage(model),
		ToolCallByIndex: map[int]*openAIFoldBlock{},
		ToolCallByID:    map[string]*openAIFoldBlock{},
	}
	for _, part := range []string{"foo", "\n\n", "bar"} {
		chunk := openAIStreamChunk{Choices: []openAIStreamChoice{{Delta: openAIDelta{Content: part}}}}
		if _, err := foldOpenAIChunk(&chunk, state, model); err != nil {
			t.Fatalf("fold error: %v", err)
		}
	}
	if len(state.Output.Content) != 1 || state.Output.Content[0].Text != "foo\n\nbar" {
		t.Fatalf("whitespace delta dropped: %q", state.Output.Content[0].Text)
	}
}

// Regression: a stream that ends without a finish_reason is a truncated response,
// not a successful completion.
func TestStreamMissingFinishReasonTruncates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, `data: {"id":"c1","choices":[{"delta":{"content":"partial"}}]}`+"\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer server.Close()
	model := baseModel()
	model.BaseURL = server.URL
	s := Stream(context.Background(), model, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}, &Options{StreamOptions: types.StreamOptions{APIKey: "sk-test"}})
	msg := drainAssistantStream(t, s)
	if msg.StopReason != types.StopError {
		t.Fatalf("expected StopError for missing finish_reason, got %s", msg.StopReason)
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

func writeTextOpenAIStream(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	events := []string{
		fmt.Sprintf(`data: {"id":"chat1","object":"chat.completion.chunk","choices":[{"delta":{"content":%q}}]}`, text),
		`data: {"id":"chat1","object":"chat.completion.chunk","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
		"data: [DONE]",
	}
	for _, event := range events {
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

func intPtr(v int) *int       { return &v }
func strPtr(v string) *string { return &v }

// An effort-SKU model whose catalog compat says none-effort must send
// reasoning_effort "none" when thinking is off; with tools present, upstream
// (Bifrost/OpenAI) otherwise rejects the request outright. Live-verified
// against Bifrost gpt-5.6-luna, 2026-09-04.
func TestStreamSimpleNoneEffortDisable(t *testing.T) {
	var reqBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		reqBody = body
		writeTextOpenAIStream(w, "ok")
	}))
	defer server.Close()

	model := baseModel()
	model.BaseURL = server.URL
	model.OpenAICompat = &types.OpenAICompat{ReasoningDisableMode: types.ReasoningDisableNoneEffort}
	drainAssistantStream(t, StreamSimple(context.Background(), model, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{APIKey: "sk-test"}}))
	if reqBody["reasoning_effort"] != "none" {
		t.Fatalf("reasoning_effort = %v, want none", reqBody["reasoning_effort"])
	}

	// An explicit level must win over the disable dialect.
	drainAssistantStream(t, StreamSimple(context.Background(), model, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{APIKey: "sk-test"}, Reasoning: "high"}))
	if reqBody["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high", reqBody["reasoning_effort"])
	}

	// Without the compat flag the field stays omitted.
	model.OpenAICompat = nil
	drainAssistantStream(t, StreamSimple(context.Background(), model, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{APIKey: "sk-test"}}))
	if _, present := reqBody["reasoning_effort"]; present {
		t.Fatalf("reasoning_effort unexpectedly present: %v", reqBody["reasoning_effort"])
	}
}
