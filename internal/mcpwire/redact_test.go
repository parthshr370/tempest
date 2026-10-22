package mcpwire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/internal/rpc"
)

func TestCredentialRedactor(t *testing.T) {
	redactor := newCredentialRedactor(mcpconfig.Server{
		Headers: map[string]string{"X-Private": "abcdefgh-long"},
		Env:     map[string]string{"LONG": "abcdefgh", "ESCAPED": "quoted\"value\\secret", "SHORT": "1234567"},
	})
	if got := redactor.text("abcdefgh-long abcdefgh 1234567"); got != "[redacted] [redacted] 1234567" {
		t.Fatalf("overlapping credentials and short values: %q", got)
	}
	raw := json.RawMessage(`{"abcdefgh":"quoted\"value\\secret","text":"\u0061bcdefgh-long","number":9007199254740993,"short":"1234567"}`)
	want := `{"[redacted]":"[redacted]","text":"[redacted]","number":9007199254740993,"short":"1234567"}`
	if got := string(redactor.json(raw)); got != want {
		t.Fatalf("decoded JSON redaction: %s", got)
	}
	original := rpc.NewError(-32000, "failure", "peer rejected abcdefgh-long")
	original.Data = map[string]any{"detail": []any{"abcdefgh-long"}}
	var remote *rpc.Error
	if err := redactor.err(original); !errors.As(err, &remote) || remote.Code != -32000 || strings.Contains(fmt.Sprint(remote), "abcdefgh") || strings.Contains(fmt.Sprint(remote.Data), "abcdefgh") {
		t.Fatalf("RPC error classification/redaction: %v", err)
	}
	if !strings.Contains(original.Message, "abcdefgh-long") {
		t.Fatal("redaction mutated the transport's error")
	}
	failure := fmt.Errorf("abcdefgh-long: %w", context.Canceled)
	if err := redactor.err(failure); !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "abcdefgh") {
		t.Fatalf("context classification/redaction: %v", err)
	}
}

func TestConfiguredHeaderIsRedactedFromSuccessfulToolResults(t *testing.T) {
	const secret = "qa-private-header-success-483921"
	peer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var message httpMessage
		if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		switch message.Method {
		case "initialize":
			writeRPC(t, writer, message.ID, json.RawMessage(`{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"qa","version":"1"}}`))
		case "tools/call":
			raw, err := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": request.Header.Get("X-Private")}}, "structuredContent": map[string]any{"echo": secret}})
			if err != nil {
				t.Error(err)
				return
			}
			writeRPC(t, writer, message.ID, raw)
		default:
			writer.WriteHeader(http.StatusAccepted)
		}
	}))
	defer peer.Close()
	client, err := Dial(t.Context(), mcpconfig.Server{Name: "qa", Transport: "http", URL: peer.URL, Headers: map[string]string{"X-Private": secret}}, t.TempDir(), Handlers{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeWire(t, client)
	raw, err := client.Call(t.Context(), "tools/call", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Count(string(raw), "[redacted]") != 2 {
		t.Fatalf("tool result leaked configured header: %s", raw)
	}
}
