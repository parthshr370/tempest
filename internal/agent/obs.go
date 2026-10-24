package agent

import (
	"strings"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/obs"
)

// errorLogRunes bounds an error message in a log line: enough to identify the
// failure without flooding the log with a provider's response body.
const errorLogRunes = 400

// redactTruncate delegates to the shared redactor (internal/obs), bounding the
// result to max runes for high-volume log fields (error messages, previews).
func redactTruncate(s string, max int) string { return obs.RedactTruncate(s, max) }

// logPreview renders a short, single-line, redacted excerpt of content blocks'
// text for debug logging. It joins text blocks, collapses whitespace, redacts
// secrets, and truncates to max runes with a marker.
func logPreview(blocks []types.ContentBlock, max int) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == types.BlockText && blk.Text != "" {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(blk.Text)
		}
	}
	return redactTruncate(b.String(), max)
}

// contentBytes sums the text length of content blocks, a cheap size metric for
// info-level logs that carries no content.
func contentBytes(blocks []types.ContentBlock) int {
	n := 0
	for _, blk := range blocks {
		n += len(blk.Text)
	}
	return n
}
