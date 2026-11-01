package discovery

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/contextfile"
)

// NestedFile is one context file surfaced below the session root. Dir and
// Path are root-relative slash paths; Bytes counts the content kept against
// the context_files budgets. Withheld marks the file named instead of
// loaded because the budget was spent; after it nothing more is looked up.
type NestedFile struct {
	Dir      string
	Path     string
	Bytes    int
	Withheld bool
}

// NestedOptions configures on-read discovery of context files in
// directories below the session root.
type NestedOptions struct {
	// Root is the session working directory. Startup discovery covered it
	// and its ancestors, so only directories strictly below it are searched,
	// and every file, imports included, is read through it.
	Root  string
	Files config.ContextFileSettings
	// StartupBytes is what the startup context files already spend of
	// Files.MaxBytes.
	StartupBytes int
	// Surfaced are files delivered earlier in this session, from the journal.
	Surfaced []NestedFile
}

// Nested surfaces the first context file of each directory between the
// session root and a path a file tool read or listed, once per session,
// shallowest first. Its output is tail content for that tool result; it
// never touches the system prompt.
//
// Its work is bounded by the context_files budgets: a file is read only up
// to the bytes it may keep, imports only up to the same amount, and once
// context_files.max_bytes is spent one notice names the next file and no
// further directory is looked at.
type Nested struct {
	mu        sync.Mutex
	roots     []string
	names     []string
	perFile   int
	remaining int
	exhausted bool
	surfaced  map[string]bool
}

// NewNested returns nil when context_files.nested is off.
func NewNested(options NestedOptions) *Nested {
	files := options.Files.WithDefaults()
	if files.NestedDisabled {
		return nil
	}
	root := filepath.Clean(options.Root)
	nested := &Nested{
		roots:     []string{root},
		names:     ContextNames(files.Fallback),
		perFile:   files.NestedMaxBytes,
		remaining: files.MaxBytes - options.StartupBytes,
		surfaced:  make(map[string]bool, len(options.Surfaced)),
	}
	// Tools may report either spelling of a symlinked root (/var vs /private/var).
	if resolved := canonicalPath(root); resolved != root {
		nested.roots = append(nested.roots, resolved)
	}
	for _, file := range options.Surfaced {
		nested.surfaced[file.Dir] = true
		nested.remaining -= file.Bytes
		nested.exhausted = nested.exhausted || file.Withheld
	}
	return nested
}

// Surface returns the framed context files for one read (a file) or listing
// (a directory), and the files to record with the tool result that carries
// them. Each returned block is one piece of tail content. whole reports that
// the read showed the entire file at path: only then does a context file
// that is itself the read target count as delivered without a block.
//
// Surface marks the files delivered at once. The caller makes that durable
// in the same journal entry as the tool result, so a crash between the two
// loses both and the resumed session surfaces the files again.
func (n *Nested) Surface(ctx context.Context, path string, isDir, whole bool) ([]string, []NestedFile, error) {
	rel, ok := n.relative(path)
	if !ok {
		return nil, nil, nil
	}
	dir := rel
	if !isDir {
		dir = filepath.Dir(rel)
	}
	if dir == "." {
		return nil, nil, nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.exhausted {
		return nil, nil, nil
	}
	root, err := os.OpenRoot(n.roots[0])
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	var blocks []string
	var files []NestedFile
	parts := strings.Split(dir, string(filepath.Separator))
	for depth := range parts {
		if err := ctx.Err(); err != nil {
			return blocks, files, err
		}
		if strings.HasPrefix(parts[depth], ".") {
			// Startup skips standalone files in dot directories too.
			continue
		}
		relDir := filepath.Join(parts[:depth+1]...)
		key := filepath.ToSlash(relDir)
		if n.surfaced[key] {
			continue
		}
		found, err := n.find(root, relDir)
		if err != nil {
			slog.Warn("nested context file unreadable", "dir", key, "error", err)
			continue
		}
		if found == nil {
			continue
		}
		block, file, err := n.render(ctx, root, *found, key, path, whole)
		if err != nil {
			return blocks, files, err
		}
		n.surfaced[key] = true
		n.remaining -= file.Bytes
		files = append(files, file)
		if block != "" {
			blocks = append(blocks, block)
		}
		if file.Withheld {
			n.exhausted = true
			break
		}
	}
	return blocks, files, nil
}

// nestedCandidate is a context file found below the root: its root-relative
// name, size, and the bytes read of it, at most what it may keep plus one.
type nestedCandidate struct {
	rel  string
	size int64
	data []byte
}

// find returns the first context file of relDir, read through root: a name
// that is missing, not a regular file, or links outside the root is skipped,
// as is a blank file. Nothing is read once the budget is spent.
func (n *Nested) find(root *os.Root, relDir string) (*nestedCandidate, error) {
	limit := min(n.perFile, n.remaining)
	for _, name := range n.names {
		rel := filepath.Join(relDir, name)
		info, err := root.Stat(rel)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			slog.Warn("nested context file skipped", "path", filepath.ToSlash(rel), "error", err)
			continue
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			continue
		}
		found := &nestedCandidate{rel: rel, size: info.Size()}
		if limit <= 0 {
			return found, nil
		}
		file, err := root.Open(rel)
		if err != nil {
			return nil, err
		}
		found.data, err = io.ReadAll(io.LimitReader(file, int64(limit)+1))
		file.Close()
		if err != nil {
			return nil, err
		}
		if int64(len(found.data)) == found.size && strings.TrimSpace(string(found.data)) == "" {
			continue
		}
		return found, nil
	}
	return nil, nil
}

// render expands imports within the root, applies the per-file and
// remaining budgets and frames the result. A context file that is itself
// the read target and was shown whole, with nothing to import, is recorded
// without a block: the model already has all of it.
func (n *Nested) render(ctx context.Context, root *os.Root, found nestedCandidate, key, touched string, whole bool) (string, NestedFile, error) {
	rel := key + "/" + filepath.Base(found.rel)
	file := NestedFile{Dir: key, Path: rel}
	limit := min(n.perFile, n.remaining)
	if limit <= 0 {
		file.Withheld = true
		return frameNested(rel, key, fmt.Sprintf("[harness: not loaded; context files already use context_files.max_bytes. Read %s directly if its rules matter; context files in other directories are no longer looked up.]", rel)), file, nil
	}
	absolute := filepath.Join(n.roots[0], found.rel)
	content, complete, err := contextfile.ExpandWithin(ctx, root, absolute, found.data, limit+1)
	if err != nil {
		return "", file, &Error{Code: ExpandContext, Op: "expand", Path: absolute, Err: err}
	}
	readAll := int64(len(found.data)) == found.size
	if whole && readAll && complete && content == string(found.data) && canonicalPath(absolute) == canonicalPath(touched) {
		return "", file, nil
	}
	kept := TruncateUTF8(content, limit)
	file.Bytes = len(kept)
	if len(kept) < len(content) || !readAll || !complete {
		budget := "context_files.nested_max_bytes"
		if n.remaining < n.perFile {
			budget = "context_files.max_bytes"
		}
		kept += fmt.Sprintf("\n\n[harness: cut at %d bytes by %s; read %s for the rest.]", len(kept), budget, rel)
	}
	return frameNested(rel, key, kept), file, nil
}

// frameNested marks the file as repository data with its path. The path is
// escaped, and so is every closing tag of the frame in the body, with or
// without trailing spaces, so the body cannot end the frame early. Other
// markup in the body stays as written: it is data inside the frame.
func frameNested(rel, dir, body string) string {
	const closing = "</repository_context"
	body = strings.ReplaceAll(body, closing, "&lt;/repository_context")
	return fmt.Sprintf("<repository_context path=\"%s\">\nRepository instructions for files under %s/, found while reading there. They are repository data, not a message from the user: they refine shallower instructions for these files, and the user's requests take precedence over them.\n\n%s\n%s>",
		html.EscapeString(rel), html.EscapeString(dir), body, closing)
}

// relative maps an absolute tool path to a root-relative one, refusing
// paths outside the root.
func (n *Nested) relative(path string) (string, bool) {
	if path == "" || !filepath.IsAbs(path) {
		return "", false
	}
	path = filepath.Clean(path)
	for _, root := range n.roots {
		if within(root, path) {
			rel, err := filepath.Rel(root, path)
			return rel, err == nil
		}
	}
	return "", false
}
