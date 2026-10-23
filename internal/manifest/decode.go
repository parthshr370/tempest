// Package manifest compares two already-rendered sets of Kubernetes-style
// resources. It reads YAML and JSON documents through a caller-supplied
// fs.FS, keys every resource by its (apiVersion, kind, namespace, name)
// identity, normalizes presentation (key order, comments, anchors, merge
// keys) and reports added, removed and changed resources with bounded
// canonical-YAML display diffs. It never executes a renderer, contacts a
// cluster or applies defaults: the inputs are compared exactly as written.
package manifest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// Per-side input bounds. They keep a single comparison's memory and CPU
// proportional to a reviewable manifest set rather than to the filesystem.
const (
	maxSideFiles     = 2048
	maxSideBytes     = 16 << 20
	maxFileBytes     = 2 << 20
	maxSideResources = 2000
	maxNesting       = 64
	maxSideNodes     = 200_000
	// maxVisitedEntries bounds directory traversal itself, so a directory
	// full of ineligible files cannot make a comparison walk without limit.
	maxVisitedEntries = 65_536
	// cancelCheckInterval is how many converted YAML nodes pass between
	// context checks while a large document is being normalized.
	cancelCheckInterval = 1024
	// maxTagDisplay bounds an unsupported tag quoted back in an error.
	maxTagDisplay = 64
)

// eligibleExtensions are the file types read when a directory is walked. A
// directly selected file is read whatever its extension.
var eligibleExtensions = map[string]bool{".yaml": true, ".yml": true, ".json": true}

// Error reports why one side of a comparison could not be read. It names the
// side, the source file and the one-based document, never a scalar value, so
// a malformed Secret cannot leak through the message.
type Error struct {
	Side     string
	Path     string
	Document int
	Reason   string
}

func (e *Error) Error() string {
	var location strings.Builder
	location.WriteString(e.Side)
	if e.Path != "" {
		location.WriteString(" ")
		location.WriteString(e.Path)
	}
	if e.Document > 0 {
		fmt.Fprintf(&location, " document %d", e.Document)
	}
	return location.String() + ": " + e.Reason
}

// view selects one file or directory inside a filesystem. Paths reported
// for resources stay relative to the filesystem root, so a comparison of
// old/ and new/ shows old/deploy.yaml rather than two identical names.
type view struct {
	fs.FS
	selected string
}

// View returns fsys restricted to the file or directory named selected, a
// slash-separated fs.ValidPath. Compare reads a selected file whatever its
// extension and walks a selected directory for .yaml, .yml and .json files.
// A filesystem passed to Compare without View selects its root directory.
func View(fsys fs.FS, selected string) fs.FS {
	return view{FS: fsys, selected: selected}
}

// resource is one decoded resource with its canonical content.
type resource struct {
	identity    Identity
	source      Source
	content     *node
	fingerprint string
}

// sideLoader reads one side of a comparison within the per-side bounds.
type sideLoader struct {
	ctx        context.Context
	side       string
	fsys       fs.FS
	files      int
	totalBytes int
	visited    int
	nodes      int
	resources  map[Identity]*resource
}

// loadSide decodes every resource reachable from the side's selection.
func loadSide(ctx context.Context, side string, input fs.FS) (map[Identity]*resource, error) {
	fsys, selected := input, "."
	if selection, ok := input.(view); ok {
		fsys, selected = selection.FS, selection.selected
	}
	if !fs.ValidPath(selected) {
		return nil, &Error{Side: side, Path: selected, Reason: "invalid path"}
	}
	loader := &sideLoader{ctx: ctx, side: side, fsys: fsys, resources: map[Identity]*resource{}}
	if err := loader.load(selected); err != nil {
		return nil, err
	}
	return loader.resources, nil
}

func (l *sideLoader) fail(name string, document int, format string, args ...any) error {
	return &Error{Side: l.side, Path: name, Document: document, Reason: fmt.Sprintf(format, args...)}
}

// load rejects a selection reached through a symlink, then reads it as a
// file or walks it as a directory.
func (l *sideLoader) load(selected string) error {
	if selected != "." {
		parts := strings.Split(selected, "/")
		for i := range parts {
			prefix := strings.Join(parts[:i+1], "/")
			info, err := fs.Lstat(l.fsys, prefix)
			if err != nil {
				return l.fail(selected, 0, "%s", describeIOError(err))
			}
			if info.Mode()&fs.ModeSymlink != 0 {
				return l.fail(selected, 0, "symlinked input %s is not followed", prefix)
			}
		}
	}
	info, err := fs.Stat(l.fsys, selected)
	if err != nil {
		return l.fail(selected, 0, "%s", describeIOError(err))
	}
	switch {
	case info.IsDir():
		return l.walk(selected)
	case info.Mode().IsRegular():
		return l.readFile(selected)
	default:
		return l.fail(selected, 0, "not a regular file or directory")
	}
}

// walk visits a directory in lexical order, reading eligible files and
// recursing into subdirectories. Version-control metadata is skipped; a
// symlinked entry fails the comparison instead of silently dropping the
// resources it may hold.
func (l *sideLoader) walk(directory string) error {
	entries, err := fs.ReadDir(l.fsys, directory)
	if err != nil {
		return l.fail(directory, 0, "%s", describeIOError(err))
	}
	for _, entry := range entries {
		if err := l.ctx.Err(); err != nil {
			return err
		}
		l.visited++
		if l.visited > maxVisitedEntries {
			return l.fail(directory, 0, "more than %d directory entries", maxVisitedEntries)
		}
		name := path.Join(directory, entry.Name())
		mode := entry.Type()
		switch {
		case mode&fs.ModeSymlink != 0:
			return l.fail(name, 0, "symlinked entry is not followed")
		case entry.IsDir():
			if entry.Name() == ".git" {
				continue
			}
			if err := l.walk(name); err != nil {
				return err
			}
		case eligibleExtensions[strings.ToLower(path.Ext(name))]:
			if !mode.IsRegular() {
				return l.fail(name, 0, "not a regular file")
			}
			if err := l.readFile(name); err != nil {
				return err
			}
		}
	}
	return nil
}

// readFile reads one file within the per-file and per-side byte budgets.
func (l *sideLoader) readFile(name string) error {
	l.files++
	if l.files > maxSideFiles {
		return l.fail(name, 0, "more than %d files", maxSideFiles)
	}
	file, err := l.fsys.Open(name)
	if err != nil {
		return l.fail(name, 0, "%s", describeIOError(err))
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return l.fail(name, 0, "%s", describeIOError(err))
	}
	if !info.Mode().IsRegular() {
		return l.fail(name, 0, "not a regular file")
	}
	if info.Size() > maxFileBytes {
		return l.fail(name, 0, "file exceeds %d bytes", maxFileBytes)
	}
	content, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil {
		return l.fail(name, 0, "%s", describeIOError(err))
	}
	if len(content) > maxFileBytes {
		return l.fail(name, 0, "file exceeds %d bytes", maxFileBytes)
	}
	l.totalBytes += len(content)
	if l.totalBytes > maxSideBytes {
		return l.fail(name, 0, "input exceeds %d bytes", maxSideBytes)
	}
	return l.decodeStream(name, content)
}

// decodeStream decodes a multi-document stream one document at a time.
// Empty and null documents are skipped but still counted, so document
// numbers match the separators a reader sees in the file.
func (l *sideLoader) decodeStream(name string, content []byte) error {
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	for document := 1; ; document++ {
		if err := l.ctx.Err(); err != nil {
			return err
		}
		var parsed yaml.Node
		err := decoder.Decode(&parsed)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return l.fail(name, document, "malformed YAML: %s", strings.TrimPrefix(err.Error(), "yaml: "))
		}
		if len(parsed.Content) == 0 {
			continue
		}
		root := parsed.Content[0]
		if root.Kind == yaml.ScalarNode && root.ShortTag() == nullTag {
			continue
		}
		converter := &converter{loader: l, name: name, document: document, inProgress: map[*yaml.Node]bool{}}
		content, err := converter.convert(root, 1)
		if err != nil {
			return err
		}
		if err := l.addResource(name, document, content, 0); err != nil {
			return err
		}
	}
}

// addResource validates one document root (or List item) and records its
// identity. Only the core v1 List wrapper is expanded; its own metadata is
// not a resource.
func (l *sideLoader) addResource(name string, document int, content *node, listDepth int) error {
	if content.kind != kindMap {
		return l.fail(name, document, "a resource must be a mapping")
	}
	apiVersion, apiOK := content.stringField("apiVersion")
	kind, kindOK := content.stringField("kind")
	if !apiOK || apiVersion == "" {
		return l.fail(name, document, "apiVersion must be a nonempty string")
	}
	if !kindOK || kind == "" {
		return l.fail(name, document, "kind must be a nonempty string")
	}
	if apiVersion == "v1" && kind == "List" {
		return l.addListItems(name, document, content, listDepth)
	}
	metadata, ok := content.field("metadata")
	if !ok || metadata.kind != kindMap {
		return l.fail(name, document, "metadata must be a mapping")
	}
	resourceName, nameOK := metadata.stringField("name")
	if !nameOK || resourceName == "" {
		if _, hasGenerateName := metadata.field("generateName"); hasGenerateName && !nameOK {
			return l.fail(name, document, "metadata.generateName without metadata.name has no stable identity")
		}
		return l.fail(name, document, "metadata.name must be a nonempty string")
	}
	namespace := ""
	if namespaceNode, present := metadata.field("namespace"); present {
		if namespaceNode.kind != kindString {
			return l.fail(name, document, "metadata.namespace must be a string when present")
		}
		namespace = namespaceNode.text
	}
	identity := Identity{APIVersion: apiVersion, Kind: kind, Namespace: namespace, Name: resourceName}
	if existing, duplicate := l.resources[identity]; duplicate {
		return l.fail(name, document, "duplicate resource %s; first defined in %s document %d", identity.label(), existing.source.Path, existing.source.Document)
	}
	if len(l.resources) >= maxSideResources {
		return l.fail(name, document, "more than %d resources", maxSideResources)
	}
	l.resources[identity] = &resource{identity: identity, source: Source{Path: name, Document: document}, content: content, fingerprint: content.fingerprint()}
	return nil
}

// addListItems expands a core v1 List, including nested core Lists.
func (l *sideLoader) addListItems(name string, document int, list *node, listDepth int) error {
	if listDepth >= maxNesting {
		return l.fail(name, document, "List nesting exceeds %d levels", maxNesting)
	}
	items, ok := list.field("items")
	if !ok || items.kind == kindNull {
		return nil
	}
	if items.kind != kindSeq {
		return l.fail(name, document, "List items must be a sequence")
	}
	for _, item := range items.items {
		if err := l.addResource(name, document, item, listDepth+1); err != nil {
			return err
		}
	}
	return nil
}

// describeIOError keeps the useful classification of a filesystem error
// without repeating the path the Error already names.
func describeIOError(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Op + ": " + pathErr.Err.Error()
	}
	return err.Error()
}
