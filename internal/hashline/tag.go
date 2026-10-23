// Package hashline implements the line-anchored patch language of the
// hashline tool: sections bound to a 4-hex content tag minted by read and
// grep, hunks that name original line numbers (PUT, CUT, REM, MV), clipboard
// registers for moves, and block locators resolved without tree-sitter.
//
// Text handled here is normalized: no byte-order mark and LF line endings.
// [Decode] produces it from raw file bytes and remembers how to restore the
// original BOM and line-ending convention on write.
package hashline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// TagLength is the number of hex characters in a section tag.
const TagLength = 4

const utf8BOM = "\ufeff"

// File is decoded file content plus what is needed to encode it back.
type File struct {
	// Text is the normalized content: BOM removed, CRLF and lone CR as LF.
	Text string
	// BOM reports a leading UTF-8 byte-order mark.
	BOM bool
	// CRLF reports that the first line break was CRLF.
	CRLF bool
}

// Decode normalizes raw file bytes.
func Decode(raw []byte) File {
	var file File
	if bytes.HasPrefix(raw, []byte(utf8BOM)) {
		file.BOM = true
		raw = raw[len(utf8BOM):]
	}
	text := string(raw)
	if lf := strings.IndexByte(text, '\n'); lf > 0 && text[lf-1] == '\r' {
		file.CRLF = true
	}
	file.Text = normalizeText(text)
	return file
}

// Encode restores the file's BOM and line-ending convention on text.
func (f File) Encode(text string) []byte {
	if f.CRLF {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	if f.BOM {
		text = utf8BOM + text
	}
	return []byte(text)
}

// normalizeText converts CRLF and lone CR line breaks to LF, the way read
// splits lines, so line numbers agree between read and hashline.
func normalizeText(text string) string {
	if !strings.Contains(text, "\r") {
		return text
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
}

// Tag returns the 4-hex section tag of raw file bytes.
func Tag(raw []byte) string {
	return TagText(Decode(raw).Text)
}

// TagText returns the 4-hex section tag of normalized text.
func TagText(text string) string {
	return tagOf(sumText(text))
}

func sumText(text string) [sha256.Size]byte {
	return sha256.Sum256([]byte(text))
}

func tagOf(sum [sha256.Size]byte) string {
	return strings.ToUpper(hex.EncodeToString(sum[:TagLength/2]))
}

// Lines splits normalized text into addressable lines. A final line break
// terminates the last line; it does not start an empty one.
func Lines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if strings.HasSuffix(text, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// joinLines rebuilds text from lines. The original's trailing line break is
// kept; an originally empty file gains one when content is added.
func joinLines(lines []string, original string) string {
	if len(lines) == 0 {
		return ""
	}
	text := strings.Join(lines, "\n")
	if original == "" || strings.HasSuffix(original, "\n") {
		text += "\n"
	}
	return text
}

// IsTag reports whether s has the shape of a section tag (4 hex digits,
// either case).
func IsTag(s string) bool {
	if len(s) != TagLength {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
