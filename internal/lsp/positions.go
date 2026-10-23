package lsp

import (
	"fmt"
	"sort"
	"unicode/utf8"
)

// lspPosition is a zero-based LSP position in UTF-16 code units.
type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

const maxProtocolUint = 1<<31 - 1

func (p lspPosition) valid() bool {
	return p.Line >= 0 && p.Character >= 0 && p.Line <= maxProtocolUint && p.Character <= maxProtocolUint
}

func (p lspPosition) less(q lspPosition) bool {
	return p.Line < q.Line || (p.Line == q.Line && p.Character < q.Character)
}

// lineIndex maps between byte offsets, one-based scalar positions and LSP
// UTF-16 positions over original, validated UTF-8 bytes. Line terminators are
// LF, CRLF and lone CR, as in the LSP base specification.
type lineIndex struct {
	text   []byte
	starts []int // byte offset where each line starts
	ends   []int // byte offset where each line's content ends, before its terminator
	bom    int   // length of a leading UTF-8 BOM, hidden from model columns
}

func newLineIndex(text []byte) *lineIndex {
	ix := &lineIndex{text: text, starts: []int{0}}
	if len(text) >= 3 && text[0] == 0xEF && text[1] == 0xBB && text[2] == 0xBF {
		ix.bom = 3
	}
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\n':
			ix.ends = append(ix.ends, i)
			ix.starts = append(ix.starts, i+1)
		case '\r':
			ix.ends = append(ix.ends, i)
			if i+1 < len(text) && text[i+1] == '\n' {
				i++
			}
			ix.starts = append(ix.starts, i+1)
		}
	}
	ix.ends = append(ix.ends, len(text))
	return ix
}

func (ix *lineIndex) lines() int { return len(ix.starts) }

// contentStart is where model column 1 begins on line.
func (ix *lineIndex) contentStart(line int) int {
	if line == 0 {
		return ix.bom
	}
	return ix.starts[line]
}

// modelOffset converts a one-based scalar position to a byte offset. The end
// of a line is valid; anything beyond it is an error, never clamped.
func (ix *lineIndex) modelOffset(p Position) (int, error) {
	if p.Line < 1 || p.Column < 1 {
		return 0, newError(CodeInvalidPosition, "line and column are one-based")
	}
	if p.Line > ix.lines() {
		return 0, newError(CodeInvalidPosition, fmt.Sprintf("line %d is beyond the end of the file (%d lines)", p.Line, ix.lines()))
	}
	line := p.Line - 1
	offset, end := ix.contentStart(line), ix.ends[line]
	for column := 1; column < p.Column; column++ {
		if offset >= end {
			return 0, newError(CodeInvalidPosition, fmt.Sprintf("column %d is beyond the end of line %d (%d columns)", p.Column, p.Line, column))
		}
		_, size := utf8.DecodeRune(ix.text[offset:end])
		offset += size
	}
	return offset, nil
}

func (ix *lineIndex) lineOf(offset int) int {
	return sort.Search(len(ix.starts), func(i int) bool { return ix.starts[i] > offset }) - 1
}

// utf16 converts a byte offset on a rune boundary to an LSP position.
func (ix *lineIndex) utf16(offset int) lspPosition {
	line := ix.lineOf(offset)
	units := 0
	for i := ix.starts[line]; i < offset; {
		r, size := utf8.DecodeRune(ix.text[i:])
		units += utf16Len(r)
		i += size
	}
	return lspPosition{Line: line, Character: units}
}

// end is the LSP position of the end of the document.
func (ix *lineIndex) end() lspPosition { return ix.utf16(len(ix.text)) }

// offset converts a server position to a byte offset, rejecting positions
// beyond the line or between the halves of a surrogate pair.
func (ix *lineIndex) offset(p lspPosition) (int, error) {
	if !p.valid() || p.Line >= ix.lines() {
		return 0, newError(CodeProtocol, "server position is beyond the end of the document")
	}
	offset, end := ix.starts[p.Line], ix.ends[p.Line]
	for units := 0; units < p.Character; {
		if offset >= end {
			return 0, newError(CodeProtocol, "server position is beyond the end of its line")
		}
		r, size := utf8.DecodeRune(ix.text[offset:end])
		units += utf16Len(r)
		if units > p.Character {
			return 0, newError(CodeProtocol, "server position splits a surrogate pair")
		}
		offset += size
	}
	return offset, nil
}

// model converts a byte offset to a one-based scalar position. An offset
// inside the hidden BOM maps to column 1.
func (ix *lineIndex) model(offset int) Position {
	line := ix.lineOf(offset)
	start := ix.contentStart(line)
	column := 1
	for i := start; i < offset; {
		_, size := utf8.DecodeRune(ix.text[i:])
		i += size
		column++
	}
	return Position{Line: line + 1, Column: column}
}

// modelRange converts a server range to model positions, validating order.
func (ix *lineIndex) modelRange(r lspRange) (Position, Position, error) {
	start, err := ix.offset(r.Start)
	if err != nil {
		return Position{}, Position{}, err
	}
	end, err := ix.offset(r.End)
	if err != nil {
		return Position{}, Position{}, err
	}
	if end < start {
		return Position{}, Position{}, newError(CodeProtocol, "server range ends before it starts")
	}
	return ix.model(start), ix.model(end), nil
}

func utf16Len(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}
