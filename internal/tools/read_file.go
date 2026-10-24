package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/document"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hashline"
	"go.harness.dev/harness/internal/refs"
	"go.harness.dev/harness/internal/truncate"
)

// maxReadPaths bounds one batched read; the shared output budget bounds its size.
const maxReadPaths = 20

// fileReader reads workspace files, resources and session artifacts for the
// read tool, recording displayed rows in the hashline store.
type fileReader struct {
	cwd     string
	options ToolsOptions
	getRoot func() (*os.Root, error)
}

// readRequest is one file read. budget caps its numbered rows; batch marks a
// read sharing a paths call's budget, where continuations are path
// selectors because offset and limit do not apply.
type readRequest struct {
	path   string
	offset *int
	limit  *int
	mode   string
	budget readBudget
	batch  bool
}

// readBudget is the output a read may still return.
type readBudget struct {
	lines int
	bytes int
}

var defaultReadBudget = readBudget{lines: truncate.DefaultMaxLines, bytes: truncate.DefaultMaxBytes}

func (budget readBudget) options() truncate.Options {
	return truncate.Options{MaxLines: truncate.Int(budget.lines), MaxBytes: truncate.Int(budget.bytes)}
}

// readBatchFile is one paths entry's outcome in a batched read's details.
type readBatchFile struct {
	Path    string          `json:"path"`
	Error   string          `json:"error,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
}

// readBatchDetails lists every paths entry in request order.
type readBatchDetails struct {
	Files []readBatchFile `json:"files"`
}

// readBatch reads every path in order under one shared output budget. Each
// file is its own [path#TAG] section with its own displayed rows recorded; a
// file that fails reports its error in place, and a file the spent budget
// leaves no room for is named with how to read it.
func (reader fileReader) readBatch(ctx context.Context, paths []string, mode string) (agent.AgentToolResult, error) {
	if len(paths) > maxReadPaths {
		return agent.AgentToolResult{}, fmt.Errorf("paths has %d entries; read at most %d files per call", len(paths), maxReadPaths)
	}
	budget := defaultReadBudget
	var content []types.ContentBlock
	var text strings.Builder
	details := readBatchDetails{Files: make([]readBatchFile, 0, len(paths))}
	failures := 0
	imageBytes, maxImageBytes := 0, base64.StdEncoding.EncodedLen(document.MaxInlineImageBytes)
	for _, path := range paths {
		if text.Len() > 0 {
			text.WriteString("\n\n")
		}
		if budget.lines <= 1 || budget.bytes <= 1 {
			fmt.Fprintf(&text, "[%s: not read; this call's output budget (%d lines or %s) is spent. Read it in another call.]", path, truncate.DefaultMaxLines, truncate.FormatSize(truncate.DefaultMaxBytes))
			details.Files = append(details.Files, readBatchFile{Path: path, Error: "budget_spent"})
			continue
		}
		result, err := reader.read(ctx, readRequest{path: path, mode: mode, budget: budget, batch: true})
		if err != nil {
			if ctx.Err() != nil {
				return agent.AgentToolResult{}, errors.New("Operation aborted")
			}
			failures++
			fmt.Fprintf(&text, "[%s: %v]", path, err)
			details.Files = append(details.Files, readBatchFile{Path: path, Error: err.Error()})
			continue
		}
		details.Files = append(details.Files, readBatchFile{Path: path, Details: result.Details})
		for _, block := range result.Content {
			if block.Type == types.BlockText {
				budget.lines -= strings.Count(block.Text, "\n") + 1
				budget.bytes -= len(block.Text)
				text.WriteString(block.Text)
				continue
			}
			if block.Type == types.BlockImage {
				imageBytes += len(block.Data)
				if imageBytes > maxImageBytes {
					return agent.AgentToolResult{}, errors.New("read batch image output exceeds the inline image budget; read images in separate calls")
				}
			}
			if text.Len() > 0 {
				content = append(content, types.NewText(text.String()))
				text.Reset()
			}
			content = append(content, block)
		}
	}
	if failures == len(paths) {
		return agent.AgentToolResult{}, errors.New(text.String())
	}
	if text.Len() > 0 {
		content = append(content, types.NewText(text.String()))
	}
	result := textResult("", details)
	result.Content = content
	return result, nil
}

// read returns one file's [path#TAG] section: its numbered rows within the
// budget, an outline, or an image.
func (reader fileReader) read(ctx context.Context, request readRequest) (agent.AgentToolResult, error) {
	cwd, options, store := reader.cwd, reader.options, reader.options.Hashline
	if err := ctx.Err(); err != nil {
		return agent.AgentToolResult{}, errors.New("Operation aborted")
	}
	var rootForRead *os.Root
	if !refs.IsURL(request.path) {
		var err error
		rootForRead, err = reader.getRoot()
		if err != nil {
			return agent.AgentToolResult{}, err
		}
	}
	selector, err := parseReadPathSelector(request.path)
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	source, early, err := loadReadSource(ctx, cwd, rootForRead, options, selector.Path)
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	if early != nil {
		return *early, nil
	}
	data, absolutePath, displayPath, isResource := source.data, source.absolutePath, source.displayPath, source.isResource
	header := fmt.Sprintf("[%s#%s]", displayPath, hashline.Tag(data))
	// observe records the rows shown in full for workspace files, the
	// ones hashline can edit; resources and session artifacts are not.
	observe := func(displayed []int) {}
	if store != nil && !isResource && absolutePath != "" {
		if _, canonical, err := rootRelPaths(cwd, absolutePath); err == nil {
			observe = func(displayed []int) { store.Observe(canonical, hashline.Decode(data).Text, displayed) }
		}
	}
	if containsNUL(data) {
		return textResult(fmt.Sprintf("%s\n[binary file, %d bytes, not shown]", header, len(data)), &readToolDetails{Binary: true}), nil
	}
	wholeFile := !selector.Set && request.offset == nil && request.limit == nil
	if request.mode == "outline" {
		if !wholeFile {
			return agent.AgentToolResult{}, errors.New("mode=outline covers the whole file; drop the line range, offset and limit, or drop mode to read those lines")
		}
		outline, ok := "", false
		if !isResource {
			outline, ok = goSourceOutline(displayPath, absolutePath, data)
		}
		if !ok {
			return agent.AgentToolResult{}, fmt.Errorf("mode=outline needs a workspace Go file that parses; read %s without mode for its numbered content", displayPath)
		}
		observe(nil)
		return outlineResult(header, "", outline, request.budget), nil
	}

	allLines := splitReadLines(string(data))
	if len(allLines) == 0 {
		observe(nil)
		return textResult(header, nil), nil
	}
	startLine := 0
	endLine := len(allLines)
	userLimitedLines := -1
	if selector.Set {
		startLine = selector.Start - 1
		endLine = min(selector.End, len(allLines))
	} else {
		if request.offset != nil {
			startLine = max(0, *request.offset-1)
		}
		if request.limit != nil {
			if *request.limit <= 0 {
				return agent.AgentToolResult{}, fmt.Errorf("limit must be greater than zero")
			}
			endLine = min(startLine+*request.limit, len(allLines))
			userLimitedLines = endLine - startLine
		}
	}
	if startLine >= len(allLines) {
		return agent.AgentToolResult{}, fmt.Errorf("offset %d is beyond end of file (%d lines total)", startLine+1, len(allLines))
	}

	numberedLines := make([]string, endLine-startLine)
	for index := startLine; index < endLine; index++ {
		numberedLines[index-startLine] = fmt.Sprintf("%d:%s", index+1, allLines[index])
	}
	byteLimit := truncate.FormatSize(request.budget.bytes)
	room := fmt.Sprintf("one read shows (%d lines or %s)", request.budget.lines, byteLimit)
	if request.batch {
		room = fmt.Sprintf("this call had room for (%d lines or %s)", request.budget.lines, byteLimit)
	}
	truncation := truncate.Head(strings.Join(numberedLines, "\n"), request.budget.options())
	if truncation.Truncated && wholeFile && request.mode == "auto" && !isResource {
		// The whole file does not fit. A Go file's declaration outline lets
		// the model pick the range it needs instead of paging blind; the
		// outline shows no rows, so it marks none seen.
		if outline, ok := goSourceOutline(displayPath, absolutePath, data); ok {
			note := fmt.Sprintf("[%s has %d lines (%s), more than %s, so this is a structural outline of its declarations, not the file's content. Each row is LINE: declaration [recover: PATH:START-END]. To see or edit a declaration, read its range, e.g. read %q; mode=raw with offset and limit pages the numbered lines instead.]", displayPath, len(allLines), truncate.FormatSize(len(data)), room, displayPath+":START-END")
			observe(nil)
			return outlineResult(header, note, outline, request.budget), nil
		}
	}
	output := header + "\n" + truncation.Content
	var details *readToolDetails
	// A batched read has no offset, so it continues with a path selector.
	continuation := func(next int) string {
		if selector.Set || request.batch {
			return fmt.Sprintf("%s:%d-%d", displayPath, next, endLine)
		}
		return fmt.Sprintf("offset=%d", next)
	}
	if truncation.FirstLineExceedsLimit {
		firstLineSize := truncate.FormatSize(len([]byte(allLines[startLine])))
		output = fmt.Sprintf("%s\n[%d: line is %s, exceeds %s limit. Use bash to read a bounded portion.]", header, startLine+1, firstLineSize, byteLimit)
		if request.batch {
			output = fmt.Sprintf("%s\n[%d: line is %s, more than %s. Read %s on its own, or use bash for a bounded portion.]", header, startLine+1, firstLineSize, room, displayPath)
		}
		details = &readToolDetails{Truncation: truncation}
	} else if truncation.Truncated {
		endDisplay := startLine + truncation.OutputLines
		nextOffset := endDisplay + 1
		limitNote := ""
		if truncation.TruncatedBy != truncate.TruncatedByLines {
			limitNote = fmt.Sprintf(" (%s limit)", byteLimit)
		}
		if selector.Set {
			output += fmt.Sprintf("\n\n[Showing lines %d-%d of selected lines %d-%d%s. Use %s to continue.]", startLine+1, endDisplay, startLine+1, endLine, limitNote, continuation(nextOffset))
		} else {
			output += fmt.Sprintf("\n\n[Showing lines %d-%d of %d%s. Use %s to continue.]", startLine+1, endDisplay, len(allLines), limitNote, continuation(nextOffset))
		}
		details = &readToolDetails{Truncation: truncation}
	} else if !selector.Set && userLimitedLines >= 0 && startLine+userLimitedLines < len(allLines) {
		remaining := len(allLines) - (startLine + userLimitedLines)
		nextOffset := startLine + userLimitedLines + 1
		output += fmt.Sprintf("\n\n[%d more lines in file. Use offset=%d to continue.]", remaining, nextOffset)
	}
	if !truncation.FirstLineExceedsLimit {
		shown := truncation.OutputLines
		if truncation.LastLinePartial {
			shown--
		}
		displayed := make([]int, 0, max(shown, 0))
		for line := startLine + 1; line <= startLine+shown; line++ {
			displayed = append(displayed, line)
		}
		observe(displayed)
	}
	return textResult(output, details), nil
}
