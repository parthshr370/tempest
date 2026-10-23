package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

const (
	maxDiagnosticsPerURI  = 1000
	maxDiagnosticURIBytes = 256 << 10
	maxDiagnosticBytes    = 4 << 20
	maxDiagnosticMessage  = 2048
	maxDiagnosticField    = 256
	// diagnosticItemOverhead approximates the encoded size of a diagnostic's
	// positions, severity and keys.
	diagnosticItemOverhead = 96
)

// Freshness values for diagnostics results.
const (
	freshCurrent     = "current"
	freshUnversioned = "unversioned"
	freshPending     = "pending"
	freshOutdated    = "outdated"
)

var severityNames = map[int]string{1: "error", 2: "warning", 3: "information", 4: "hint"}

type diagnosticOut struct {
	Line      int             `json:"line"`
	Column    int             `json:"column"`
	EndLine   int             `json:"end_line"`
	EndColumn int             `json:"end_column"`
	Severity  string          `json:"severity"`
	Code      json.RawMessage `json:"code,omitempty"`
	Source    string          `json:"source,omitempty"`
	Message   string          `json:"message"`
}

// diagEntry is one URI's cached, already normalized diagnostics.
type diagEntry struct {
	versioned  bool
	version    int64 // publication version, when versioned
	docVersion int32 // open document version when the entry was stored
	epoch      uint64
	items      []diagnosticOut
	dropped    map[string]int // diagnostics not cached, by severity
	size       int
	resultID   string
	stale      bool
}

type diagnosticsState struct {
	push  map[string]*diagEntry
	pull  map[string]*diagEntry
	bytes int
	// epoch moves when a file the server isn't told about changes, making
	// earlier publications uncertain.
	epoch  uint64
	signal chan struct{}
}

func (d *diagnosticsState) init() {
	d.push = map[string]*diagEntry{}
	d.pull = map[string]*diagEntry{}
	d.bytes = 0
	d.signal = make(chan struct{})
}

func (d *diagnosticsState) forget(rel string) {
	for _, cache := range []map[string]*diagEntry{d.push, d.pull} {
		if entry := cache[rel]; entry != nil {
			d.bytes -= entry.size
			delete(cache, rel)
		}
	}
}

func (d *diagnosticsState) wake() {
	close(d.signal)
	d.signal = make(chan struct{})
}

// store replaces rel's entry, evicting least-recently-used documents' entries
// until the server-wide budget fits.
func (d *diagnosticsState) store(cache map[string]*diagEntry, rel string, entry *diagEntry, docs map[string]*openDoc) {
	if old := cache[rel]; old != nil {
		d.bytes -= old.size
		delete(cache, rel)
	}
	for d.bytes+entry.size > maxDiagnosticBytes {
		victim, victimUse := "", uint64(0)
		for _, other := range []map[string]*diagEntry{d.push, d.pull} {
			for path := range other {
				use := uint64(0)
				if doc := docs[path]; doc != nil {
					use = doc.lastUse
				}
				if path != rel && (victim == "" || use < victimUse) {
					victim, victimUse = path, use
				}
			}
		}
		if victim == "" {
			break
		}
		d.forget(victim)
	}
	cache[rel] = entry
	d.bytes += entry.size
}

// markUncertain downgrades earlier publications after an unannounced change.
func (s *server) markUncertain() {
	s.mu.Lock()
	s.diags.epoch++
	s.diags.wake()
	s.mu.Unlock()
}

type rawDiagnostic struct {
	Range    *lspRange       `json:"range"`
	Severity *int            `json:"severity"`
	Code     json.RawMessage `json:"code"`
	Source   string          `json:"source"`
	Message  *string         `json:"message"`
}

// normalizeDiagnostics converts and bounds raw diagnostics against the text
// they were reported for. Invalid ranges and entries beyond the cache bounds
// are counted as dropped, never silently lost.
func normalizeDiagnostics(raws []json.RawMessage, index *lineIndex) *diagEntry {
	entry := &diagEntry{dropped: map[string]int{}}
	for _, raw := range raws {
		var d rawDiagnostic
		if json.Unmarshal(raw, &d) != nil || d.Range == nil || d.Message == nil {
			entry.dropped["information"]++
			continue
		}
		severity := "information"
		if d.Severity != nil {
			if name, ok := severityNames[*d.Severity]; ok {
				severity = name
			}
		}
		if len(entry.items) >= maxDiagnosticsPerURI || entry.size >= maxDiagnosticURIBytes {
			entry.dropped[severity]++
			continue
		}
		start, end, err := index.modelRange(*d.Range)
		if err != nil {
			entry.dropped[severity]++
			continue
		}
		out := diagnosticOut{Line: start.Line, Column: start.Column, EndLine: end.Line, EndColumn: end.Column, Severity: severity, Code: diagnosticCode(d.Code), Source: boundText(d.Source, maxDiagnosticField), Message: boundMessage(*d.Message)}
		entry.items = append(entry.items, out)
		entry.size += len(out.Message) + len(out.Source) + len(out.Code) + diagnosticItemOverhead
	}
	sort.SliceStable(entry.items, func(i, j int) bool {
		a, b := entry.items[i], entry.items[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Column != b.Column {
			return a.Column < b.Column
		}
		if a.EndLine != b.EndLine {
			return a.EndLine < b.EndLine
		}
		return a.EndColumn < b.EndColumn
	})
	return entry
}

// diagnosticCode preserves an explicit integer or bounded string code.
func diagnosticCode(raw json.RawMessage) json.RawMessage {
	var number int64
	if json.Unmarshal(raw, &number) == nil {
		return json.RawMessage(fmt.Sprint(number))
	}
	var text string
	if json.Unmarshal(raw, &text) == nil && len(text) <= maxDiagnosticField {
		encoded, _ := json.Marshal(text)
		return encoded
	}
	return nil
}

func boundMessage(message string) string {
	const marker = "[truncated]"
	if len(message) <= maxDiagnosticMessage {
		return message
	}
	// boundText appends "…" when it cuts.
	return boundText(message, maxDiagnosticMessage-len("…")-len(marker)) + marker
}

// onPublish applies textDocument/publishDiagnostics in read order on the
// reader. A future version is a protocol error that retires the server.
func (s *server) onPublish(params json.RawMessage) *Error {
	var publication struct {
		URI         string            `json:"uri"`
		Version     *int64            `json:"version"`
		Diagnostics []json.RawMessage `json:"diagnostics"`
	}
	if json.Unmarshal(params, &publication) != nil || publication.Diagnostics == nil {
		return newError(CodeProtocol, "malformed publishDiagnostics notification")
	}
	rel, ok := uriPath(s.m.root, publication.URI)
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc := s.docs[rel]
	if doc == nil {
		return nil
	}
	if publication.Version != nil {
		switch {
		case *publication.Version < int64(doc.version):
			return nil
		case *publication.Version > int64(doc.version):
			return newError(CodeProtocol, fmt.Sprintf("diagnostics published for future version %d of %s (open version %d)", *publication.Version, rel, doc.version))
		}
	}
	entry := normalizeDiagnostics(publication.Diagnostics, doc.index)
	entry.versioned = publication.Version != nil
	if entry.versioned {
		entry.version = *publication.Version
	}
	entry.docVersion = doc.version
	entry.epoch = s.diags.epoch
	s.diags.store(s.diags.push, rel, entry, s.docs)
	s.diags.wake()
	return nil
}

// onRefresh invalidates pull result IDs.
func (s *server) onRefresh() {
	s.mu.Lock()
	for _, entry := range s.diags.pull {
		entry.stale = true
		entry.resultID = ""
	}
	s.diags.wake()
	s.mu.Unlock()
}

func pushFreshness(entry *diagEntry, version int32, epoch uint64) string {
	switch {
	case entry == nil:
		return freshPending
	case entry.epoch != epoch:
		return freshOutdated
	case entry.versioned && entry.version == int64(version):
		return freshCurrent
	case !entry.versioned && entry.docVersion == version:
		return freshUnversioned
	}
	return freshOutdated
}

// diagnostics answers an explicit diagnostics query for an already
// synchronized document. No publication means pending, never zero errors.
func (s *server) diagnostics(ctx context.Context, q Query, record *openDoc) (*diagEntry, string, error) {
	if s.caps.diagnostic {
		return s.pullDiagnostics(ctx, q, record)
	}
	return s.pushDiagnostics(ctx, q, record)
}

// pushDiagnostics waits up to q.Wait for a publication that matches the
// captured version. A quiet interval is never treated as proof.
func (s *server) pushDiagnostics(ctx context.Context, q Query, record *openDoc) (*diagEntry, string, error) {
	timer := time.NewTimer(q.Wait)
	defer timer.Stop()
	for {
		s.mu.Lock()
		entry := s.diags.push[record.path]
		freshness := pushFreshness(entry, record.version, s.diags.epoch)
		signal := s.diags.signal
		s.mu.Unlock()
		if freshness == freshCurrent || freshness == freshUnversioned || q.Wait == 0 {
			return entry, freshness, nil
		}
		select {
		case <-signal:
		case <-timer.C:
			return entry, freshness, nil
		case <-s.gone:
			return nil, "", s.lostError()
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return entry, freshness, nil
			}
			return nil, "", asError(ctx.Err())
		}
	}
}

// pullDiagnostics sends textDocument/diagnostic within q.Wait. On timeout the
// cache is returned as not fresh; with Wait zero no request is sent.
func (s *server) pullDiagnostics(ctx context.Context, q Query, record *openDoc) (*diagEntry, string, error) {
	pulled := false
	if q.Wait > 0 {
		s.mu.Lock()
		previous := s.diags.pull[record.path]
		previousID := ""
		if previous != nil && !previous.stale {
			previousID = previous.resultID
		}
		s.mu.Unlock()
		params := map[string]any{"textDocument": textDocumentID{URI: record.uri}}
		if s.caps.diagIdentifier != "" {
			params["identifier"] = s.caps.diagIdentifier
		}
		if previousID != "" {
			params["previousResultId"] = previousID
		}
		waitCtx, cancel := context.WithTimeout(ctx, q.Wait)
		raw, err := s.call(waitCtx, "textDocument/diagnostic", params)
		cancel()
		switch {
		case err == nil:
			if err = s.storePull(record, raw, previousID); err != nil {
				return nil, "", err
			}
			pulled = true
		case ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded):
			return nil, "", asError(ctx.Err())
		case waitCtx.Err() == nil:
			return nil, "", err
		}
	}
	s.mu.Lock()
	entry := s.diags.pull[record.path]
	s.mu.Unlock()
	switch {
	case entry == nil:
		return nil, freshPending, nil
	case pulled && entry.docVersion == record.version:
		return entry, freshCurrent, nil
	}
	return entry, freshOutdated, nil
}

// storePull caches a full or unchanged pull report. An unchanged report is
// valid only against the result ID this client retained and sent.
func (s *server) storePull(record *openDoc, raw json.RawMessage, previousID string) error {
	var report struct {
		Kind     string            `json:"kind"`
		ResultID string            `json:"resultId"`
		Items    []json.RawMessage `json:"items"`
	}
	if json.Unmarshal(raw, &report) != nil {
		return newError(CodeProtocol, "malformed textDocument/diagnostic report")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var entry *diagEntry
	switch report.Kind {
	case "full":
		if report.Items == nil {
			return newError(CodeProtocol, "full diagnostic report has no items")
		}
		entry = normalizeDiagnostics(report.Items, record.index)
	case "unchanged":
		previous := s.diags.pull[record.path]
		if previousID == "" || previous == nil || previous.resultID != previousID {
			s.diags.forget(record.path)
			return newError(CodeProtocol, "unchanged diagnostic report without a retained matching resultId")
		}
		entry = &diagEntry{items: previous.items, dropped: previous.dropped, size: previous.size}
	default:
		return newError(CodeProtocol, "unknown diagnostic report kind")
	}
	// An oversized result ID isn't retained: a cut ID could never match.
	if len(report.ResultID) <= maxDiagnosticField {
		entry.resultID = report.ResultID
	}
	entry.docVersion = record.version
	entry.epoch = s.diags.epoch
	entry.versioned = true
	entry.version = int64(record.version)
	s.diags.store(s.diags.pull, record.path, entry, s.docs)
	return nil
}

type diagnosticsResult struct {
	Server      string          `json:"server"`
	Path        string          `json:"path"`
	Version     int32           `json:"version"`
	Anchor      string          `json:"anchor"`
	Fresh       bool            `json:"fresh"`
	Freshness   string          `json:"freshness"`
	Diagnostics []diagnosticOut `json:"diagnostics"`
	Total       int             `json:"total"`
	Omitted     int             `json:"omitted"`
	Truncated   bool            `json:"truncated"`
}

// diagnosticsValue filters by exact severity and fits the result budget.
func diagnosticsValue(server string, record *openDoc, entry *diagEntry, freshness, severity string, maxResults int) (json.RawMessage, error) {
	var matching []diagnosticOut
	dropped := 0
	if entry != nil {
		for _, item := range entry.items {
			if severity == "all" || item.Severity == severity {
				matching = append(matching, item)
			}
		}
		for name, count := range entry.dropped {
			if severity == "all" || name == severity {
				dropped += count
			}
		}
	}
	total := len(matching) + dropped
	limit := min(len(matching), maxResults)
	return fitResult(limit, maxResultBytes, func(n int) ([]byte, error) {
		items := matching[:n]
		if items == nil {
			items = []diagnosticOut{}
		}
		return json.Marshal(diagnosticsResult{
			Server: server, Path: record.path, Version: record.version, Anchor: record.anchor,
			Fresh: freshness == freshCurrent, Freshness: freshness,
			Diagnostics: items, Total: total, Omitted: total - n, Truncated: total > n,
		})
	})
}
