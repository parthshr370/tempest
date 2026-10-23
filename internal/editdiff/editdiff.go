// Package editdiff applies find-and-replace edits to file content with
// fuzzy matching and renders unified and line-numbered diffs.
// Package editdiff implements file editing: line-ending normalization, Unicode
// NFKC fuzzy matching, multi-edit application with overlap detection, unified
// diff generation, and BOM stripping. The apply function rejects
// empty/duplicate/overlap/not-found/noop edits with verbatim error messages.
package editdiff

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/pmezard/go-difflib/difflib"
	"golang.org/x/text/unicode/norm"
)

// LineEnding identifies the newline convention of a piece of text.
type LineEnding string

const (
	// LF is the Unix line ending.
	LF LineEnding = "\n"
	// CRLF is the Windows line ending.
	CRLF LineEnding = "\r\n"
	// BOM is the UTF-8 byte-order mark.
	BOM = "\ufeff"
)

// AnchorLength is the number of hexadecimal characters in a SHA-256 content
// anchor.
const AnchorLength = sha256.Size * 2

// ContentAnchor returns the full SHA-256 hex digest of content.
func ContentAnchor(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// ErrStaleAnchor classifies edits whose file content changed after it was read.
var ErrStaleAnchor = errors.New("stale content anchor")

// StaleAnchorError reports that a file changed after its content anchor was read.
type StaleAnchorError struct {
	// Path is the path the caller attempted to edit.
	Path string
	// Expected is the anchor supplied by the caller.
	Expected string
	// Actual is the anchor calculated from the current file contents.
	Actual string
}

// Error implements error.
func (e *StaleAnchorError) Error() string {
	return fmt.Sprintf("stale anchor, re-read %s (expected %s, current %s)", e.Path, e.Expected, e.Actual)
}

// Is reports whether target classifies stale content anchors.
func (e *StaleAnchorError) Is(target error) bool {
	return target == ErrStaleAnchor
}

// Edit is a single find-and-replace, optionally guarded by an anchor read from
// the file before it was changed.
type Edit struct {
	// OldText is the text to locate in the content.
	OldText string `json:"old_text"`
	// NewText is the replacement text.
	NewText string `json:"new_text"`
	// Anchor is the optional full 64-character SHA-256 hexadecimal digest emitted by the read tool.
	Anchor string `json:"anchor,omitempty"`
}

// AppliedEditsResult holds the content before and after edits were applied.
type AppliedEditsResult struct {
	// BaseContent is the normalized content before edits.
	BaseContent string
	// NewContent is the content after edits.
	NewContent string
}

// FuzzyMatchResult reports where a search text was found and whether fuzzy
// (normalized) matching was needed.
type FuzzyMatchResult struct {
	// Found reports whether the search text was located.
	Found bool
	// Index is the byte offset of the match, or -1 when not found.
	Index int
	// MatchLength is the byte length of the matched region.
	MatchLength int
	// UsedFuzzyMatch reports whether a normalized (non-exact) match was used.
	UsedFuzzyMatch bool
	// ContentForReplacement is the content the Index refers into (normalized when fuzzy).
	ContentForReplacement string
}

// DiffStringResult is a rendered line-numbered diff and the first changed line.
type DiffStringResult struct {
	// Diff is the rendered diff text.
	Diff string
	// FirstChangedLine is the 1-based new-file line of the first change, or nil.
	FirstChangedLine *int
}

// textReplacement keeps byte offsets in the original replacement base so
// reverse-order splicing doesn't invalidate an earlier match.
type textReplacement struct {
	matchIndex  int
	matchLength int
	newText     string
}

// matchedEdit retains the caller's index after matches are sorted, so errors
// still identify the edit the caller supplied.
type matchedEdit struct {
	editIndex int
	textReplacement
}

// lineSpan maps normalized-content byte offsets back to whole lines when we
// preserve original formatting outside a fuzzy replacement.
type lineSpan struct {
	start int
	end   int
}

// DetectLineEnding returns CRLF if the first newline in content is \r\n,
// otherwise LF.
func DetectLineEnding(content string) LineEnding {
	crlfIdx := strings.Index(content, "\r\n")
	lfIdx := strings.Index(content, "\n")
	if lfIdx == -1 || crlfIdx == -1 {
		return LF
	}
	if crlfIdx < lfIdx {
		return CRLF
	}
	return LF
}

// NormalizeToLF converts all CRLF and lone CR line endings in text to LF.
func NormalizeToLF(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
}

// RestoreLineEndings converts LF line endings in text back to ending.
func RestoreLineEndings(text string, ending LineEnding) string {
	if ending == CRLF {
		return strings.ReplaceAll(text, "\n", "\r\n")
	}
	return text
}

// NormalizeForFuzzyMatch canonicalizes text for tolerant matching: it applies
// NFKC normalization, trims trailing whitespace per line, and folds smart
// quotes, dashes, and exotic spaces to their ASCII equivalents.
func NormalizeForFuzzyMatch(text string) string {
	text = norm.NFKC.String(text)
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimRightFunc(lines[i], unicode.IsSpace)
	}
	text = strings.Join(lines, "\n")
	replacer := strings.NewReplacer(
		"\u2018", "'", "\u2019", "'", "\u201a", "'", "\u201b", "'",
		"\u201c", "\"", "\u201d", "\"", "\u201e", "\"", "\u201f", "\"",
		"\u2010", "-", "\u2011", "-", "\u2012", "-", "\u2013", "-", "\u2014", "-", "\u2015", "-", "\u2212", "-",
		"\u00a0", " ", "\u2002", " ", "\u2003", " ", "\u2004", " ", "\u2005", " ", "\u2006", " ", "\u2007", " ", "\u2008", " ", "\u2009", " ", "\u200a", " ", "\u202f", " ", "\u205f", " ", "\u3000", " ",
	)
	return replacer.Replace(text)
}

// FuzzyFindText locates oldText in content, first exactly and then via
// normalized matching; when the fuzzy path is used, Index refers into the
// normalized content returned in ContentForReplacement.
func FuzzyFindText(content string, oldText string) FuzzyMatchResult {
	if idx := strings.Index(content, oldText); idx != -1 {
		return FuzzyMatchResult{Found: true, Index: idx, MatchLength: len(oldText), UsedFuzzyMatch: false, ContentForReplacement: content}
	}

	fuzzyContent := NormalizeForFuzzyMatch(content)
	fuzzyOldText := NormalizeForFuzzyMatch(oldText)
	idx := strings.Index(fuzzyContent, fuzzyOldText)
	if idx == -1 {
		return FuzzyMatchResult{Found: false, Index: -1, MatchLength: 0, UsedFuzzyMatch: false, ContentForReplacement: content}
	}
	return FuzzyMatchResult{Found: true, Index: idx, MatchLength: len(fuzzyOldText), UsedFuzzyMatch: true, ContentForReplacement: fuzzyContent}
}

// StripBOM splits a leading UTF-8 BOM off content, returning the BOM (or "")
// and the remaining text.
func StripBOM(content string) (bom string, text string) {
	if strings.HasPrefix(content, BOM) {
		return BOM, strings.TrimPrefix(content, BOM)
	}
	return "", content
}

// ApplyEditsToNormalizedContent applies edits to LF-normalized content,
// requiring each OldText to be present, non-empty, and unique, and rejecting
// overlapping or no-op edits. When any edit matches only fuzzily, unchanged
// lines are preserved verbatim. path is used only in error messages.
func ApplyEditsToNormalizedContent(normalizedContent string, edits []Edit, path string) (AppliedEditsResult, error) {
	normalizedEdits := make([]Edit, len(edits))
	for i, edit := range edits {
		normalizedEdits[i] = Edit{OldText: NormalizeToLF(edit.OldText), NewText: NormalizeToLF(edit.NewText)}
	}
	for i, edit := range normalizedEdits {
		if len(edit.OldText) == 0 {
			return AppliedEditsResult{}, getEmptyOldTextError(path, i, len(normalizedEdits))
		}
	}

	usedFuzzyMatch := false
	for _, edit := range normalizedEdits {
		if FuzzyFindText(normalizedContent, edit.OldText).UsedFuzzyMatch {
			usedFuzzyMatch = true
			break
		}
	}
	replacementBaseContent := normalizedContent
	if usedFuzzyMatch {
		replacementBaseContent = NormalizeForFuzzyMatch(normalizedContent)
	}

	matched := make([]matchedEdit, 0, len(normalizedEdits))
	for i, edit := range normalizedEdits {
		match := FuzzyFindText(replacementBaseContent, edit.OldText)
		if !match.Found {
			return AppliedEditsResult{}, getNotFoundError(path, i, len(normalizedEdits))
		}
		needle := edit.OldText
		if usedFuzzyMatch {
			needle = NormalizeForFuzzyMatch(needle)
		}
		occurrences := countOccurrences(replacementBaseContent, needle)
		if occurrences > 1 {
			return AppliedEditsResult{}, getDuplicateError(path, i, len(normalizedEdits), occurrences)
		}
		matched = append(matched, matchedEdit{editIndex: i, textReplacement: textReplacement{matchIndex: match.Index, matchLength: match.MatchLength, newText: edit.NewText}})
	}

	sortMatchedEdits(matched)
	for i := 1; i < len(matched); i++ {
		prev := matched[i-1]
		cur := matched[i]
		if prev.matchIndex+prev.matchLength > cur.matchIndex {
			return AppliedEditsResult{}, fmt.Errorf("edits[%d] and edits[%d] overlap in %s. Merge them into one edit or target disjoint regions.", prev.editIndex, cur.editIndex, path)
		}
	}

	baseContent := normalizedContent
	var newContent string
	if usedFuzzyMatch {
		replacements := make([]textReplacement, len(matched))
		for i, edit := range matched {
			replacements[i] = edit.textReplacement
		}
		var err error
		newContent, err = ApplyReplacementsPreservingUnchangedLines(normalizedContent, replacementBaseContent, replacements)
		if err != nil {
			return AppliedEditsResult{}, err
		}
	} else {
		replacements := make([]textReplacement, len(matched))
		for i, edit := range matched {
			replacements[i] = edit.textReplacement
		}
		newContent = applyReplacements(replacementBaseContent, replacements, 0)
	}

	if baseContent == newContent {
		return AppliedEditsResult{}, getNoChangeError(path, len(normalizedEdits))
	}
	return AppliedEditsResult{BaseContent: baseContent, NewContent: newContent}, nil
}

// ApplyReplacementsPreservingUnchangedLines applies replacements (whose offsets
// refer into baseContent, the normalized form) while re-emitting untouched lines
// from originalContent verbatim, so only lines actually spanned by a replacement
// lose their original formatting. It requires the two inputs to share a line count.
func ApplyReplacementsPreservingUnchangedLines(originalContent string, baseContent string, replacements []textReplacement) (string, error) {
	originalLines := splitLinesWithEndings(originalContent)
	baseLines := getLineSpans(baseContent)
	if len(originalLines) != len(baseLines) {
		return "", fmt.Errorf("Cannot preserve unchanged lines because the base content has a different line count.")
	}

	sorted := append([]textReplacement(nil), replacements...)
	sortReplacements(sorted)
	type group struct {
		startLine    int
		endLine      int
		replacements []textReplacement
	}
	groups := []group{}
	for _, replacement := range sorted {
		startLine, endLine, err := getReplacementLineRange(baseLines, replacement)
		if err != nil {
			return "", err
		}
		if len(groups) > 0 && startLine < groups[len(groups)-1].endLine {
			cur := &groups[len(groups)-1]
			if endLine > cur.endLine {
				cur.endLine = endLine
			}
			cur.replacements = append(cur.replacements, replacement)
			continue
		}
		groups = append(groups, group{startLine: startLine, endLine: endLine, replacements: []textReplacement{replacement}})
	}

	originalLineIndex := 0
	var result strings.Builder
	for _, group := range groups {
		result.WriteString(strings.Join(originalLines[originalLineIndex:group.startLine], ""))
		groupStartOffset := baseLines[group.startLine].start
		groupEndOffset := baseLines[group.endLine-1].end
		result.WriteString(applyReplacements(baseContent[groupStartOffset:groupEndOffset], group.replacements, groupStartOffset))
		originalLineIndex = group.endLine
	}
	result.WriteString(strings.Join(originalLines[originalLineIndex:], ""))
	return result.String(), nil
}

// GenerateUnifiedPatch returns a standard unified diff between oldContent and
// newContent, defaulting to 4 context lines when contextLines is 0.
func GenerateUnifiedPatch(path string, oldContent string, newContent string, contextLines int) string {
	if contextLines == 0 {
		contextLines = 4
	}
	patch, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(oldContent),
		B:        difflib.SplitLines(newContent),
		FromFile: path,
		ToFile:   path,
		Context:  contextLines,
	})
	return patch
}

// GenerateDiffString renders a compact, line-numbered diff of oldContent versus
// newContent, collapsing runs of unchanged lines beyond contextLines (default 4)
// into "..." markers and reporting the first changed line.
func GenerateDiffString(oldContent string, newContent string, contextLines int) DiffStringResult {
	if contextLines == 0 {
		contextLines = 4
	}
	parts := diffLines(oldContent, newContent)
	output := []string{}
	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")
	lineNumWidth := len(fmt.Sprintf("%d", max(len(oldLines), len(newLines))))
	oldLineNum := 1
	newLineNum := 1
	lastWasChange := false
	var firstChangedLine *int

	for i, part := range parts {
		raw := append([]string(nil), part.lines...)
		if len(raw) > 0 && raw[len(raw)-1] == "" {
			raw = raw[:len(raw)-1]
		}
		if part.added || part.removed {
			if firstChangedLine == nil {
				v := newLineNum
				firstChangedLine = &v
			}
			for _, line := range raw {
				if part.added {
					output = append(output, fmt.Sprintf("+%*d %s", lineNumWidth, newLineNum, line))
					newLineNum++
				} else {
					output = append(output, fmt.Sprintf("-%*d %s", lineNumWidth, oldLineNum, line))
					oldLineNum++
				}
			}
			lastWasChange = true
			continue
		}

		nextPartIsChange := i < len(parts)-1 && (parts[i+1].added || parts[i+1].removed)
		hasLeadingChange := lastWasChange
		hasTrailingChange := nextPartIsChange
		if hasLeadingChange && hasTrailingChange {
			if len(raw) <= contextLines*2 {
				for _, line := range raw {
					output = append(output, fmt.Sprintf(" %*d %s", lineNumWidth, oldLineNum, line))
					oldLineNum++
					newLineNum++
				}
			} else {
				leading := raw[:contextLines]
				trailing := raw[len(raw)-contextLines:]
				skipped := len(raw) - len(leading) - len(trailing)
				for _, line := range leading {
					output = append(output, fmt.Sprintf(" %*d %s", lineNumWidth, oldLineNum, line))
					oldLineNum++
					newLineNum++
				}
				output = append(output, fmt.Sprintf(" %*s ...", lineNumWidth, ""))
				oldLineNum += skipped
				newLineNum += skipped
				for _, line := range trailing {
					output = append(output, fmt.Sprintf(" %*d %s", lineNumWidth, oldLineNum, line))
					oldLineNum++
					newLineNum++
				}
			}
		} else if hasLeadingChange {
			shown := raw
			if len(shown) > contextLines {
				shown = raw[:contextLines]
			}
			skipped := len(raw) - len(shown)
			for _, line := range shown {
				output = append(output, fmt.Sprintf(" %*d %s", lineNumWidth, oldLineNum, line))
				oldLineNum++
				newLineNum++
			}
			if skipped > 0 {
				output = append(output, fmt.Sprintf(" %*s ...", lineNumWidth, ""))
				oldLineNum += skipped
				newLineNum += skipped
			}
		} else if hasTrailingChange {
			skipped := max(0, len(raw)-contextLines)
			if skipped > 0 {
				output = append(output, fmt.Sprintf(" %*s ...", lineNumWidth, ""))
				oldLineNum += skipped
				newLineNum += skipped
			}
			for _, line := range raw[skipped:] {
				output = append(output, fmt.Sprintf(" %*d %s", lineNumWidth, oldLineNum, line))
				oldLineNum++
				newLineNum++
			}
		} else {
			oldLineNum += len(raw)
			newLineNum += len(raw)
		}
		lastWasChange = false
	}

	return DiffStringResult{Diff: strings.Join(output, "\n"), FirstChangedLine: firstChangedLine}
}

// splitLinesWithEndings splits content into lines that retain their trailing
// newline, so joining them reproduces the original exactly.
func splitLinesWithEndings(content string) []string {
	if content == "" {
		return []string{}
	}
	parts := strings.SplitAfter(content, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// getLineSpans returns the byte [start,end) span of each line in content.
func getLineSpans(content string) []lineSpan {
	offset := 0
	lines := splitLinesWithEndings(content)
	spans := make([]lineSpan, len(lines))
	for i, line := range lines {
		spans[i] = lineSpan{start: offset, end: offset + len(line)}
		offset = spans[i].end
	}
	return spans
}

// getReplacementLineRange returns the [startLine, endLine) line indices that a
// replacement's byte range spans within lines.
func getReplacementLineRange(lines []lineSpan, replacement textReplacement) (int, int, error) {
	replacementStart := replacement.matchIndex
	replacementEnd := replacement.matchIndex + replacement.matchLength
	startLine := -1
	for i, line := range lines {
		if replacementStart >= line.start && replacementStart < line.end {
			startLine = i
			break
		}
	}
	if startLine == -1 {
		return 0, 0, fmt.Errorf("Replacement range is outside the base content.")
	}
	endLine := startLine
	for endLine < len(lines) && lines[endLine].end < replacementEnd {
		endLine++
	}
	if endLine >= len(lines) {
		return 0, 0, fmt.Errorf("Replacement range is outside the base content.")
	}
	return startLine, endLine + 1, nil
}

// applyReplacements splices replacements into content, applying them from last
// to first so earlier match indices stay valid; offset is subtracted from each
// match index to translate into content-local coordinates.
func applyReplacements(content string, replacements []textReplacement, offset int) string {
	result := content
	for i := len(replacements) - 1; i >= 0; i-- {
		replacement := replacements[i]
		matchIndex := replacement.matchIndex - offset
		result = result[:matchIndex] + replacement.newText + result[matchIndex+replacement.matchLength:]
	}
	return result
}

// countOccurrences counts fuzzy-normalized occurrences of oldText in content.
func countOccurrences(content string, oldText string) int {
	fuzzyContent := NormalizeForFuzzyMatch(content)
	fuzzyOldText := NormalizeForFuzzyMatch(oldText)
	return strings.Count(fuzzyContent, fuzzyOldText)
}

func getNotFoundError(path string, editIndex int, totalEdits int) error {
	if totalEdits == 1 {
		return fmt.Errorf("Could not find the exact text in %s. The old text must match exactly including all whitespace and newlines.", path)
	}
	return fmt.Errorf("Could not find edits[%d] in %s. The old_text must match exactly including all whitespace and newlines.", editIndex, path)
}

func getDuplicateError(path string, editIndex int, totalEdits int, occurrences int) error {
	if totalEdits == 1 {
		return fmt.Errorf("Found %d occurrences of the text in %s. The text must be unique. Please provide more context to make it unique.", occurrences, path)
	}
	return fmt.Errorf("Found %d occurrences of edits[%d] in %s. Each old_text must be unique. Please provide more context to make it unique.", occurrences, editIndex, path)
}

func getEmptyOldTextError(path string, editIndex int, totalEdits int) error {
	if totalEdits == 1 {
		return fmt.Errorf("old_text must not be empty in %s.", path)
	}
	return fmt.Errorf("edits[%d].old_text must not be empty in %s.", editIndex, path)
}

func getNoChangeError(path string, totalEdits int) error {
	if totalEdits == 1 {
		return fmt.Errorf("No changes made to %s. The replacement produced identical content. This might indicate an issue with special characters or the text not existing as expected.", path)
	}
	return fmt.Errorf("No changes made to %s. The replacements produced identical content.", path)
}

// sortMatchedEdits stably insertion-sorts edits by ascending match index.
func sortMatchedEdits(in []matchedEdit) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j-1].matchIndex > in[j].matchIndex; j-- {
			in[j-1], in[j] = in[j], in[j-1]
		}
	}
}

// sortReplacements stably insertion-sorts replacements by ascending match index.
func sortReplacements(in []textReplacement) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j-1].matchIndex > in[j].matchIndex; j-- {
			in[j-1], in[j] = in[j], in[j-1]
		}
	}
}

// diffPart groups adjacent LCS output with the same change kind, letting the
// renderer collapse context without rebuilding the diff.
type diffPart struct {
	lines   []string
	added   bool
	removed bool
}

// diffLines computes a line-level diff of oldContent versus newContent via a
// longest-common-subsequence table, returning contiguous added/removed/unchanged parts.
func diffLines(oldContent, newContent string) []diffPart {
	a := strings.Split(oldContent, "\n")
	b := strings.Split(newContent, "\n")
	m, n := len(a), len(b)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	parts := []diffPart{}
	appendPart := func(line string, added, removed bool) {
		if len(parts) > 0 && parts[len(parts)-1].added == added && parts[len(parts)-1].removed == removed {
			parts[len(parts)-1].lines = append(parts[len(parts)-1].lines, line)
			return
		}
		parts = append(parts, diffPart{lines: []string{line}, added: added, removed: removed})
	}
	i, j := 0, 0
	for i < m && j < n {
		if a[i] == b[j] {
			appendPart(a[i], false, false)
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			appendPart(a[i], false, true)
			i++
		} else {
			appendPart(b[j], true, false)
			j++
		}
	}
	for i < m {
		appendPart(a[i], false, true)
		i++
	}
	for j < n {
		appendPart(b[j], true, false)
		j++
	}
	return parts
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
