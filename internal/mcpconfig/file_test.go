package mcpconfig

import (
	"strings"
	"testing"
)

func TestParseFileAcceptsClaudeDesktopObjectShape(t *testing.T) {
	servers, err := ParseFile([]byte(`{"mcpServers": {
		"everything": {"command": "npx", "args": ["-y", "server-everything"], "env": {"TOKEN": "t"}, "permission": "allow"},
		"docs": {"url": "https://docs.example/mcp", "headers": {"Authorization": "Bearer x"}},
		"typed": {"type": "http", "url": "https://typed.example/mcp"},
		"off": {"command": "broken", "disabled": true}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := Normalize(servers)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != 3 || normalized[0].Name != "everything" || normalized[1].Name != "docs" || normalized[2].Name != "typed" {
		t.Fatalf("names/order: %#v", normalized)
	}
	if s := normalized[0]; s.Transport != "stdio" || s.Command != "npx" || len(s.Args) != 2 || s.Env["TOKEN"] != "t" || s.Permission != "allow" {
		t.Fatalf("stdio entry: %#v", s)
	}
	if s := normalized[1]; s.Transport != "http" || s.Headers["Authorization"] != "Bearer x" || s.Permission != "prompt" {
		t.Fatalf("http entry: %#v", s)
	}

	array, err := ParseFile([]byte(`{"mcpServers":[{"name":"issues","transport":"stdio","command":"issue-mcp"}]}`))
	if err != nil || len(array) != 1 || array[0].Transport != "stdio" {
		t.Fatalf("array shape: %#v %v", array, err)
	}
}

func TestParseFileShapeErrorsSayWhatIsExpected(t *testing.T) {
	for input, want := range map[string]string{
		`{"mcpServers":"npx"}`: `mcpServers must be an object keyed by server name or an array`,
		`{"servers":{}}`:       `unknown top-level field "servers"; expected {"mcpServers"`,
		`{"mcpServers":{"x":{"command":"npx","args":"-y"}}}`:                `server "x": field "args" must be an array of strings, not string`,
		`{"mcpServers":{"x":{"command":"npx","autoApprove":[]}}}`:           `server "x": unknown field "autoApprove" (supported:`,
		`{"mcpServers":{"x":{"type":"sse","url":"https://a.example/sse"}}}`: `legacy HTTP+SSE`,
		`{"mcpServers":{"x":{}}}`:                                           `set exactly one of "command"`,
		`{"mcpServers":[{"command":"npx"}]}`:                                `mcpServers[0]: missing "name"`,
	} {
		_, err := ParseFile([]byte(input))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", input, err, want)
		}
	}
}
