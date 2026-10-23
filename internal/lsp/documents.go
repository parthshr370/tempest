package lsp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"
)

const (
	maxDocumentBytes = 1 << 20
	maxOpenDocuments = 64
	maxOpenBytes     = 16 << 20
)

// errReopen asks the query loop to retire the server and start a fresh
// generation: a document version is about to exhaust int32.
var errReopen = errors.New("document version space exhausted")

// openDoc is the server's view of one disk-backed document.
type openDoc struct {
	path       string
	uri        string
	languageID string
	version    int32
	anchor     string
	text       []byte
	index      *lineIndex
	lastUse    uint64
}

type textDocumentID struct {
	URI string `json:"uri"`
}

type versionedDocumentID struct {
	URI     string `json:"uri"`
	Version int32  `json:"version"`
}

// validateDocument enforces the per-file wire rules: bounded, valid UTF-8,
// no NUL. Text is never normalized.
func validateDocument(doc Document) error {
	if len(doc.Text) > maxDocumentBytes {
		return newError(CodeDocumentLimit, fmt.Sprintf("%s is %d bytes; language server documents are limited to 1 MiB", doc.Path, len(doc.Text)))
	}
	if !utf8.Valid(doc.Text) || bytes.IndexByte(doc.Text, 0) >= 0 {
		return newError(CodeInvalidDocument, doc.Path+" is not valid UTF-8 text or contains NUL bytes")
	}
	return nil
}

// fileURI is the file:// URI of a root-relative path.
func fileURI(absolute string) string {
	return (&url.URL{Scheme: "file", Path: absolute}).String()
}

// uriPath maps a server URI to a contained root-relative path. Only local
// file URIs with an empty or localhost authority, no query or fragment, valid
// percent encoding and no traversal are eligible.
func uriPath(root, uri string) (string, bool) {
	if strings.ContainsAny(uri, "#?") {
		return "", false
	}
	u, err := url.Parse(uri)
	if err != nil || !strings.EqualFold(u.Scheme, "file") || u.Opaque != "" || u.User != nil || (u.Host != "" && !strings.EqualFold(u.Host, "localhost")) {
		return "", false
	}
	p := u.Path
	if p == "" || p[0] != '/' || strings.ContainsRune(p, 0) || path.Clean(p) != p {
		return "", false
	}
	prefix := strings.TrimSuffix(root, "/") + "/"
	if !strings.HasPrefix(p, prefix) || len(p) == len(prefix) {
		return "", false
	}
	return p[len(prefix):], true
}

func (s *server) uri(rel string) string { return fileURI(path.Join(s.m.root, rel)) }

// openDocument returns the open record for rel, if any.
func (s *server) openDocument(rel string) *openDoc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.docs[rel]
}

// syncDocument makes the server's view of doc match its bytes: didOpen at
// version 1, or didChange at version+1 when the hash moved. Must run in the
// server's operation lane.
func (s *server) syncDocument(ctx context.Context, doc Document, languageID string) (*openDoc, error) {
	if err := validateDocument(doc); err != nil {
		return nil, err
	}
	s.mu.Lock()
	existing := s.docs[doc.Path]
	s.mu.Unlock()
	if existing == nil {
		return s.didOpen(ctx, doc, languageID)
	}
	if existing.anchor == doc.Anchor {
		return existing, nil
	}
	if existing.version == math.MaxInt32 {
		return nil, errReopen
	}
	return s.didChange(ctx, existing, doc)
}

func (s *server) didOpen(ctx context.Context, doc Document, languageID string) (*openDoc, error) {
	if err := s.makeRoom(ctx, doc.Path, len(doc.Text)); err != nil {
		return nil, err
	}
	record := &openDoc{path: doc.Path, uri: s.uri(doc.Path), languageID: languageID, version: 1, anchor: doc.Anchor, text: doc.Text, index: newLineIndex(doc.Text)}
	params := map[string]any{"textDocument": map[string]any{"uri": record.uri, "languageId": languageID, "version": 1, "text": string(doc.Text)}}
	if err := s.notify(ctx, "textDocument/didOpen", params); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.docs[doc.Path] = record
	s.openBytes += len(doc.Text)
	s.useSeq++
	record.lastUse = s.useSeq
	s.mu.Unlock()
	return record, nil
}

func (s *server) didChange(ctx context.Context, existing *openDoc, doc Document) (*openDoc, error) {
	growth := len(doc.Text) - len(existing.text)
	if growth > 0 {
		if err := s.makeRoom(ctx, doc.Path, growth); err != nil {
			return nil, err
		}
	}
	change := map[string]any{"text": string(doc.Text)}
	if s.caps.syncKind == 2 {
		// One range replacing the entire previous document, ending at its old UTF-16 end.
		change["range"] = lspRange{End: existing.index.end()}
	}
	version := existing.version + 1
	params := map[string]any{"textDocument": versionedDocumentID{URI: existing.uri, Version: version}, "contentChanges": []any{change}}
	if err := s.notify(ctx, "textDocument/didChange", params); err != nil {
		return nil, err
	}
	record := &openDoc{path: existing.path, uri: existing.uri, languageID: existing.languageID, version: version, anchor: doc.Anchor, text: doc.Text, index: newLineIndex(doc.Text)}
	s.mu.Lock()
	// Reconciling a background change isn't a use; queries touch explicitly.
	record.lastUse = existing.lastUse
	if s.docs[doc.Path] == existing {
		s.docs[doc.Path] = record
		s.openBytes += len(doc.Text) - len(existing.text)
	}
	s.mu.Unlock()
	if s.caps.save {
		save := map[string]any{"textDocument": textDocumentID{URI: record.uri}}
		if s.caps.saveIncludeText {
			save["text"] = string(doc.Text)
		}
		if err := s.notify(ctx, "textDocument/didSave", save); err != nil {
			return nil, err
		}
	}
	return record, nil
}

// didClose closes rel and forgets its cached diagnostics.
func (s *server) didClose(ctx context.Context, rel string) error {
	s.mu.Lock()
	record := s.docs[rel]
	s.mu.Unlock()
	if record == nil {
		return nil
	}
	if err := s.notify(ctx, "textDocument/didClose", map[string]any{"textDocument": textDocumentID{URI: record.uri}}); err != nil {
		return err
	}
	s.mu.Lock()
	if s.docs[rel] == record {
		delete(s.docs, rel)
		s.openBytes -= len(record.text)
	}
	s.diags.forget(rel)
	s.mu.Unlock()
	return nil
}

// pin protects the active query's document from eviction; "" clears it.
// Pins change only inside the operation lane.
func (s *server) pin(rel string) {
	s.mu.Lock()
	s.pinned = rel
	s.mu.Unlock()
}

// makeRoom closes least-recently-used documents until one more document of
// size bytes fits. It never closes the document being synchronized or the
// active query's document.
func (s *server) makeRoom(ctx context.Context, pinned string, size int) error {
	for {
		s.mu.Lock()
		count := len(s.docs)
		if _, open := s.docs[pinned]; open {
			count--
		}
		if count < maxOpenDocuments && s.openBytes+size <= maxOpenBytes {
			s.mu.Unlock()
			return nil
		}
		var victim *openDoc
		for rel, record := range s.docs {
			if rel != pinned && rel != s.pinned && (victim == nil || record.lastUse < victim.lastUse) {
				victim = record
			}
		}
		s.mu.Unlock()
		if victim == nil {
			return newError(CodeDocumentLimit, "open document capacity (64 files, 16 MiB) is exhausted")
		}
		if err := s.didClose(ctx, victim.path); err != nil {
			return err
		}
	}
}

// touch marks record as used by a query for LRU eviction.
func (s *server) touch(record *openDoc) {
	s.mu.Lock()
	s.useSeq++
	record.lastUse = s.useSeq
	s.mu.Unlock()
}

// openPaths lists currently open documents.
func (s *server) openPaths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	paths := make([]string, 0, len(s.docs))
	for rel := range s.docs {
		paths = append(paths, rel)
	}
	return paths
}

// reconcileOpen re-reads every open document except skip and pushes changed
// snapshots; deleted or unreadable files are closed. This catches bash and
// external edits without a recursive watcher.
func (s *server) reconcileOpen(ctx context.Context, skip string) error {
	for _, rel := range s.openPaths() {
		if rel == skip {
			continue
		}
		if err := s.reconcilePath(ctx, rel); err != nil {
			return err
		}
	}
	return nil
}

// reconcilePath brings one open document up to date with disk.
func (s *server) reconcilePath(ctx context.Context, rel string) error {
	record := s.openDocument(rel)
	if record == nil {
		return nil
	}
	doc, err := s.m.ws.ReadFile(ctx, rel)
	if ctx.Err() != nil {
		return asError(ctx.Err())
	}
	if err != nil || doc.Path != rel || validateDocument(doc) != nil {
		return s.didClose(ctx, rel)
	}
	_, err = s.syncDocument(ctx, doc, record.languageID)
	var lspErr *Error
	if errors.As(err, &lspErr) && (lspErr.Code == CodeOutputLimit || lspErr.Code == CodeDocumentLimit) {
		// A background document that no longer fits is closed rather than
		// failing every later query.
		return s.didClose(ctx, rel)
	}
	return err
}
