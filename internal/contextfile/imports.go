// Package contextfile expands bounded @file references in context files.
package contextfile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MaxImportDepth is the largest number of import edges followed from a root.
const MaxImportDepth = 5

// ExpandOptions configures import expansion.
type ExpandOptions struct {
	// HomeDir resolves ~ imports; they must still lie inside the root.
	// Empty leaves them literal.
	HomeDir string
	// MaxBytes bounds what all imports read together; negative is no limit.
	MaxBytes int
}

// ErrorCode classifies context import errors.
type ErrorCode string

const (
	InvalidOptions ErrorCode = "invalid_options"
	InvalidSource  ErrorCode = "invalid_source"
	ReadSource     ErrorCode = "read_source"
)

// Error is a typed context-import failure.
type Error struct {
	Code ErrorCode
	Path string
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("context import %s: %s", e.Code, e.Path) }
func (e *Error) Unwrap() error { return e.Err }
func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && e.Code == other.Code
}

// Expand expands @file imports of filePath, a file inside root. Every
// import is opened through root, so neither a ../ or absolute path nor a
// symlink reaches a file outside it. Imports read at most options.MaxBytes
// bytes together; complete is false when they were cut there. Imports that
// cannot be read stay literal; only invalid options, source failures and
// cancellation fail.
func Expand(ctx context.Context, root *os.Root, filePath string, content []byte, options ExpandOptions) (text string, complete bool, err error) {
	if root == nil {
		return "", false, &Error{Code: InvalidOptions, Path: filePath, Err: errors.New("a root is required")}
	}
	e := &expander{home: options.HomeDir, root: root, rootDir: filepath.Clean(root.Name()), budget: options.MaxBytes}
	text, e, err = expandFrom(ctx, filePath, content, e)
	return text, e != nil && !e.truncated, err
}

// expander carries one expansion's limits and state through the import tree.
type expander struct {
	home    string
	visited map[string]struct{}
	// root confines imports; rootDir is its directory.
	root    *os.Root
	rootDir string
	// budget is what imports may still read; negative is unbounded.
	budget    int
	truncated bool
}

func expandFrom(ctx context.Context, filePath string, content []byte, e *expander) (string, *expander, error) {
	absolute, err := filepath.Abs(filePath)
	if err != nil {
		return "", nil, &Error{Code: InvalidSource, Path: filePath, Err: err}
	}
	e.visited = map[string]struct{}{canonicalPath(absolute): {}}
	text, err := e.expand(ctx, absolute, string(content), 0)
	return text, e, err
}

// expand walks only prose: fenced code stays literal so examples don't accidentally import local files.
func (e *expander) expand(ctx context.Context, filePath, content string, depth int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var out strings.Builder
	inFence := false
	fenceChar := byte(0)
	fenceLen := 0
	for len(content) > 0 {
		line := content
		newline := ""
		if index := strings.IndexByte(content, '\n'); index >= 0 {
			line, content, newline = content[:index], content[index+1:], "\n"
		} else {
			content = ""
		}
		trimmed := strings.TrimLeft(line, " \t")
		if marker, count := fence(trimmed); count > 0 {
			if !inFence {
				inFence, fenceChar, fenceLen = true, marker, count
			} else if marker == fenceChar && count >= fenceLen {
				inFence = false
			}
			out.WriteString(line)
			out.WriteString(newline)
			continue
		}
		if inFence {
			out.WriteString(line)
			out.WriteString(newline)
			continue
		}
		expanded, err := e.expandLine(ctx, filePath, line, depth)
		if err != nil {
			return "", err
		}
		out.WriteString(expanded)
		out.WriteString(newline)
	}
	return out.String(), nil
}

// fence recognizes the marker and minimum length needed to protect a Markdown code block.
func fence(line string) (byte, int) {
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return 0, 0
	}
	marker := line[0]
	count := 0
	for count < len(line) && line[count] == marker {
		count++
	}
	if count < 3 {
		return 0, 0
	}
	return marker, count
}

// expandLine ignores inline code and keeps trailing prose punctuation outside a resolved import token.
func (e *expander) expandLine(ctx context.Context, source, line string, depth int) (string, error) {
	var out strings.Builder
	inlineRun := 0
	for i := 0; i < len(line); {
		if line[i] == '`' {
			run := 1
			for i+run < len(line) && line[i+run] == '`' {
				run++
			}
			if inlineRun == 0 {
				inlineRun = run
			} else if inlineRun == run {
				inlineRun = 0
			}
			out.WriteString(line[i : i+run])
			i += run
			continue
		}
		if inlineRun != 0 || line[i] != '@' || (i > 0 && line[i-1] != ' ' && line[i-1] != '\t') || i+1 >= len(line) || !importStart(line[i+1]) {
			out.WriteByte(line[i])
			i++
			continue
		}
		end := i + 1
		for end < len(line) && !tokenWhitespace(line[end]) {
			end++
		}
		tokenEnd := end
		for tokenEnd > i+1 && strings.ContainsRune(".,;:!?)]}\"'", rune(line[tokenEnd-1])) {
			tokenEnd--
		}
		candidate := line[i+1 : tokenEnd]
		literal := line[i:end]
		replacement, ok, err := e.importFile(ctx, source, candidate, depth)
		if err != nil {
			return "", err
		}
		if ok {
			out.WriteString(replacement)
			out.WriteString(line[tokenEnd:end])
		} else {
			out.WriteString(literal)
		}
		i = end
	}
	return out.String(), nil
}

func importStart(b byte) bool {
	return b == '.' || b == '/' || b == '~' || b == '_' || b == '-' || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

func tokenWhitespace(b byte) bool {
	switch b {
	case ' ', '\t', '\r', '\n', '\v', '\f':
		return true
	default:
		return false
	}
}

// importFile leaves missing, cyclic, deep, non-regular, out-of-root and
// over-budget references literal so optional imports don't become errors.
func (e *expander) importFile(ctx context.Context, source, target string, depth int) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	resolved := target
	if resolved == "~" || strings.HasPrefix(resolved, "~/") {
		if e.home == "" {
			return "", false, nil
		}
		resolved = filepath.Join(e.home, strings.TrimPrefix(resolved[1:], "/"))
	} else if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(source), resolved)
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", false, nil
	}
	key := canonicalPath(absolute)
	if _, exists := e.visited[key]; exists {
		return "", false, nil
	}
	if depth >= MaxImportDepth {
		return "", false, nil
	}
	data, ok := e.read(absolute)
	if !ok {
		return "", false, nil
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	e.visited[key] = struct{}{}
	expanded, err := e.expand(ctx, absolute, string(data), depth+1)
	if err != nil {
		return "", false, err
	}
	return expanded, true, nil
}

// read returns a regular file's bytes, opened through the root, and at most
// what the budget leaves; a cut read marks the expansion incomplete. Opening
// only regular files keeps a FIFO from blocking.
func (e *expander) read(absolute string) ([]byte, bool) {
	name, err := filepath.Rel(e.rootDir, absolute)
	if err != nil || !filepath.IsLocal(name) {
		return nil, false
	}
	info, err := e.root.Stat(name)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	if e.budget == 0 {
		e.truncated = true
		return nil, false
	}
	file, err := e.root.Open(name)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	var reader io.Reader = file
	if e.budget > 0 {
		reader = io.LimitReader(file, int64(e.budget)+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, false
	}
	if e.budget > 0 {
		if len(data) > e.budget {
			data, e.truncated = data[:e.budget], true
		}
		e.budget -= len(data)
	}
	return data, true
}

// canonicalPath gives the recursion guard one identity for a file even when callers reach it through a symlink.
func canonicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}
