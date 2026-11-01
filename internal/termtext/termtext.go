// Package termtext renders text another party chose (an MCP server's
// message, title, label, URL or error description) for a terminal. Control
// and format characters are shown escaped, so the text cannot clear the
// screen, move the cursor, retitle the window, overwrite a trusted prefix on
// its line or reorder what follows: ECMA-48 C0 and C1 controls (ESC and the
// CSI and OSC sequences it starts, CR, BS, BEL), and Unicode bidi and other
// format characters. The zero width joiner and non-joiner pass through: they
// only shape the characters on either side (Persian words, emoji sequences)
// and cannot move, hide or reorder text. Invalid UTF-8 bytes are shown as \xNN.
package termtext

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Line renders text on one line; line breaks are escaped too.
func Line(text string) string { return render(text, false) }

// Message renders text keeping its line breaks, indenting each following
// line by two spaces so it stays visibly under the line that attributes it.
func Message(text string) string { return render(text, true) }

func render(text string, multiline bool) string {
	var out strings.Builder
	for i, r := range text {
		switch {
		case r == utf8.RuneError && !strings.HasPrefix(text[i:], "\uFFFD"):
			fmt.Fprintf(&out, `\x%02x`, text[i])
		case r == '\n' && multiline:
			out.WriteString("\n  ")
		case r == '\t':
			out.WriteByte(' ')
		case r < 0x100 && unicode.IsControl(r):
			fmt.Fprintf(&out, `\x%02x`, r)
		case r == '\u200c' || r == '\u200d':
			out.WriteRune(r)
		case unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) && r <= 0xffff:
			fmt.Fprintf(&out, `\u%04x`, r)
		case unicode.In(r, unicode.Cf):
			fmt.Fprintf(&out, `\U%08x`, r)
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}
