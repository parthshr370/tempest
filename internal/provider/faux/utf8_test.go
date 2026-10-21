package faux

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"go.harness.dev/harness/internal/engine/types"
)

// Real providers stream whole runes; `tempest serve -faux-script` marshals each
// delta to JSON, which turns a split rune into U+FFFD on the wire.
func TestTextDeltasNeverSplitRunes(t *testing.T) {
	f := New(Options{})
	f.SetResponses(Respond(Text(strings.Repeat("ünïcode 字 🙂 ", 40))))
	s := f.Stream(context.Background(), f.Model(), types.Context{}, nil)
	for _, event := range collectEvents(s) {
		if event.Type == types.EvTextDelta && !utf8.ValidString(event.Delta) {
			t.Fatalf("text delta splits a rune: %q", event.Delta)
		}
	}
}
