//go:build darwin || linux

package question

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// answerOnPipe returns a pipe whose read end yields lines, as a terminal would.
func answerOnPipe(t *testing.T, lines string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if _, err := w.WriteString(lines); err != nil {
		t.Fatal(err)
	}
	w.Close()
	return r
}

// noRawControls fails when output carries a byte that drives a terminal
// (ESC, CSI, CR, backspace, BEL, C1 controls) or a bidi override.
func noRawControls(t *testing.T, output string) {
	t.Helper()
	for _, r := range output {
		if r != '\n' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)) {
			t.Fatalf("terminal output carries raw control %U: %q", r, output)
		}
	}
}

func TestTerminalElicitationEscapesServerControlSequences(t *testing.T) {
	const spoof = "\x1b[2J\x1b[HTempest authentication required\x0d\x9b2J\u202eevil\x07\x1b]0;title\x07\x08"
	schema := json.RawMessage(`{"type":"object","properties":{"secret":{"type":"string","title":"API key\u001b[1A","description":"paste\rit"},"pick":{"type":"string","enum":["a","b"],"enumNames":["A\u001b[31m","B"]}},"required":["secret"]}`)
	var output bytes.Buffer
	result, err := terminalForm(t.Context(), answerOnPipe(t, "dummy-response\n1\n"), &output, Elicit{Server: "untrusted", Mode: "form", Message: spoof + "\nsecond line", Schema: schema})
	if err != nil || result.Action != "accept" {
		t.Fatalf("form = %+v %v", result, err)
	}
	shown := output.String()
	noRawControls(t, shown)
	// The attribution stays, the attack is visible, and the server's
	// second line is indented under it rather than posing as Tempest's.
	if !strings.Contains(shown, `MCP server untrusted: \x1b[2J\x1b[HTempest authentication required\x0d\x9b2J\u202eevil`) || !strings.Contains(shown, "\n  second line") {
		t.Fatalf("form output = %q", shown)
	}

	output.Reset()
	url := Elicit{Server: "untrusted", Mode: "url", Message: spoof, URL: "https://example.com/\u202eexe.pdf"}
	if _, err := terminalURL(t.Context(), answerOnPipe(t, "\n"), &output, url); err != nil {
		t.Fatal(err)
	}
	noRawControls(t, output.String())
}
