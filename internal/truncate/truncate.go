// Package truncate provides output truncation helpers for the coding tools:
// TruncateHead (cap by lines/bytes, never split a line), TruncateTail (UTF-8
// boundary walk for trailing bytes), TruncateLine (single-line cap), and
// FormatSize (human-readable byte counts).
package truncate

import (
	"fmt"
	"strings"
)

const (
	// DefaultMaxLines is the default line cap for truncation.
	DefaultMaxLines = 2000
	// DefaultMaxBytes is the default byte cap for truncation.
	DefaultMaxBytes = 50 * 1024
	// GrepMaxLineLength is the max characters kept for a single grep match line.
	GrepMaxLineLength = 500
	// TruncatedByLines marks truncation caused by the line limit.
	TruncatedByLines = "lines"
	// TruncatedByBytes marks truncation caused by the byte limit.
	TruncatedByBytes    = "bytes"
	truncatedByNone     = ""
	truncatedLineSuffix = "... [truncated]"
)

// Result holds the truncated content plus metrics about what was kept and
// what was dropped.
type Result struct {
	Content               string
	Truncated             bool
	TruncatedBy           string
	TotalLines            int
	TotalBytes            int
	OutputLines           int
	OutputBytes           int
	LastLinePartial       bool
	FirstLineExceedsLimit bool
	MaxLines              int
	MaxBytes              int
}

// Options sets line and byte caps for truncation. Nil fields fall back to
// DefaultMaxLines and DefaultMaxBytes respectively.
type Options struct {
	MaxLines *int
	MaxBytes *int
}

// LineResult holds a single-line truncation result.
type LineResult struct {
	Text         string
	WasTruncated bool
}

// FormatSize returns a human-readable byte count string (e.g. "1.5MB",
// "512B", "3.2KB", "2.0GB", "1.1TB").
func FormatSize(bytes int) string {
	if bytes < 1024 {
		return fmt.Sprintf("%dB", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	}
	if bytes < 1024*1024*1024 {
		return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
	}
	if bytes < 1024*1024*1024*1024 {
		return fmt.Sprintf("%.1fGB", float64(bytes)/(1024*1024*1024))
	}
	return fmt.Sprintf("%.1fTB", float64(bytes)/(1024*1024*1024))
}

// Head keeps the earliest complete lines that fit the configured line and byte caps.
func Head(content string, options Options) Result {
	maxLines, maxBytes := limits(options)
	totalBytes := len([]byte(content))
	lines := splitLinesForCounting(content)
	totalLines := len(lines)

	if totalLines <= maxLines && totalBytes <= maxBytes {
		return Result{Content: content, Truncated: false, TruncatedBy: truncatedByNone, TotalLines: totalLines, TotalBytes: totalBytes, OutputLines: totalLines, OutputBytes: totalBytes, MaxLines: maxLines, MaxBytes: maxBytes}
	}

	firstLineBytes := 0
	if len(lines) > 0 {
		firstLineBytes = len([]byte(lines[0]))
	}
	if firstLineBytes > maxBytes {
		return Result{Content: "", Truncated: true, TruncatedBy: TruncatedByBytes, TotalLines: totalLines, TotalBytes: totalBytes, OutputLines: 0, OutputBytes: 0, FirstLineExceedsLimit: true, MaxLines: maxLines, MaxBytes: maxBytes}
	}

	outputLines := make([]string, 0, min(len(lines), maxLines))
	outputBytes := 0
	truncatedBy := TruncatedByLines
	for i := 0; i < len(lines) && i < maxLines; i++ {
		lineBytes := len([]byte(lines[i]))
		if i > 0 {
			lineBytes++
		}
		if outputBytes+lineBytes > maxBytes {
			truncatedBy = TruncatedByBytes
			break
		}
		outputLines = append(outputLines, lines[i])
		outputBytes += lineBytes
	}
	if len(outputLines) >= maxLines && outputBytes <= maxBytes {
		truncatedBy = TruncatedByLines
	}

	output := strings.Join(outputLines, "\n")
	return Result{Content: output, Truncated: true, TruncatedBy: truncatedBy, TotalLines: totalLines, TotalBytes: totalBytes, OutputLines: len(outputLines), OutputBytes: len([]byte(output)), MaxLines: maxLines, MaxBytes: maxBytes}
}

// Tail keeps the latest complete lines that fit the configured line and byte caps.
func Tail(content string, options Options) Result {
	maxLines, maxBytes := limits(options)
	totalBytes := len([]byte(content))
	lines := splitLinesForCounting(content)
	totalLines := len(lines)

	if totalLines <= maxLines && totalBytes <= maxBytes {
		return Result{Content: content, Truncated: false, TruncatedBy: truncatedByNone, TotalLines: totalLines, TotalBytes: totalBytes, OutputLines: totalLines, OutputBytes: totalBytes, MaxLines: maxLines, MaxBytes: maxBytes}
	}

	outputLines := make([]string, 0, min(len(lines), maxLines))
	outputBytes := 0
	truncatedBy := TruncatedByLines
	lastLinePartial := false

	for i := len(lines) - 1; i >= 0 && len(outputLines) < maxLines; i-- {
		lineBytes := len([]byte(lines[i]))
		if len(outputLines) > 0 {
			lineBytes++
		}
		if outputBytes+lineBytes > maxBytes {
			truncatedBy = TruncatedByBytes
			if len(outputLines) == 0 {
				line := truncateStringToBytesFromEnd(lines[i], maxBytes)
				outputLines = append([]string{line}, outputLines...)
				outputBytes = len([]byte(line))
				lastLinePartial = true
			}
			break
		}
		outputLines = append([]string{lines[i]}, outputLines...)
		outputBytes += lineBytes
	}
	if len(outputLines) >= maxLines && outputBytes <= maxBytes {
		truncatedBy = TruncatedByLines
	}

	output := strings.Join(outputLines, "\n")
	return Result{Content: output, Truncated: true, TruncatedBy: truncatedBy, TotalLines: totalLines, TotalBytes: totalBytes, OutputLines: len(outputLines), OutputBytes: len([]byte(output)), LastLinePartial: lastLinePartial, MaxLines: maxLines, MaxBytes: maxBytes}
}

// Line shortens one line to maxChars characters, using [GrepMaxLineLength] when maxChars is non-positive.
func Line(line string, maxChars int) LineResult {
	if maxChars <= 0 {
		maxChars = GrepMaxLineLength
	}
	runes := []rune(line)
	if len(runes) <= maxChars {
		return LineResult{Text: line, WasTruncated: false}
	}
	return LineResult{Text: string(runes[:maxChars]) + truncatedLineSuffix, WasTruncated: true}
}

// splitLinesForCounting doesn't count the final empty segment from a trailing newline, matching normal line counting.
func splitLinesForCounting(content string) []string {
	if content == "" {
		return []string{}
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// truncateStringToBytesFromEnd advances past UTF-8 continuation bytes so a byte cap never starts mid-rune.
func truncateStringToBytesFromEnd(s string, maxBytes int) string {
	buf := []byte(s)
	if len(buf) <= maxBytes {
		return s
	}
	if maxBytes <= 0 {
		return ""
	}
	start := len(buf) - maxBytes
	for start < len(buf) && (buf[start]&0xc0) == 0x80 {
		start++
	}
	return string(buf[start:])
}

// limits centralizes nil-as-default handling so [Head] and [Tail] apply the same caps.
func limits(options Options) (int, int) {
	maxLines := DefaultMaxLines
	if options.MaxLines != nil {
		maxLines = *options.MaxLines
	}
	maxBytes := DefaultMaxBytes
	if options.MaxBytes != nil {
		maxBytes = *options.MaxBytes
	}
	return maxLines, maxBytes
}

// Int returns a pointer to v, for use with Options fields.
func Int(v int) *int { return &v }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
