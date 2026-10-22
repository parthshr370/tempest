package mcpwire

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/mcpconfig"
)

func TestConfiguredHTTPHeaderSecretIsRedactedFromRemoteErrors(t *testing.T) {
	const secret = "qa-private-header-canary-483921"
	for _, phase := range []string{"initialize", "tools/call"} {
		t.Run(phase, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet {
					writer.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				var message httpMessage
				if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
					t.Errorf("decode request: %v", err)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				if message.Method == phase {
					writer.Header().Set("Content-Type", "application/json")
					_, err := fmt.Fprintf(writer, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":%q}}`, message.ID, "upstream rejected X-Private-Key="+request.Header.Get("X-Private-Key"))
					if err != nil {
						t.Errorf("write error response: %v", err)
					}
					return
				}
				if message.Method == "initialize" {
					writeRPC(t, writer, message.ID, json.RawMessage(`{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"qa","version":"1"}}`))
					return
				}
				writer.WriteHeader(http.StatusAccepted)
			}))
			defer peer.Close()
			client, err := Dial(context.Background(), mcpconfig.Server{Name: "qa", Transport: "http", URL: peer.URL, Headers: map[string]string{"X-Private-Key": secret}}, t.TempDir(), Handlers{})
			if client != nil {
				defer closeWire(t, client)
			}
			if phase != "initialize" {
				if err != nil {
					t.Fatal(err)
				}
				_, err = client.Call(context.Background(), "tools/call", struct{}{})
			}
			if err == nil {
				t.Fatal("expected remote failure")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("configured HTTP header secret escaped through remote error diagnostics")
			}
		})
	}
}
