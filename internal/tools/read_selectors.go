package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
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
	table     string
	sqlParams string
}

func parseReadTarget(requested string) readTarget {
	target := readTarget{path: requested}
	if before, params, ok := strings.Cut(requested, "?"); ok && sqlitePath(before) {
		target.path, target.sqlParams = before, params
	} else if before, query, ok := strings.Cut(requested, "?q="); ok {
		target.path, target.query, target.hasQuery = before, query, true
	}
	if colon := archiveMemberSplit(target.path); colon >= 0 {
		target.member, target.path, target.hasMember = target.path[colon+1:], target.path[:colon], true
	}
	if colon := sqliteTableSplit(target.path); colon >= 0 {
		target.table, target.path = target.path[colon+1:], target.path[:colon]
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
		var resource refs.Resource
		var err error
		if refs.GitHubURL(target.path) {
			resource, err = refs.ReadGitHub(refs.WithProcessAdmission(ctx, options.AdmitProcess), cwd, target.path)
			err = codingToolError(err, options)
		} else {
			resource, err = options.Refs.Read(ctx, target.path)
		}
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
			case statErr == nil && (sqlitePath(relPath) || strings.EqualFold(filepath.Ext(relPath), ".pdf")) && info.Size() > document.MaxCodingInput:
				return readSource{}, nil, fmt.Errorf("%s exceeds the %d MiB SQLite/PDF read limit", displayPath, document.MaxCodingInput>>20)
			default:
				if sqlitePath(relPath) || strings.EqualFold(filepath.Ext(relPath), ".pdf") {
					data, err = readFileInRootLimit(root, relPath, document.MaxCodingInput)
				} else {
					data, err = readFileInRoot(root, relPath)
				}
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
	if sqlitePath(name) || bytes.HasPrefix(src.data, []byte("SQLite format 3\x00")) {
		params, query := target.sqlParams, ""
		if strings.HasPrefix(params, "q=") {
			query, params = params[2:], ""
		} else if before, after, ok := strings.Cut(params, "&q="); ok {
			params, query = before, after
		}
		if target.hasQuery {
			query = target.query
		}
		values, err := url.ParseQuery(params)
		if err != nil {
			return readSource{}, nil, fmt.Errorf("invalid SQLite query parameters: %w", err)
		}
		if query != "" {
			values.Set("q", query)
		} else if strings.Contains(target.sqlParams, "q=") {
			return readSource{}, nil, fmt.Errorf("SQLite q must contain a read-only SELECT")
		}
		for key, items := range values {
			if len(items) != 1 {
				return readSource{}, nil, fmt.Errorf("SQLite query parameter %q must occur once", key)
			}
			if key == "q" && strings.TrimSpace(items[0]) == "" {
				return readSource{}, nil, fmt.Errorf("SQLite q must contain a read-only SELECT")
			}
			if key != "q" && key != "limit" && key != "offset" {
				return readSource{}, nil, fmt.Errorf("unknown SQLite query parameter %q", key)
			}
		}
		limit, offset := 20, 0
		for key, dest := range map[string]*int{"limit": &limit, "offset": &offset} {
			if value := values.Get(key); value != "" {
				*dest, err = strconv.Atoi(value)
				if err != nil {
					return readSource{}, nil, fmt.Errorf("SQLite %s must be an integer", key)
				}
			}
		}
		if values.Get("q") != "" && values.Get("limit") == "" {
			limit = 1000
		}
		var wal []byte
		if src.absolutePath != "" {
			if rel, err := rootRelPath(cwd, src.absolutePath+"-wal"); err == nil {
				if info, err := root.Stat(rel); err == nil && info.Size() > document.MaxCodingInput {
					return readSource{}, nil, fmt.Errorf("SQLite WAL exceeds the %d MiB read limit", document.MaxCodingInput>>20)
				}
				wal, err = readFileInRootLimit(root, rel, document.MaxCodingInput)
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return readSource{}, nil, err
				}
			}
		}
		release, err := admitCodingProcess(ctx, options)
		if err != nil {
			return readSource{}, nil, err
		}
		defer release()
		text, err := document.ReadSQLite(ctx, src.data, wal, target.table, values.Get("q"), limit, offset)
		if err != nil {
			return readSource{}, nil, fmt.Errorf("%s: %w", src.displayPath, err)
		}
		derived(text, requested)
		return src, nil, nil
	}
	if strings.EqualFold(path.Ext(name), ".pdf") || bytes.HasPrefix(src.data, []byte("%PDF-")) {
		if target.hasQuery {
			return readSource{}, nil, fmt.Errorf("%s is a PDF; ?q= applies to JSON or SQLite", src.displayPath)
		}
		release, err := admitCodingProcess(ctx, options)
		if err != nil {
			return readSource{}, nil, err
		}
		defer release()
		text, err := document.ReadPDF(ctx, src.data)
		if err != nil {
			return readSource{}, nil, fmt.Errorf("%s: %w", src.displayPath, err)
		}
		derived(text, src.displayPath)
		return src, nil, nil
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

func sqlitePath(name string) bool {
	if colon := sqliteTableSplit(name); colon >= 0 {
		name = name[:colon]
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".db", ".db3", ".sqlite", ".sqlite3":
		return true
	}
	return false
}

func sqliteTableSplit(name string) int {
	for i := strings.IndexByte(name, ':'); i >= 0; {
		switch strings.ToLower(path.Ext(name[:i])) {
		case ".db", ".db3", ".sqlite", ".sqlite3":
			return i
		}
		next := strings.IndexByte(name[i+1:], ':')
		if next < 0 {
			break
		}
		i += next + 1
	}
	return -1
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
