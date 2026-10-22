package acp

import (
	"encoding/json"
	"errors"
	"testing"

	"go.harness.dev/harness/internal/mcpclient"
	"go.harness.dev/harness/tempest"
)

func TestACPServers(t *testing.T) {
	raw := []json.RawMessage{json.RawMessage(`{"name":"issues","command":"fixture","args":["--stdio"],"env":[{"name":"TOKEN","value":"secret"}]}`), json.RawMessage(`{"type":"http","name":"docs","url":"https://docs.example/mcp","headers":[{"name":"Authorization","value":"Bearer secret"}]}`)}
	servers, wireErr := convertMCPServers(raw)
	if wireErr != nil {
		t.Fatal(wireErr)
	}
	if len(servers) != 2 || servers[0].Transport != "stdio" || servers[1].Transport != "http" || servers[0].Permission != "prompt" || servers[1].Permission != "prompt" || servers[0].Env["TOKEN"] != "secret" {
		t.Fatalf("conversion %#v", servers)
	}
	inherited, wireErr := convertMCPServers(nil)
	if inherited != nil || wireErr != nil {
		t.Fatal("lost omission")
	}
	disabled, wireErr := convertMCPServers([]json.RawMessage{})
	if disabled == nil || len(disabled) != 0 || wireErr != nil {
		t.Fatal("lost explicit disable")
	}
	var invalidPresence openParams
	if json.Unmarshal([]byte(`{"mcpServers":null}`), &invalidPresence) == nil {
		t.Fatal("accepted explicit null mcpServers")
	}
	for name, definition := range map[string]string{
		"stdio-tag":        `{"type":"stdio","name":"x","command":"fixture"}`,
		"empty-tag":        `{"type":"","name":"x","command":"fixture"}`,
		"null-tag":         `{"type":null,"name":"x","command":"fixture"}`,
		"allow":            `{"name":"x","command":"fixture","permission":"allow"}`,
		"duplicate-env":    `{"name":"x","command":"fixture","env":[{"name":"TOKEN","value":"a"},{"name":"TOKEN","value":"b"}]}`,
		"duplicate-header": `{"type":"http","name":"x","url":"https://example.com","headers":[{"name":"Authorization","value":"a"},{"name":"authorization","value":"b"}]}`,
		"variant":          `{"type":"http","name":"x","url":"https://example.com","command":"fixture"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := convertMCPServers([]json.RawMessage{json.RawMessage(definition)}); err == nil || err.Code != -32602 {
				t.Fatalf("invalid params: %v", err)
			}
		})
	}
	if _, err := convertMCPServers([]json.RawMessage{json.RawMessage(`{"type":"sse","name":"x"}`)}); err == nil || err.Code != -32601 {
		t.Fatalf("SSE: %v", err)
	}
	if got := failure(tempest.ErrMCPConfigConflict); got.Code != -32012 {
		t.Fatalf("conflict: %#v", got)
	}
	startup := &mcpclient.StartupError{Server: "issues", Phase: "discovery", Err: errors.New("secret endpoint credentials")}
	got := failure(startup)
	if got.Code != -32013 || got.Message != "MCP server issues discovery failed" {
		t.Fatalf("unsanitized startup: %#v", got)
	}
}
