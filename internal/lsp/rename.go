package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go.harness.dev/harness/internal/editdiff"
)

const (
	maxRenameFiles     = 64
	maxRenameEdits     = 2000
	maxRenameText      = 1 << 20
	maxRenamePlanBytes = 128 << 10
	maxSnapshotFiles   = 256
	maxSnapshotBytes   = 16 << 20
	renameDiffContext  = 3
)

type planFile struct {
	Path   string          `json:"path"`
	Anchor string          `json:"anchor"`
	Edits  []editdiff.Edit `json:"edits"`
	Diff   string          `json:"diff"`
}

type renamePlan struct {
	Server    string     `json:"server"`
	Applied   bool       `json:"applied"`
	NoChanges bool       `json:"no_changes,omitempty"`
	Files     []planFile `json:"files"`
	FileCount int        `json:"file_count"`
	EditCount int        `json:"edit_count"`
}

type textEdit struct {
	Range        *lspRange `json:"range"`
	NewText      *string   `json:"newText"`
	AnnotationID *string   `json:"annotationId"`
}

// renameTarget is one file's edits from a WorkspaceEdit.
type renameTarget struct {
	path    string
	version *int64 // non-nil when the server named a document version
	edits   []textEdit
}

func rejectRename(format string, args ...any) *Error {
	return newError(CodeRenameRejected, fmt.Sprintf(format, args...))
}

// parseWorkspaceEdit accepts either a changes map or ordered TextDocumentEdit
// documentChanges, never both, never resource operations, confirmations,
// duplicate targets or uncontained URIs.
func parseWorkspaceEdit(root string, raw json.RawMessage) ([]renameTarget, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil, newError(CodeProtocol, "rename result is not a WorkspaceEdit object")
	}
	present := func(name string) bool {
		value, ok := fields[name]
		return ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null"))
	}
	if present("changes") && present("documentChanges") {
		return nil, rejectRename("the server sent both changes and documentChanges")
	}
	if present("changeAnnotations") {
		var annotations map[string]struct {
			NeedsConfirmation bool `json:"needsConfirmation"`
		}
		if json.Unmarshal(fields["changeAnnotations"], &annotations) != nil {
			return nil, newError(CodeProtocol, "malformed changeAnnotations")
		}
		for _, annotation := range annotations {
			if annotation.NeedsConfirmation {
				return nil, rejectRename("the server requires confirmation for part of the rename")
			}
		}
	}
	var targets []renameTarget
	seen := map[string]bool{}
	edits := 0
	addTarget := func(uri string, version *int64, rawEdits []textEdit) error {
		rel, ok := uriPath(root, uri)
		if !ok {
			return rejectRename("the rename touches a non-file or outside-workspace URI")
		}
		if seen[rel] {
			return rejectRename("the rename names %s more than once", rel)
		}
		seen[rel] = true
		edits += len(rawEdits)
		if len(seen) > maxRenameFiles || edits > maxRenameEdits {
			return newError(CodeOutputLimit, fmt.Sprintf("the rename exceeds %d files or %d edits; no partial plan is returned", maxRenameFiles, maxRenameEdits))
		}
		for _, edit := range rawEdits {
			if edit.Range == nil || edit.NewText == nil {
				return newError(CodeProtocol, "malformed TextEdit in rename result")
			}
		}
		targets = append(targets, renameTarget{path: rel, version: version, edits: rawEdits})
		return nil
	}
	switch {
	case present("changes"):
		var changes map[string][]textEdit
		if json.Unmarshal(fields["changes"], &changes) != nil {
			return nil, newError(CodeProtocol, "malformed WorkspaceEdit changes")
		}
		uris := make([]string, 0, len(changes))
		for uri := range changes {
			uris = append(uris, uri)
		}
		sort.Strings(uris)
		for _, uri := range uris {
			if err := addTarget(uri, nil, changes[uri]); err != nil {
				return nil, err
			}
		}
	case present("documentChanges"):
		var changes []json.RawMessage
		if json.Unmarshal(fields["documentChanges"], &changes) != nil {
			return nil, newError(CodeProtocol, "malformed WorkspaceEdit documentChanges")
		}
		for _, change := range changes {
			var item struct {
				Kind         *string `json:"kind"`
				TextDocument *struct {
					URI     string `json:"uri"`
					Version *int64 `json:"version"`
				} `json:"textDocument"`
				Edits []textEdit `json:"edits"`
			}
			if json.Unmarshal(change, &item) != nil {
				return nil, newError(CodeProtocol, "malformed documentChanges entry")
			}
			if item.Kind != nil {
				return nil, rejectRename("the rename includes a %s resource operation, which Tempest doesn't perform", boundText(*item.Kind, maxEchoedName))
			}
			if item.TextDocument == nil || item.Edits == nil {
				return nil, newError(CodeProtocol, "unsupported documentChanges entry")
			}
			if err := addTarget(item.TextDocument.URI, item.TextDocument.Version, item.Edits); err != nil {
				return nil, err
			}
		}
	}
	return targets, nil
}

// byteEdit is a validated edit in original byte offsets.
type byteEdit struct {
	start, end int
	text       string
}

// applyServerEdits validates ranges against the original snapshot and applies
// them in descending order: overlaps and ambiguous same-offset insertions fail.
func applyServerEdits(rel string, text []byte, index *lineIndex, edits []textEdit) ([]byte, int, error) {
	converted := make([]byteEdit, 0, len(edits))
	replacement := 0
	for _, edit := range edits {
		start, err := index.offset(edit.Range.Start)
		if err != nil {
			return nil, 0, rejectRename("invalid range in %s: %s", rel, asError(err).Detail)
		}
		end, err := index.offset(edit.Range.End)
		if err != nil {
			return nil, 0, rejectRename("invalid range in %s: %s", rel, asError(err).Detail)
		}
		if end < start {
			return nil, 0, rejectRename("reversed range in %s", rel)
		}
		if start < index.bom {
			return nil, 0, rejectRename("an edit in %s touches the byte-order mark", rel)
		}
		if strings.ContainsRune(*edit.NewText, 0) {
			return nil, 0, rejectRename("replacement text for %s contains NUL", rel)
		}
		replacement += len(*edit.NewText)
		converted = append(converted, byteEdit{start: start, end: end, text: *edit.NewText})
	}
	sort.SliceStable(converted, func(i, j int) bool { return converted[i].start < converted[j].start })
	for i := 1; i < len(converted); i++ {
		previous, current := converted[i-1], converted[i]
		if previous.end > current.start {
			return nil, 0, rejectRename("overlapping edits in %s", rel)
		}
		if previous.start == current.start && (previous.start == previous.end || current.start == current.end) {
			return nil, 0, rejectRename("ambiguous insertions at the same position in %s", rel)
		}
	}
	result := append([]byte(nil), text...)
	for i := len(converted) - 1; i >= 0; i-- {
		edit := converted[i]
		result = append(result[:edit.start], append([]byte(edit.text), result[edit.end:]...)...)
	}
	return result, replacement, nil
}

// span is a region of the normalized original content.
type span struct{ start, end int }

// normalizedOffsets maps original byte offsets to the BOM-free LF content the
// edit tool matches against.
type normalizedOffsets struct {
	bom int
	crs []int // offsets of '\r' that begin a CRLF pair
}

func newNormalizedOffsets(text []byte, bom int) normalizedOffsets {
	offsets := normalizedOffsets{bom: bom}
	for i := bom; i+1 < len(text); i++ {
		if text[i] == '\r' && text[i+1] == '\n' {
			offsets.crs = append(offsets.crs, i)
		}
	}
	return offsets
}

func (n normalizedOffsets) at(offset int) int {
	pairs := sort.SearchInts(n.crs, offset) // CRLF pairs that start before offset
	return offset - n.bom - pairs
}

// planEdits turns the server's intended content into exact, unique edit
// inputs for the existing edit tool, falling back to one whole-file
// replacement when line-context hunks can't reproduce it exactly.
func planEdits(rel string, original, updated []byte, index *lineIndex, edits []textEdit) ([]editdiff.Edit, string, string, error) {
	_, oldContent := editdiff.StripBOM(string(original))
	_, newContent := editdiff.StripBOM(string(updated))
	normOld := editdiff.NormalizeToLF(oldContent)
	normNew := editdiff.NormalizeToLF(newContent)
	if normOld == normNew {
		return nil, normOld, normNew, rejectRename("the rename changes only line endings in %s, which the edit tool normalizes", rel)
	}
	offsets := newNormalizedOffsets(original, index.bom)
	var regions []span
	for _, edit := range edits {
		start, _ := index.offset(edit.Range.Start)
		end, _ := index.offset(edit.Range.End)
		regions = append(regions, lineSpan(normOld, offsets.at(start), offsets.at(end)))
	}
	hunks := uniqueHunks(normOld, mergeSpans(regions))
	planned, ok := hunkEdits(normOld, hunks, edits, index, offsets)
	if !ok || !dryApplyMatches(rel, normOld, planned, normNew) {
		if normOld == "" {
			return nil, normOld, normNew, rejectRename("%s is empty; the edit tool can't insert into an empty file", rel)
		}
		planned = []editdiff.Edit{{OldText: normOld, NewText: normNew}}
		if !dryApplyMatches(rel, normOld, planned, normNew) {
			return nil, normOld, normNew, rejectRename("the edit tool can't reproduce the server's rename of %s", rel)
		}
	}
	return planned, normOld, normNew, nil
}

// lineSpan widens [start,end) to whole lines, including the final newline.
func lineSpan(content string, start, end int) span {
	lineStart := strings.LastIndexByte(content[:start], '\n') + 1
	lineEnd := len(content)
	if i := strings.IndexByte(content[end:], '\n'); i >= 0 {
		lineEnd = end + i + 1
	}
	return span{lineStart, lineEnd}
}

func mergeSpans(spans []span) []span {
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	merged := spans[:0]
	for _, s := range spans {
		if n := len(merged); n > 0 && s.start < merged[n-1].end {
			merged[n-1].end = max(merged[n-1].end, s.end)
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

// uniqueHunks widens each span by a line on both sides until its text is
// nonempty and unique under the edit tool's fuzzy matching.
func uniqueHunks(content string, spans []span) []span {
	fuzzyContent := editdiff.NormalizeForFuzzyMatch(content)
	for {
		widened := false
		for i, s := range spans {
			text := content[s.start:s.end]
			if text != "" && strings.Count(fuzzyContent, editdiff.NormalizeForFuzzyMatch(text)) == 1 {
				continue
			}
			if s.start == 0 && s.end == len(content) {
				continue
			}
			start := 0
			if s.start > 0 {
				start = strings.LastIndexByte(content[:s.start-1], '\n') + 1
			}
			end := len(content)
			if s.end < len(content) {
				if j := strings.IndexByte(content[s.end:], '\n'); j >= 0 {
					end = s.end + j + 1
				}
			}
			spans[i] = span{start, end}
			widened = true
		}
		if !widened {
			return spans
		}
		spans = mergeSpans(spans)
	}
}

// hunkEdits computes each hunk's replacement by applying the server's edits
// that fall inside it, in normalized coordinates.
func hunkEdits(normOld string, hunks []span, edits []textEdit, index *lineIndex, offsets normalizedOffsets) ([]editdiff.Edit, bool) {
	type normalizedEdit struct {
		start, end int
		text       string
	}
	var normalized []normalizedEdit
	for _, edit := range edits {
		start, _ := index.offset(edit.Range.Start)
		end, _ := index.offset(edit.Range.End)
		normalized = append(normalized, normalizedEdit{offsets.at(start), offsets.at(end), editdiff.NormalizeToLF(*edit.NewText)})
	}
	sort.SliceStable(normalized, func(i, j int) bool { return normalized[i].start < normalized[j].start })
	var planned []editdiff.Edit
	next := 0
	for h, hunk := range hunks {
		// An edit starting exactly at a hunk boundary belongs to the hunk that
		// starts there, if any.
		owns := func(edit normalizedEdit) bool {
			if edit.start < hunk.start {
				return false
			}
			if edit.start < hunk.end {
				return true
			}
			return edit.start == hunk.end && !(h+1 < len(hunks) && hunks[h+1].start == edit.start)
		}
		var builder strings.Builder
		cursor := hunk.start
		for next < len(normalized) && owns(normalized[next]) {
			edit := normalized[next]
			if edit.end > hunk.end || edit.start < cursor {
				return nil, false
			}
			builder.WriteString(normOld[cursor:edit.start])
			builder.WriteString(edit.text)
			cursor = edit.end
			next++
		}
		builder.WriteString(normOld[cursor:hunk.end])
		if oldText := normOld[hunk.start:hunk.end]; oldText != builder.String() {
			planned = append(planned, editdiff.Edit{OldText: oldText, NewText: builder.String()})
		}
	}
	return planned, next == len(normalized) && len(planned) > 0
}

// dryApplyMatches runs the edit tool's own matcher and compares the result.
func dryApplyMatches(rel, normOld string, edits []editdiff.Edit, normNew string) bool {
	applied, err := editdiff.ApplyEditsToNormalizedContent(normOld, edits, rel)
	return err == nil && applied.NewContent == normNew
}

// renameValue validates the server's WorkspaceEdit against the pre-request
// snapshot and open versions and returns a complete anchored plan. It never
// writes: the model applies each file with the ordinary edit tool.
func (s *server) renameValue(ctx context.Context, raw json.RawMessage, snapshot map[string]string, w *watch) (json.RawMessage, error) {
	targets, err := parseWorkspaceEdit(s.m.root, raw)
	if err != nil {
		return nil, err
	}
	plan := renamePlan{Server: s.route.name, Files: []planFile{}}
	replacement := 0
	for _, target := range targets {
		record := s.openDocument(target.path)
		expected, inSnapshot := snapshot[target.path]
		switch {
		case target.version != nil && (record == nil || int64(record.version) != *target.version):
			return nil, newError(CodeContentModified, fmt.Sprintf("the rename targets a different version of %s than Tempest synchronized", target.path))
		case record != nil:
			expected = record.anchor
		case !inSnapshot:
			return nil, rejectRename("%s was not in the pre-request workspace snapshot, so the bytes the server used are unknown", target.path)
		}
		doc, err := s.m.ws.ReadFile(ctx, target.path)
		if ctx.Err() != nil {
			return nil, asError(ctx.Err())
		}
		if err != nil || doc.Path != target.path {
			return nil, newError(CodeContentModified, target.path+" can't be read anymore; recompute the rename")
		}
		if doc.Anchor != expected || s.m.changedSince(w, target.path) {
			return nil, newError(CodeContentModified, target.path+" changed during the rename; recompute it")
		}
		if err = validateDocument(doc); err != nil {
			return nil, err
		}
		if len(target.edits) == 0 {
			continue
		}
		index := newLineIndex(doc.Text)
		updated, size, err := applyServerEdits(target.path, doc.Text, index, target.edits)
		if err != nil {
			return nil, err
		}
		if replacement += size; replacement > maxRenameText {
			return nil, newError(CodeOutputLimit, "the rename's replacement text exceeds 1 MiB; no partial plan is returned")
		}
		if bytes.Equal(updated, doc.Text) {
			continue
		}
		edits, normOld, normNew, err := planEdits(target.path, doc.Text, updated, index, target.edits)
		if err != nil {
			return nil, err
		}
		for i := range edits {
			edits[i].Anchor = doc.Anchor
		}
		plan.Files = append(plan.Files, planFile{Path: target.path, Anchor: doc.Anchor, Edits: edits, Diff: editdiff.GenerateUnifiedPatch(target.path, normOld, normNew, renameDiffContext)})
		plan.EditCount += len(edits)
	}
	sort.Slice(plan.Files, func(i, j int) bool { return plan.Files[i].Path < plan.Files[j].Path })
	plan.FileCount = len(plan.Files)
	plan.NoChanges = plan.FileCount == 0
	value, err := json.Marshal(plan)
	if err != nil {
		return nil, wrapError(CodeProtocol, "encode rename plan", err)
	}
	if cost := encodedCost(value); cost > maxRenamePlanBytes {
		return nil, newError(CodeOutputLimit, fmt.Sprintf("the rename plan for %d files and %d edits needs %d encoded bytes, over the 128 KiB limit; no partial plan is returned", plan.FileCount, plan.EditCount, cost))
	}
	return value, nil
}

// snapshotHashes captures potential unopened rename targets before dispatch.
func (m *Manager) snapshotHashes(ctx context.Context) (map[string]string, error) {
	docs, err := m.ws.Snapshot(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, asError(ctx.Err())
		}
		return nil, asError(err)
	}
	if len(docs) > maxSnapshotFiles {
		return nil, newError(CodeRenameScopeLimit, fmt.Sprintf("the workspace has more than %d source files; rename needs a bounded snapshot", maxSnapshotFiles))
	}
	hashes := make(map[string]string, len(docs))
	total := 0
	for _, doc := range docs {
		if total += len(doc.Text); total > maxSnapshotBytes {
			return nil, newError(CodeRenameScopeLimit, "the workspace's source files exceed 16 MiB; rename needs a bounded snapshot")
		}
		hashes[doc.Path] = doc.Anchor
	}
	return hashes, nil
}
