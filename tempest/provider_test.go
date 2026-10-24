package tempest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/agent"
)

func TestProviderSettingsReachSDKSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HARNESS_MODEL", "")
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "" {
			t.Errorf("request=%s %v", r.URL, r.Header)
		}
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.Model != "sdk-model" {
			t.Errorf("model=%q", payload.Model)
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"SDK_PROVIDER_OK\"},\"finish_reason\":\"stop\"}]}\n\n")
	}))
	defer endpoint.Close()
	settings := filepath.Join(home, ".config", "harness", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]ProviderDefinition{"gateway": {Protocol: "openai-chat", BaseURL: endpoint.URL + "/v1", NoAuth: new(true)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, append(append([]byte(`{"providers":`), raw...), '}'), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(context.Background(), Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), New: &NewSession{Model: "gateway:sdk-model"}})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	result, err := session.Prompt(context.Background(), Prompt{Text: "hi"})
	if err != nil || result.StopReason != agent.StopCompleted || calls.Load() != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls.Load())
	}
}
