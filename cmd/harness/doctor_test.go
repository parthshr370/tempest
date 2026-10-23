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

	"go.harness.dev/harness/internal/adapter"
)

func TestPingTargetsAreConfiguredRoleModels(t *testing.T) {
	resolve := func(defaultModel, planModel, smolModel string) []pingTarget {
		t.Helper()
		roles, err := adapter.ResolveRoles(adapter.RoleRoutingConfig{DefaultModel: defaultModel, PlanModel: planModel, SmolModel: smolModel})
		if err != nil {
			t.Fatal(err)
		}
		return pingTargets(roles)
	}
	only := resolve("openrouter:~google/gemini-flash-latest", "", "")
	if len(only) != 1 || strings.Join(only[0].roles, "+") != "default+plan+smol" || only[0].router.Model.Provider != "openrouter" || only[0].router.Model.ID != "~google/gemini-flash-latest" {
		t.Fatalf("default-only targets: %+v", only)
	}
	split := resolve("openrouter:~google/gemini-flash-latest", "openrouter:~google/gemini-flash-latest", "openai:gpt-small@https://example.test/v1")
	if len(split) != 2 || strings.Join(split[0].roles, "+") != "default+plan" || strings.Join(split[1].roles, "+") != "smol" || split[1].router.Model.ID != "gpt-small" {
		t.Fatalf("split targets: %+v", split)
	}
}

func TestDoctorPingProbesConfiguredModelAndReportsProviderStatus(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests, http.StatusNotFound} {
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
				if body.Model != "gpt-configured" || body.MaxTokens != 16 || len(body.Messages) != 1 || body.Messages[0].Content != "hi" {
					t.Errorf("unexpected probe: %+v", body)
				}
				writer.Header().Set("Content-Type", "text/event-stream")
				writer.WriteHeader(status)
				payload := "data: {\"choices\":[{\"delta\":{\"content\":\"h\"},\"finish_reason\":\"length\"}]}\n\n"
				switch status {
				case http.StatusTooManyRequests:
					payload = key + " rate limit"
				case http.StatusNotFound:
					payload = `{"error":{"message":"allowed-providers setting permits only: google-vertex"}}`
				}
				if _, err := io.WriteString(writer, payload); err != nil {
					t.Error(err)
				}
			}))
			defer endpoint.Close()
			var output, diagnostics bytes.Buffer
			code := runDoctor([]string{"--ping", "-model", "openai:gpt-configured@" + endpoint.URL}, &output, &diagnostics)
			if calls.Load() != 1 {
				t.Fatalf("probe calls: %d output=%s", calls.Load(), &output)
			}
			if strings.Contains(output.String()+diagnostics.String(), key) {
				t.Fatal("doctor leaked key")
			}
			label := "default+plan+smol (openai:gpt-configured): "
			switch status {
			case http.StatusOK:
				if code != 0 || !strings.Contains(output.String(), label+"ok ") {
					t.Fatalf("doctor: code=%d output=%s diagnostics=%s", code, &output, &diagnostics)
				}
			case http.StatusTooManyRequests:
				if code != 1 || !strings.Contains(output.String(), label+"error rate_limited: provider HTTP 429") {
					t.Fatalf("doctor: code=%d output=%s", code, &output)
				}
			default:
				if code != 1 || !strings.Contains(output.String(), "provider HTTP 404") || !strings.Contains(output.String(), "allowed-providers setting") {
					t.Fatalf("doctor: code=%d output=%s", code, &output)
				}
			}
		})
	}
}
