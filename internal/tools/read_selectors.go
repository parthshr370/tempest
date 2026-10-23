package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/document"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/pathutil"
	"go.harness.dev/harness/internal/refs"
)

// maxDirEntries bounds a directory listing from read.
const maxDirEntries = 500

// readTarget splits a read path (after any trailing line range) into the
// file and the selectors applied to its content:
//
//	bundle.zip:dir/a.txt   archive member (.zip, .tar, .tar.gz, .tgz)
//	data.json?q=.items[0]  JSON query, see read_jsonquery.go
type readTarget struct {
	path      string
	member    string
	hasMember bool
	query     string
	hasQuery  bool
}

func parseReadTarget(requested string) readTarget {
	target := readTarget{path: requested}
	if before, query, ok := strings.Cut(requested, "?q="); ok {
		target.path, target.query, target.hasQuery = before, query, true
	}
	if colon := archiveMemberSplit(target.path); colon >= 0 {
		target.member, target.path, target.hasMember = target.path[colon+1:], target.path[:colon], true
	}
	return target
}

// readSource is what the read tool renders. isResource is set whenever data
// is not the raw bytes of a workspace file (URL reads, listings, archive
// members, extracted document text, query output); absolutePath is then "".
type readSource struct {
	data         []byte
	absolutePath string
	displayPath  string
	isResource   bool
}

// loadReadSource resolves a read path (workspace file, session artifact or
// internal URL) and applies its content selectors. A non-nil result is
// returned as is (images).
func loadReadSource(ctx context.Context, cwd string, root *os.Root, options ToolsOptions, requested string) (readSource, *agent.AgentToolResult, error) {
	target := parseReadTarget(requested)
	var src readSource
	var name string // drives format detection
	listing := false
	if refs.IsURL(target.path) {
		resource, err := options.Refs.Read(ctx, target.path)
		if err != nil {
			return readSource{}, nil, err
		}
		src = readSource{data: resource.Data, displayPath: resource.URI, isResource: true}
		name = resource.URI
		if resource.Dir {
			entries := make([]dirEntry, len(resource.Entries))
			for i, entry := range resource.Entries {
				entries[i] = dirEntry{name: entry.Name, dir: entry.Dir}
			}
			src.data, listing = renderDirEntries(entries, resource.Total), true
		}
		listing = listing || resource.Rendered
	} else {
		absolutePath := pathutil.ResolveReadPath(target.path, cwd)
		displayPath := readDisplayPath(cwd, absolutePath, target.path)
		var data []byte
		relPath, err := rootRelPath(cwd, absolutePath)
		if err == nil {
			info, statErr := statInRoot(root, relPath)
			switch {
			case statErr == nil && info.IsDir():
				text, listErr := listWorkspaceDir(root, relPath)
				if listErr != nil {
					return readSource{}, nil, listErr
				}
				data, listing = text, true
			case statErr == nil && archiveKind(relPath) != "" && info.Size() > maxArchiveBytes:
				return readSource{}, nil, fmt.Errorf("archive %s is %d bytes, over the %d MiB read limit; use bash (unzip -l, tar -tf)", displayPath, info.Size(), maxArchiveBytes>>20)
			default:
				data, err = readFileInRoot(root, relPath)
			}
		} else if artifact, ok := sessionArtifactName(options.SessionDataDir, absolutePath); ok && errors.Is(err, ErrOutsideRoot) {
			data, err = readSessionArtifact(options.SessionDataDir, artifact)
		}
		if err != nil {
			return readSource{}, nil, err
		}
		src = readSource{data: data, displayPath: displayPath, isResource: listing}
		if !listing {
			src.absolutePath = absolutePath
		}
		name = absolutePath
	}
	if listing {
		if target.hasMember || target.hasQuery {
			return readSource{}, nil, fmt.Errorf("%s is not a file; archive members and ?q= queries apply to files", src.displayPath)
		}
		return src, nil, nil
	}
	if !target.hasMember && !target.hasQuery {
		if mimeType := imageMimeType(name); mimeType != "" {
			result := agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("Read image file [" + mimeType + "]"), types.NewImage(base64.StdEncoding.EncodeToString(src.data), mimeType)}}
			return readSource{}, &result, nil
		}
	}
	derived := func(data []byte, display string) {
		src.data, src.displayPath, src.absolutePath, src.isResource = data, display, "", true
	}
	if kind := archiveKind(name); kind != "" {
		if len(src.data) > maxArchiveBytes {
			return readSource{}, nil, fmt.Errorf("archive %s is %d bytes, over the %d MiB read limit; use bash (unzip -l, tar -tf)", src.displayPath, len(src.data), maxArchiveBytes>>20)
		}
		if !target.hasMember {
			text, err := listArchive(kind, src.data)
			if err != nil {
				return readSource{}, nil, fmt.Errorf("%s: %w", src.displayPath, err)
			}
			if target.hasQuery {
				return readSource{}, nil, fmt.Errorf("%s is an archive; query a JSON member with %s:<member>?q=...", src.displayPath, src.displayPath)
			}
			derived([]byte(text), src.displayPath)
			return src, nil, nil
		}
		member, err := readArchiveMember(kind, src.data, target.member)
		if err != nil {
			return readSource{}, nil, fmt.Errorf("%s: %w", src.displayPath, err)
		}
		display := src.displayPath + ":" + target.member
		if member.listing != "" {
			if target.hasQuery {
				return readSource{}, nil, fmt.Errorf("%s is a directory inside the archive; ?q= applies to files", display)
			}
			derived([]byte(member.listing), display)
			return src, nil, nil
		}
		derived(member.data, display)
		name = target.member
	}
	if err := unsupportedFormat(name, src.data, src.displayPath); err != nil {
		return readSource{}, nil, err
	}
	if mediaType := document.OOXMLMediaType(name); mediaType != "" {
		text, err := document.ExtractOOXMLBytes(src.data, mediaType)
		if err != nil {
			return readSource{}, nil, fmt.Errorf("%s: %w", src.displayPath, err)
		}
		if target.hasQuery {
			return readSource{}, nil, fmt.Errorf("%s is an office document; ?q= applies to JSON files", src.displayPath)
		}
		derived([]byte(text), src.displayPath)
	}
	if target.hasQuery {
		output, err := runJSONQuery(src.data, target.query)
		if err != nil {
			return readSource{}, nil, fmt.Errorf("%s: %w", src.displayPath, err)
		}
		derived([]byte(output), src.displayPath+"?q="+target.query)
	}
	return src, nil, nil
}

// unsupportedFormat names formats read cannot render, so the model goes
// straight to a tool that can instead of seeing "binary file".
func unsupportedFormat(name string, data []byte, display string) error {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".pdf") || bytes.HasPrefix(data, []byte("%PDF-")):
		return fmt.Errorf("%s is a PDF; read does not render PDFs. Use bash, for example pdftotext %s - | head -200", display, shellWord(display))
	case bytes.HasPrefix(data, []byte("SQLite format 3\x00")) || strings.HasSuffix(lower, ".sqlite") || strings.HasSuffix(lower, ".sqlite3"):
		return fmt.Errorf("%s is a SQLite database; read does not query databases. Use bash, for example sqlite3 %s '.tables'", display, shellWord(display))
	}
	return nil
}

// shellWord quotes display for a suggested command; internal URLs expand in
// bash, so they stay as written.
func shellWord(display string) string {
	if refs.IsURL(display) {
		return display
	}
	return "'" + strings.ReplaceAll(display, "'", `'\''`) + "'"
}

type dirEntry struct {
	name string
	dir  bool
}

// listWorkspaceDir lists a workspace directory through the root.
func listWorkspaceDir(root *os.Root, relPath string) ([]byte, error) {
	dirEntries, err := fs.ReadDir(root.FS(), filepath.ToSlash(relPath))
	if err != nil {
		return nil, fmt.Errorf("Cannot read directory: %s", err.Error())
	}
	sort.Slice(dirEntries, func(i, j int) bool {
		return strings.ToLower(dirEntries[i].Name()) < strings.ToLower(dirEntries[j].Name())
	})
	entries := make([]dirEntry, 0, min(len(dirEntries), maxDirEntries))
	total := 0
	for _, entry := range dirEntries {
		info, err := statInRoot(root, path.Join(filepath.ToSlash(relPath), entry.Name()))
		if err != nil {
			continue
		}
		total++
		if len(entries) < maxDirEntries {
			entries = append(entries, dirEntry{name: entry.Name(), dir: info.IsDir()})
		}
	}
	return renderDirEntries(entries, total), nil
}

// renderDirEntries prints one entry per line, directories with a trailing
// slash, like the ls tool.
func renderDirEntries(entries []dirEntry, total int) []byte {
	var b strings.Builder
	if total == 0 {
		b.WriteString("(empty directory)\n")
	}
	for _, entry := range entries {
		b.WriteString(entry.name)
		if entry.dir {
			b.WriteByte('/')
		}
		b.WriteByte('\n')
	}
	if total > len(entries) {
		fmt.Fprintf(&b, "[%d more entries not shown; use ls with a limit or find]\n", total-len(entries))
	}
	return []byte(b.String())
}
