package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/catalog"
)

func TestCheapestCatalogModels(t *testing.T) {
	for _, provider := range catalog.Providers() {
		cheapest, found := cheapestModel(provider.ID)
		if !found {
			t.Fatalf("no priced model for %s", provider.ID)
		}
		for _, candidate := range catalog.Models() {
			if candidate.Provider == provider.ID && !candidate.Cost.Unknown && candidate.Cost.Input+candidate.Cost.Output < cheapest.Cost.Input+cheapest.Cost.Output {
				t.Fatalf("%s isn't cheapest for %s", cheapest.ID, provider.ID)
			}
		}
	}
	if _, found := cheapestModel("unconfigured-provider"); found {
		t.Fatal("unknown provider has a cheapest model")
	}
}

func TestDoctorProbeUsesOneTokenAndRedactsFailures(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "OPENROUTER_API_KEY"} {
				t.Setenv(name, "")
			}
			const key = "doctor-fixture-secret"
			t.Setenv("OPENAI_API_KEY", key)
			var calls atomic.Int32
			endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				var body struct {
					Model     string `json:"model"`
					MaxTokens int    `json:"max_completion_tokens"`
					Messages  []struct {
						Role    string `json:"role"`
						Content string `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				cheapest, _ := cheapestModel("openai")
				if body.Model != cheapest.ID || body.MaxTokens != 1 || len(body.Messages) != 1 || body.Messages[0].Role != "user" || body.Messages[0].Content != "hi" {
					t.Errorf("unexpected probe: %+v", body)
				}
				writer.Header().Set("Content-Type", "text/event-stream")
				writer.WriteHeader(status)
				payload := "data: {\"choices\":[{\"delta\":{\"content\":\"h\"},\"finish_reason\":\"length\"}]}\n\n"
				if status != http.StatusOK {
					payload = key + " rate limit"
				}
				if _, err := io.WriteString(writer, payload); err != nil {
					t.Error(err)
				}
			}))
			defer endpoint.Close()
			t.Setenv("OPENAI_BASE_URL", endpoint.URL)
			var output, diagnostics bytes.Buffer
			code := runDoctor([]string{"--ping"}, &output, &diagnostics)
			if calls.Load() != 1 {
				t.Fatalf("probe calls: %d", calls.Load())
			}
			if strings.Contains(output.String()+diagnostics.String(), key) {
				t.Fatal("doctor leaked key")
			}
			if status == http.StatusOK {
				if code != 0 || !strings.Contains(output.String(), "openai: ok ") {
					t.Fatalf("doctor: code=%d output=%s diagnostics=%s", code, &output, &diagnostics)
				}
			} else if code != 1 || !strings.Contains(output.String(), "rate_limited") {
				t.Fatalf("doctor: code=%d output=%s", code, &output)
			}
		})
	}
}
