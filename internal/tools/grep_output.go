package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/hashline"
	"go.harness.dev/harness/internal/truncate"
)

// grepSource tells formatGrepOutput how to show and record the files that
// matched.
type grepSource struct {
	// display is the path shown in a file's [display#TAG] header.
	display func(filePath string) string
	// read returns the full file, for its tag and line text.
	read func(filePath string) ([]byte, error)
	// canonical is the snapshot-store key; "" leaves the file unrecorded.
	canonical func(filePath string) string
}

// grepRowMeta ties an output row to the file line it displays in full, so
// only rows that survive truncation count as displayed under the tag.
type grepRowMeta struct {
	file int
	line int
}

// grepFile is one matched file's content.
type grepFile struct {
	path  string
	text  string
	lines []string
	tag   string
	hits  []int
	rg    map[int]string
}

// formatGrepOutput groups matches per file under [path#TAG] headers with
// LINE:TEXT rows (with context, matches are marked `*` and context rows
// ` `), and records each file's displayed rows in store.
func formatGrepOutput(matches []grepMatch, src grepSource, store *hashline.Store, contextLines int, limit int, matchLimitReached bool, backend string) agent.AgentToolResult {
	if len(matches) == 0 {
		return textResult("No matches found", nil)
	}
	var files []*grepFile
	byPath := map[string]*grepFile{}
	for _, match := range matches {
		file := byPath[match.filePath]
		if file == nil {
			file = &grepFile{path: match.filePath, rg: map[int]string{}}
			byPath[match.filePath] = file
			files = append(files, file)
		}
		if _, seen := file.rg[match.lineNumber]; !seen {
			file.hits = append(file.hits, match.lineNumber)
		}
		file.rg[match.lineNumber] = match.lineText
	}
	var out []string
	var meta []grepRowMeta
	linesTruncated := false
	row := func(fileIndex, line int, text string) {
		cut := truncate.Line(text, 0)
		if cut.WasTruncated {
			linesTruncated = true
			line = 0 // shown partially: not displayed in full
		}
		out = append(out, cut.Text)
		meta = append(meta, grepRowMeta{file: fileIndex, line: line})
	}
	for index, file := range files {
		slices.Sort(file.hits)
		if data, err := src.read(file.path); err == nil && !containsNUL(data) {
			file.text = hashline.Decode(data).Text
			file.lines = hashline.Lines(file.text)
			file.tag = hashline.TagText(file.text)
		}
		if index > 0 {
			out = append(out, "")
			meta = append(meta, grepRowMeta{file: index})
		}
		header := "[" + src.display(file.path) + "]"
		if file.tag != "" {
			header = "[" + src.display(file.path) + "#" + file.tag + "]"
		}
		out = append(out, header)
		meta = append(meta, grepRowMeta{file: index})
		if file.lines == nil || contextLines == 0 {
			for _, hit := range file.hits {
				if hit <= len(file.lines) {
					row(index, hit, fmt.Sprintf("%d:%s", hit, file.lines[hit-1]))
					continue
				}
				// The file changed or could not be read: show the line as
				// the search saw it, never as displayed under a tag.
				text := strings.TrimRight(strings.ReplaceAll(file.rg[hit], "\r", ""), "\n")
				row(index, 0, fmt.Sprintf("%d:%s", hit, text))
			}
			continue
		}
		last := 0
		for _, hit := range file.hits {
			if hit > len(file.lines) {
				continue
			}
			start := max(max(1, hit-contextLines), last+1)
			end := min(len(file.lines), hit+contextLines)
			if last > 0 && start > last+1 {
				out = append(out, "...")
				meta = append(meta, grepRowMeta{file: index})
			}
			for line := start; line <= end; line++ {
				marker := " "
				if _, isHit := file.rg[line]; isHit {
					marker = "*"
				}
				row(index, line, fmt.Sprintf("%s%d:%s", marker, line, file.lines[line-1]))
			}
			last = max(last, end)
		}
	}
	truncation := truncate.Head(strings.Join(out, "\n"), truncate.Options{MaxLines: truncate.Int(int(^uint(0) >> 1))})
	if store != nil {
		shown := truncation.OutputLines
		if truncation.LastLinePartial {
			shown--
		}
		displayed := make([][]int, len(files))
		for i := range min(max(shown, 0), len(meta)) {
			if meta[i].line > 0 {
				displayed[meta[i].file] = append(displayed[meta[i].file], meta[i].line)
			}
		}
		for index, file := range files {
			if file.tag == "" {
				continue
			}
			if key := src.canonical(file.path); key != "" {
				store.Observe(key, file.text, displayed[index])
			}
		}
	}

	output := truncation.Content
	details := grepDetails{Backend: backend}
	notices := []string{}
	if matchLimitReached {
		notices = append(notices, fmt.Sprintf("%d matches limit reached. Use limit=%d for more, or refine pattern", limit, limit*2))
		details.MatchLimitReached = limit
	}
	if truncation.Truncated {
		notices = append(notices, truncate.FormatSize(truncate.DefaultMaxBytes)+" limit reached")
		details.Truncation = &truncation
	}
	if linesTruncated {
		notices = append(notices, fmt.Sprintf("Some lines truncated to %d chars. Use read tool to see full lines", truncate.GrepMaxLineLength))
		details.LinesTruncated = true
	}
	if len(notices) > 0 {
		output += "\n\n[" + strings.Join(notices, ". ") + "]"
	}
	return textResult(output, details)
}

// grepDetails is grep's structured result, keyed as before the per-file
// grouping so clients keep reading the same fields.
type grepDetails struct {
	MatchLimitReached int              `json:"matchLimitReached,omitempty"`
	Truncation        *truncate.Result `json:"truncation,omitempty"`
	LinesTruncated    bool             `json:"linesTruncated,omitempty"`
	Backend           string           `json:"backend"`
}

// workspaceGrepSource shows workspace matches by their cwd-relative path, the
// form hashline sections accept, and records them under canonical paths.
func workspaceGrepSource(cwd string, root *os.Root) grepSource {
	return grepSource{
		display: func(filePath string) string {
			if rel, err := relInside(cwd, filePath); err == nil {
				return filepath.ToSlash(rel)
			}
			return filepath.ToSlash(filePath)
		},
		read: func(filePath string) ([]byte, error) {
			rel, err := relInside(cwd, filePath)
			if err != nil {
				return nil, err
			}
			return readFileInRoot(root, filepath.ToSlash(rel))
		},
		canonical: func(filePath string) string {
			if _, canonical, err := rootRelPaths(cwd, filePath); err == nil {
				return canonical
			}
			return ""
		},
	}
}
