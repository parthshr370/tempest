package google

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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
		ID:            "gemini-2.5-flash",
		Name:          "Gemini 2.5 Flash",
		API:           "google-generative-ai",
		Provider:      "google",
		ContextWindow: 1048576,
		MaxTokens:     8192,
		Cost:          types.ModelCost{Input: 0.15, Output: 0.6},
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

	if params["model"] != "gemini-2.5-flash" {
		t.Fatalf("model = %v", params["model"])
	}

	sys, ok := params["systemInstruction"].(map[string]any)
	if !ok || sys == nil {
		t.Fatalf("systemInstruction missing: %#v", params)
	}
	parts := sys["parts"].([]map[string]any)
	if parts[0]["text"] != "system prompt" {
		t.Fatalf("system parts = %#v", parts)
	}

	contents := params["contents"].([]map[string]any)
	if len(contents) != 1 || contents[0]["role"] != "user" {
		t.Fatalf("contents = %#v", contents)
	}

	tools := params["tools"].([]map[string]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", tools)
	}

	gc := params["generationConfig"].(map[string]any)
	if gc["temperature"] != 0.7 || gc["maxOutputTokens"] != 2048 {
		t.Fatalf("generationConfig = %#v", gc)
	}
}

func TestConvertMessagesUserAndAssistant(t *testing.T) {
	model := baseModel()
	messages := []types.Message{
		types.UserMessage{Content: types.StringContent("hello")},
		types.AssistantMessage{API: "google-generative-ai", Provider: "google", Model: "gemini-2.5-flash", Content: []types.ContentBlock{
			types.NewText("hi there"),
		}},
	}

	contents := convertMessages(messages, model)
	if len(contents) != 2 {
		t.Fatalf("contents len = %d: %#v", len(contents), contents)
	}
	if contents[0]["role"] != "user" || contents[1]["role"] != "model" {
		t.Fatalf("roles = %v, %v", contents[0]["role"], contents[1]["role"])
	}
}

func TestConvertMessagesToolResults(t *testing.T) {
	model := baseModel()
	messages := []types.Message{
		types.AssistantMessage{Content: []types.ContentBlock{
			types.NewToolCall("tc1", "read", json.RawMessage(`{}`)),
		}, API: "google-generative-ai", Provider: "google", Model: "gemini-2.5-flash"},
		types.ToolResultMessage{ToolCallID: "tc1", ToolName: "read", Content: []types.ContentBlock{types.NewText("result")}},
	}

	contents := convertMessages(messages, model)
	foundFR := false
	for _, c := range contents {
		if parts, ok := c["parts"].([]map[string]any); ok {
			for _, p := range parts {
				if _, ok := p["functionResponse"]; ok {
					foundFR = true
				}
			}
		}
	}
	if !foundFR {
		t.Fatalf("functionResponse not found in %#v", contents)
	}
	if contents[len(contents)-1]["role"] != "user" {
		t.Fatalf("last content should be user: %#v", contents[len(contents)-1])
	}
}

// Regression: an errored tool result must keep its text under the "error" key so
// the model can see why the tool failed (it used to be blanked to "").
func TestConvertMessagesToolErrorKeepsText(t *testing.T) {
	model := baseModel()
	messages := []types.Message{
		types.AssistantMessage{Content: []types.ContentBlock{
			types.NewToolCall("tc1", "read", json.RawMessage(`{}`)),
		}, API: "google-generative-ai", Provider: "google", Model: "gemini-2.5-flash"},
		types.ToolResultMessage{ToolCallID: "tc1", ToolName: "read", IsError: true, Content: []types.ContentBlock{types.NewText("file not found")}},
	}

	contents := convertMessages(messages, model)
	last := contents[len(contents)-1]
	parts := last["parts"].([]map[string]any)
	fr := parts[0]["functionResponse"].(map[string]any)
	resp := fr["response"].(map[string]any)
	if got, ok := resp["error"]; !ok || got != "file not found" {
		t.Fatalf("tool error text lost: response=%#v", resp)
	}
}

func TestMapStopReason(t *testing.T) {
	cases := map[string]types.StopReason{
		"STOP":                      types.StopStop,
		"MAX_TOKENS":                types.StopLength,
		"SAFETY":                    types.StopError,
		"MALFORMED_FUNCTION_CALL":   types.StopError,
		"BLOCKLIST":                 types.StopError,
		"FINISH_REASON_UNSPECIFIED": types.StopError,
		"UNKNOWN_REASON":            types.StopError,
	}
	for reason, want := range cases {
		got := mapStopReason(reason)
		if got != want {
			t.Errorf("mapStopReason(%q) = %s, want %s", reason, got, want)
		}
	}
}

func TestFoldGoogleChunksBuildsMessage(t *testing.T) {
	model := baseModel()
	state := &googleFoldState{Output: newOutputMessage(model)}

	chunks := []googleStreamChunk{
		{Candidates: []googleCandidate{{Content: &googleContent{Parts: []googlePart{{Text: "Hello "}}}}}},
		{Candidates: []googleCandidate{{Content: &googleContent{Parts: []googlePart{{Text: "world"}}}}}},
		{Candidates: []googleCandidate{{Content: &googleContent{Parts: []googlePart{
			{FunctionCall: &googleFuncCall{Name: "read", Args: map[string]any{"path": "x"}}},
		}}}}},
		{Candidates: []googleCandidate{{FinishReason: "STOP"}}, UsageMetadata: &googleUsageMetadata{PromptTokenCount: 10, CandidatesTokenCount: 5, ThoughtsTokenCount: 0, CachedContentTokenCount: 2, TotalTokenCount: 15}},
	}

	var streamTypes []types.StreamEventType
	for _, chunk := range chunks {
		out, err := foldGoogleChunk(&chunk, state, model)
		if err != nil {
			t.Fatalf("fold error: %v", err)
		}
		for _, ev := range out {
			streamTypes = append(streamTypes, ev.Type)
		}
	}
	finalEvents := foldGoogleFinalizeBlocks(state)
	for _, ev := range finalEvents {
		streamTypes = append(streamTypes, ev.Type)
	}

	got := streamEventTypes(streamTypes)
	want := "text_start,text_delta,text_delta,toolcall_start,toolcall_delta,toolcall_end,text_end"
	if strings.Join(got, ",") != want {
		t.Fatalf("got stream events:  %v\nwant: %v", strings.Join(got, ","), want)
	}
	if state.Output.StopReason != types.StopToolUse {
		t.Fatalf("stop reason = %s", state.Output.StopReason)
	}
	if len(state.Output.Content) != 2 {
		t.Fatalf("content len = %d", len(state.Output.Content))
	}
	if state.Output.Content[0].Text != "Hello world" {
		t.Fatalf("text = %q", state.Output.Content[0].Text)
	}
	if string(state.Output.Content[1].Arguments) != `{"path":"x"}` {
		t.Fatalf("args = %s", state.Output.Content[1].Arguments)
	}
	if state.Output.Usage.Input != 8 || state.Output.Usage.CacheRead != 2 {
		t.Fatalf("usage input=%d cacheRead=%d", state.Output.Usage.Input, state.Output.Usage.CacheRead)
	}
}

func TestStreamHTTPAndIdleWatchdog(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "sk-test" {
			t.Errorf("bad auth header: %v", r.Header)
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writeTextGoogleStream(w, "Hello")
	}))
	defer server.Close()
	model := baseModel()
	model.BaseURL = server.URL

	s := Stream(context.Background(), model, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}, &Options{StreamOptions: types.StreamOptions{APIKey: "sk-test"}})
	message := drainAssistantStream(t, s)

	if requestBody["model"] != "gemini-2.5-flash" {
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

func TestStreamSimple(t *testing.T) {
	var reqBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writeTextGoogleStream(w, "ok")
	}))
	defer server.Close()

	model := baseModel()
	model.BaseURL = server.URL
	model.Reasoning = true
	drainAssistantStream(t, StreamSimple(context.Background(), model, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{APIKey: "sk-test"}, Reasoning: "high"}))

	var request struct {
		Generation struct {
			Thinking struct {
				IncludeThoughts bool `json:"includeThoughts"`
			} `json:"thinkingConfig"`
		} `json:"generationConfig"`
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if !request.Generation.Thinking.IncludeThoughts {
		t.Fatalf("generationConfig.thinkingConfig missing: %s", body)
	}
}

func TestToolCallIDMonotonic(t *testing.T) {
	model := baseModel()
	state := &googleFoldState{Output: newOutputMessage(model)}

	chunk := googleStreamChunk{
		Candidates: []googleCandidate{{
			Content: &googleContent{Parts: []googlePart{
				{FunctionCall: &googleFuncCall{Name: "read", Args: map[string]any{}}},
				{FunctionCall: &googleFuncCall{Name: "write", Args: map[string]any{}}},
			}},
		}},
	}

	events, err := foldGoogleChunk(&chunk, state, model)
	if err != nil {
		t.Fatalf("fold error: %v", err)
	}
	if len(state.Output.Content) != 2 {
		t.Fatalf("content len = %d", len(state.Output.Content))
	}
	id1 := state.Output.Content[0].ID
	id2 := state.Output.Content[1].ID
	if id1 == "" || id2 == "" {
		t.Fatalf("tool call IDs empty: %s, %s", id1, id2)
	}
	if id1 == id2 {
		t.Fatalf("tool call IDs should be unique: %s", id1)
	}
	t.Logf("tool call IDs: %s, %s, events=%v", id1, id2, len(events))
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

func writeTextGoogleStream(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	events := []string{
		fmt.Sprintf(`data: {"candidates":[{"content":{"parts":[{"text":%q}]}}]}`, text),
		`data: {"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2,"thoughtsTokenCount":0,"cachedContentTokenCount":0,"totalTokenCount":3}}`,
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
