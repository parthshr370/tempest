package vertex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

func TestNonstreamPublisherEndpoints(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "project")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "global")
	t.Setenv("GOOGLE_CLOUD_API_KEY", "")
	for _, modelID := range []string{"gemini-test", "claude-test"} {
		t.Run(modelID, func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if strings.HasPrefix(modelID, "claude-") {
					if !strings.HasSuffix(r.URL.Path, ":rawPredict") || request["stream"] != nil {
						t.Errorf("rawPredict request: %s %s", r.URL, request)
					}
					io.WriteString(w, `{"id":"msg_1","content":[{"type":"thinking","thinking":"plan","signature":"sig"},{"type":"tool_use","id":"call_1","name":"lookup","input":{"path":"file.txt"}}],"stop_reason":"tool_use","usage":{"input_tokens":4,"output_tokens":3}}`)
				} else {
					if !strings.HasSuffix(r.URL.Path, ":generateContent") || r.URL.Query().Get("alt") != "" {
						t.Errorf("generateContent request: %s", r.URL)
					}
					io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"plan","thought":true,"thoughtSignature":"sig"},{"functionCall":{"id":"call_1","name":"lookup","args":{"path":"file.txt"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":3,"totalTokenCount":7}}`)
				}
			}))
			defer endpoint.Close()
			model := types.Model{ID: modelID, API: "google-vertex", Provider: "vertex", BaseURL: endpoint.URL, MaxTokens: 100}
			opts := &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{Transport: "http", Headers: map[string]string{"Authorization": "Bearer token"}}}
			result, err := StreamSimple(context.Background(), model, types.Context{}, opts).Result(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result.StopReason != types.StopToolUse || len(result.Content) != 2 || result.Content[0].ThinkingSignature != "sig" || string(result.Content[1].Arguments) != `{"path":"file.txt"}` || result.Usage.Input != 4 {
				t.Fatalf("result: %+v", result)
			}
		})
	}
}
