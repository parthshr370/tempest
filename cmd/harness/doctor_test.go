package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestDoctorCustomProviderRedactsConfiguredCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Gateway-Key") != "settings-secret" || r.URL.Query().Get("key") != "query-secret" {
			t.Error("custom credentials not sent")
		}
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "settings-secret query-secret")
	}))
	defer endpoint.Close()
	settings := filepath.Join(home, ".config", "harness", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0700); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]any{"model": "custom:test", "providers": map[string]any{"custom": map[string]any{"protocol": "openai-chat", "base_url": endpoint.URL, "api_key": "settings-secret", "auth_header": "X-Gateway-Key", "auth_prefix": "", "query": map[string]string{"key": "query-secret"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	var output, diagnostics bytes.Buffer
	if code := runDoctor([]string{"--ping"}, &output, &diagnostics); code != 1 {
		t.Fatalf("exit=%d output=%s", code, &output)
	}
	for _, secret := range []string{"settings-secret", "query-secret"} {
		if strings.Contains(output.String()+diagnostics.String(), secret) {
			t.Fatalf("doctor leaked configured credential %q", secret)
		}
	}
	if !strings.Contains(output.String(), "custom: api=openai-completions") || !strings.Contains(output.String(), "provider HTTP 401") {
		t.Fatalf("doctor custom provider=%s", &output)
	}
}
