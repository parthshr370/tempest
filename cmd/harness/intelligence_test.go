package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntelligenceFlagsReachConfigOnEveryCommand proves each session-creating
// command parses -enable-lsp, -lsp-config and -cloud-policy and resolves them
// with flag precedence: a malformed file fails startup with its field, and
// -enable-lsp=false skips the server file even when the environment enables it.
func TestIntelligenceFlagsReachConfigOnEveryCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	badServers := filepath.Join(dir, "servers.json")
	badPolicy := filepath.Join(dir, "policy.json")
	for path, content := range map[string]string{badServers: `{"servers":[{"name":"cobol","command":"x"}]}`, badPolicy: `{"rules":[{"tool":"kubectl","context":"prod-*","action":"deny"}]}`} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// One-shot stops at the missing prompt and the server commands at the
	// zero idle timeout, both after configuration resolved successfully.
	commands := map[string][]string{"one-shot": nil, "serve": {"serve", "-idle-timeout", "0"}, "mcp": {"mcp", "-idle-timeout", "0"}}
	for name, prefix := range commands {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HARNESS_ENABLE_LSP", "true")
			for _, test := range []struct {
				args []string
				want string
			}{
				{[]string{"-lsp-config", badServers}, "config lsp_config"},
				{[]string{"-cloud-policy", badPolicy}, "config cloud_policy"},
				{[]string{"-enable-lsp=false", "-lsp-config", badServers}, ""},
				{[]string{"-lsp-config", "", "-cloud-policy", ""}, ""},
			} {
				var stderr bytes.Buffer
				run(append(append([]string(nil), prefix...), test.args...), &bytes.Buffer{}, &stderr)
				text := stderr.String()
				if test.want != "" && !strings.Contains(text, test.want) {
					t.Fatalf("%v: stderr = %q, want %q", test.args, text, test.want)
				}
				if test.want == "" && strings.Contains(text, "config ") {
					t.Fatalf("%v: unexpected configuration error %q", test.args, text)
				}
			}
		})
	}
}

func TestHelpListsIntelligenceFlags(t *testing.T) {
	for _, args := range [][]string{{"-help"}, {"serve", "-help"}, {"mcp", "-help"}} {
		var stderr bytes.Buffer
		run(args, &bytes.Buffer{}, &stderr)
		for _, flagName := range []string{"-enable-lsp", "-lsp-config", "-cloud-policy"} {
			if !strings.Contains(stderr.String(), flagName) {
				t.Fatalf("%v help lacks %s", args, flagName)
			}
		}
	}
}
