package termtext

import "testing"

func TestEscapesControlsAndKeepsText(t *testing.T) {
	for _, c := range []struct{ in, line, message string }{
		{"plain ünïcode 日本", "plain ünïcode 日本", "plain ünïcode 日本"},
		{"می\u200cروم 👩\u200d💻", "می\u200cروم 👩\u200d💻", "می\u200cروم 👩\u200d💻"},
		{"\u200e\u200f\u061c\u2069\ufeff\u00ad", `\u200e\u200f\u061c\u2069\ufeff\u00ad`, `\u200e\u200f\u061c\u2069\ufeff\u00ad`},
		{"\x1b[2J\x1b]0;t\x07", `\x1b[2J\x1b]0;t\x07`, `\x1b[2J\x1b]0;t\x07`},
		{"a\rb\bc\x7f\u009b", `a\x0db\x08c\x7f\x9b`, `a\x0db\x08c\x7f\x9b`},
		{"one\ntwo", `one\x0atwo`, "one\n  two"},
		{"\u202eexe\u2066\u2028\U000e0001", `\u202eexe\u2066\u2028\U000e0001`, `\u202eexe\u2066\u2028\U000e0001`},
		{"bad\xffbyte \uFFFD", `bad\xffbyte ` + "\uFFFD", `bad\xffbyte ` + "\uFFFD"},
		{"tab\there", "tab here", "tab here"},
	} {
		if got := Line(c.in); got != c.line {
			t.Errorf("Line(%q) = %q, want %q", c.in, got, c.line)
		}
		if got := Message(c.in); got != c.message {
			t.Errorf("Message(%q) = %q, want %q", c.in, got, c.message)
		}
	}
}
