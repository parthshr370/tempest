package tempest_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/tempest"
)

func TestChatGPTSourceOnlyHostAndResume(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("OPENAI_BASE_URL", "https://unrelated-tool.example/v1")
	t.Setenv("HARNESS_OPENAI_BASE_URL", "https://another-tool.example/v1")
	t.Setenv("OPENAI_API_KEY", "api-key-must-not-be-used")
	var requests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path != "/v1/responses" || request.Header.Get("Authorization") != "Bearer sdk-plan-token" {
			t.Error("SDK did not use its explicit ChatGPT source")
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"SDK signed-in response\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"sdk_response\"}}\n\n")
	}))
	defer endpoint.Close()
	var acquisitions atomic.Int32
	source := func(context.Context) (tempest.RequestCredential, error) {
		acquisitions.Add(1)
		return tempest.RequestCredential{AccessToken: "sdk-plan-token", BaseURL: endpoint.URL + "/v1"}, nil
	}
	host, err := tempest.OpenHost(t.Context(), tempest.HostOptions{SessionDir: filepath.Join(root, "sessions"), Provider: tempest.ProviderConfig{ChatGPTCredentials: source}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := host.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	session, err := host.Create(t.Context(), tempest.AgentOptions{Cwd: root, New: &tempest.NewSession{Model: "openai-chatgpt:fixture-model"}})
	if err != nil {
		t.Fatal(err)
	}
	identifier := session.ID()
	for turn := range 2 {
		if _, err := session.Prompt(t.Context(), tempest.Prompt{Text: fmt.Sprintf("say hello %d", turn)}); err != nil {
			t.Fatal(err)
		}
		if session.LastAssistantText() != "SDK signed-in response" {
			t.Fatal("SDK session did not complete with its signed-in provider")
		}
		if turn == 0 {
			if err := host.CloseSession(t.Context(), identifier); err != nil {
				t.Fatal(err)
			}
			session, err = host.Open(t.Context(), identifier)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if requests.Load() != 2 || acquisitions.Load() != 2 {
		t.Fatalf("SDK request credentials: requests=%d acquisitions=%d", requests.Load(), acquisitions.Load())
	}
}

func TestChatGPTRejectsExplicitSDKOutputCap(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	source := func(context.Context) (tempest.RequestCredential, error) {
		t.Error("explicit unsupported cap reached credential acquisition")
		return tempest.RequestCredential{}, nil
	}
	host, err := tempest.OpenHost(t.Context(), tempest.HostOptions{
		SessionDir: filepath.Join(root, "sessions"),
		Provider:   tempest.ProviderConfig{ChatGPTCredentials: source, MaxTokens: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	_, err = host.Create(t.Context(), tempest.AgentOptions{Cwd: root, New: &tempest.NewSession{Model: "openai-chatgpt:fixture-model"}})
	if err == nil || !strings.Contains(err.Error(), "explicit output-token caps") {
		t.Fatalf("explicit SDK cap accepted: %v", err)
	}
}
