package mcpconfig

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	input := []Server{{Name: "issues", Transport: "stdio", Command: "fixture", Args: []string{"original"}, Env: map[string]string{"TOKEN": "secret"}, StartupTimeout: "0.5m"}}
	normalized, err := Normalize(input)
	if err != nil {
		t.Fatal(err)
	}
	input[0].Args[0] = "changed"
	input[0].Env["TOKEN"] = "changed"
	if normalized[0].Args[0] != "original" || normalized[0].Env["TOKEN"] != "secret" || normalized[0].StartupTimeout != "30s" || normalized[0].Permission != "prompt" {
		t.Fatalf("normalization lost ownership/defaults: %#v", normalized)
	}
	empty, err := Normalize([]Server{})
	if err != nil || empty == nil {
		t.Fatal("explicit empty lost")
	}
}
func TestMCPConfigRejectsUnsafeVariants(t *testing.T) {
	validHTTP := Server{Name: "docs", Transport: "http", URL: "http://127.0.0.1/mcp"}
	cases := map[string]Server{
		"name":            {Name: "bad__name", Transport: "stdio", Command: "fixture"},
		"stdio-url":       {Name: "x", Transport: "stdio", Command: "fixture", URL: "https://example.com"},
		"remote-http":     {Name: "x", Transport: "http", URL: "http://example.com/mcp"},
		"credentials-url": {Name: "x", Transport: "http", URL: "https://user:secret@example.com/mcp"},
		"fragment":        {Name: "x", Transport: "http", URL: "https://example.com/mcp#fragment"},
		"timeout":         {Name: "x", Transport: "stdio", Command: "fixture", CallTimeout: "0s"},
		"variant":         {Name: "x", Transport: "http", URL: "https://example.com/mcp", Command: "fixture"},
		"env":             {Name: "x", Transport: "stdio", Command: "fixture", Env: map[string]string{"BAD=KEY": "v"}},
	}
	for _, header := range []string{"Host", "Content-Length", "Accept", "Content-Type", "MCP-Protocol-Version", "mCp-SeSsIoN-Id", "Last-Event-ID", "bad\r\nkey"} {
		server := validHTTP
		server.Headers = map[string]string{header: "spoof"}
		cases[header] = server
	}
	server := validHTTP
	server.Headers = map[string]string{"Authorization": "secret\r\ninjected: yes"}
	cases["header-value"] = server
	for name, server := range cases {
		t.Run(name, func(t *testing.T) {
			if Validate([]Server{server}) == nil {
				t.Fatal("accepted unsafe configuration")
			}
		})
	}
	if err := Validate([]Server{validHTTP, validHTTP}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate: %v", err)
	}
}
