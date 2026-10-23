// Command server is a scriptable fake language server for internal/lsp tests.
// It speaks the LSP base protocol on stdio with its own framing and UTF-16
// arithmetic, records every inbound message in a JSON-lines ledger before
// dispatch, and derives definitions, references, renames and diagnostics from
// document text so tests exercise real round trips.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

type errorReply struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type config struct {
	SyncKind             int                        `json:"sync_kind"`
	SyncNumber           bool                       `json:"sync_number"`
	NoOpenClose          bool                       `json:"no_open_close"`
	Save                 json.RawMessage            `json:"save"`
	PositionEncoding     string                     `json:"position_encoding"`
	NoDefinition         bool                       `json:"no_definition"`
	NoReferences         bool                       `json:"no_references"`
	NoRename             bool                       `json:"no_rename"`
	PullDiagnostics      bool                       `json:"pull_diagnostics"`
	DiagnosticIdentifier string                     `json:"diagnostic_identifier"`
	Push                 string                     `json:"push"`
	PushDelayMS          int                        `json:"push_delay_ms"`
	LocationLinks        bool                       `json:"location_links"`
	RenameForm           string                     `json:"rename_form"`
	Responses            map[string]json.RawMessage `json:"responses"`
	Errors               map[string]errorReply      `json:"errors"`
	DelayMS              map[string]int             `json:"delay_ms"`
	Hang                 []string                   `json:"hang"`
	ExitOn               string                     `json:"exit_on"`
	MalformedOn          string                     `json:"malformed_on"`
	OversizeOn           string                     `json:"oversize_on"`
	InitDelayMS          int                        `json:"init_delay_ms"`
	ShutdownHang         bool                       `json:"shutdown_hang"`
	StderrFloodBytes     int                        `json:"stderr_flood_bytes"`
	StderrText           string                     `json:"stderr_text"`
	SpawnChild           string                     `json:"spawn_child"`
	StopReadingOn        string                     `json:"stop_reading_on"`
	ConfigSections       []string                   `json:"config_sections"`
	ProbeClient          bool                       `json:"probe_client"`
}

type document struct {
	text    string
	version int
}

type server struct {
	cfg     config
	ledger  *os.File
	ledgerM sync.Mutex
	out     *bufio.Writer
	outM    sync.Mutex
	mu      sync.Mutex
	root    string // root URI
	rootDir string
	docs    map[string]*document
	nextID  int
}

func main() {
	configPath := flag.String("config", "", "fixture config JSON file")
	ledgerPath := flag.String("ledger", "", "ledger file")
	sleep := flag.Duration("sleep", 0, "sleep then exit (descendant mode)")
	flag.Parse()
	if *sleep > 0 {
		time.Sleep(*sleep)
		return
	}
	s := &server{docs: map[string]*document{}, out: bufio.NewWriter(os.Stdout)}
	if *configPath != "" {
		raw, err := os.ReadFile(*configPath)
		if err != nil || json.Unmarshal(raw, &s.cfg) != nil {
			fmt.Fprintln(os.Stderr, "bad fixture config")
			os.Exit(2)
		}
	}
	if s.cfg.SyncKind == 0 {
		s.cfg.SyncKind = 1
	}
	if s.cfg.Push == "" {
		s.cfg.Push = "versioned"
	}
	ledger, err := os.OpenFile(*ledgerPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(2)
	}
	s.ledger = ledger
	s.record(map[string]any{"event": "start", "pid": os.Getpid()})
	if s.cfg.StderrText != "" {
		fmt.Fprint(os.Stderr, s.cfg.StderrText)
	}
	if s.cfg.StderrFloodBytes > 0 {
		go func() {
			chunk := []byte(strings.Repeat("flood ", 1024))
			for written := 0; written < s.cfg.StderrFloodBytes; written += len(chunk) {
				if _, err := os.Stderr.Write(chunk); err != nil {
					return
				}
			}
		}()
	}
	if s.cfg.SpawnChild != "" {
		child := exec.Command(os.Args[0], "-sleep", "60s")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if s.cfg.SpawnChild == "setsid" {
			child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		}
		if child.Start() == nil {
			s.record(map[string]any{"event": "child", "pid": child.Process.Pid})
		}
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		body, err := readFrame(reader)
		if err != nil {
			s.record(map[string]any{"event": "eof"})
			os.Exit(0)
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(body, &m) != nil {
			s.record(map[string]any{"event": "bad_frame"})
			continue
		}
		if m.Method == "" {
			s.record(map[string]any{"event": "response", "id": m.ID, "result": m.Result, "error": m.Error})
			continue
		}
		s.record(map[string]any{"event": "recv", "method": m.Method, "id": m.ID, "params": m.Params})
		if m.Method == s.cfg.StopReadingOn {
			time.Sleep(time.Hour)
		}
		s.dispatch(m.ID, m.Method, m.Params)
	}
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, err
			}
		}
	}
	if length < 0 {
		return nil, io.ErrUnexpectedEOF
	}
	body := make([]byte, length)
	_, err := io.ReadFull(r, body)
	return body, err
}

func (s *server) record(event map[string]any) {
	line, _ := json.Marshal(event)
	s.ledgerM.Lock()
	_, _ = s.ledger.Write(append(line, '\n'))
	s.ledgerM.Unlock()
}

func (s *server) writeRaw(b []byte) {
	s.outM.Lock()
	defer s.outM.Unlock()
	_, _ = s.out.Write(b)
	_ = s.out.Flush()
}

func (s *server) send(message map[string]any) {
	message["jsonrpc"] = "2.0"
	body, _ := json.Marshal(message)
	s.writeRaw([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)))
}

func (s *server) reply(id json.RawMessage, result any) {
	s.send(map[string]any{"id": id, "result": result})
}

func (s *server) request(method string, params any) {
	s.mu.Lock()
	s.nextID++
	id := fmt.Sprintf("srv-%d", s.nextID)
	s.mu.Unlock()
	message := map[string]any{"id": id, "method": method}
	if params != nil {
		message["params"] = params
	}
	s.send(message)
}

func (s *server) dispatch(id json.RawMessage, method string, params json.RawMessage) {
	if method == s.cfg.ExitOn {
		os.Exit(3)
	}
	if method == s.cfg.MalformedOn {
		s.writeRaw([]byte("Content-Length: abc\r\n\r\n{}"))
		return
	}
	if method == s.cfg.OversizeOn {
		s.writeRaw([]byte("Content-Length: 99999999\r\n\r\n{"))
		return
	}
	switch method {
	case "$/cancelRequest":
		var p struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal(params, &p) == nil {
			cancelledIDs.Store(string(p.ID), true)
		}
		return
	case "initialize":
		s.initialize(id, params)
		return
	case "initialized":
		s.afterInitialized()
		return
	case "shutdown":
		if !s.cfg.ShutdownHang {
			s.reply(id, nil)
		}
		return
	case "exit":
		s.record(map[string]any{"event": "exit"})
		os.Exit(0)
	case "textDocument/didOpen":
		s.didOpen(params)
		return
	case "textDocument/didChange":
		s.didChange(params)
		return
	case "textDocument/didClose":
		var p struct {
			TextDocument struct{ URI string } `json:"textDocument"`
		}
		_ = json.Unmarshal(params, &p)
		s.mu.Lock()
		delete(s.docs, p.TextDocument.URI)
		s.mu.Unlock()
		return
	}
	if len(id) == 0 {
		return
	}
	hang := false
	for _, h := range s.cfg.Hang {
		hang = hang || h == method
	}
	delay := time.Duration(s.cfg.DelayMS[method]) * time.Millisecond
	if hang {
		// Answer only after the client cancels, proving it tolerates a late result.
		go func() {
			for !s.cancelled(id) {
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(20 * time.Millisecond)
			s.answer(id, method, params)
		}()
		return
	}
	if delay > 0 {
		go func() {
			time.Sleep(delay)
			s.answer(id, method, params)
		}()
		return
	}
	s.answer(id, method, params)
}

var cancelledIDs sync.Map

func (s *server) cancelled(id json.RawMessage) bool {
	_, ok := cancelledIDs.Load(string(id))
	return ok
}

func (s *server) answer(id json.RawMessage, method string, params json.RawMessage) {
	if e, ok := s.cfg.Errors[method]; ok {
		s.send(map[string]any{"id": id, "error": e})
		return
	}
	if raw, ok := s.cfg.Responses[method]; ok {
		s.mu.Lock()
		root := s.root
		s.mu.Unlock()
		s.reply(id, json.RawMessage(strings.ReplaceAll(string(raw), "${ROOT}", root)))
		return
	}
	switch method {
	case "textDocument/definition":
		s.reply(id, s.definition(params))
	case "textDocument/references":
		s.reply(id, s.references(params))
	case "textDocument/rename":
		s.reply(id, s.rename(params))
	case "textDocument/diagnostic":
		s.reply(id, s.pull(params))
	default:
		s.send(map[string]any{"id": id, "error": errorReply{Code: -32601, Message: "fixture: unknown method"}})
	}
}

func (s *server) initialize(id json.RawMessage, params json.RawMessage) {
	var p struct {
		RootURI string `json:"rootUri"`
	}
	_ = json.Unmarshal(params, &p)
	s.mu.Lock()
	s.root = p.RootURI
	if u, err := url.Parse(p.RootURI); err == nil {
		s.rootDir = u.Path
	}
	s.mu.Unlock()
	if s.cfg.InitDelayMS > 0 {
		time.Sleep(time.Duration(s.cfg.InitDelayMS) * time.Millisecond)
	}
	caps := map[string]any{}
	if s.cfg.SyncNumber {
		caps["textDocumentSync"] = s.cfg.SyncKind
	} else {
		sync := map[string]any{"openClose": !s.cfg.NoOpenClose, "change": s.cfg.SyncKind}
		if len(s.cfg.Save) > 0 {
			sync["save"] = s.cfg.Save
		}
		caps["textDocumentSync"] = sync
	}
	if s.cfg.PositionEncoding != "" {
		caps["positionEncoding"] = s.cfg.PositionEncoding
	}
	caps["definitionProvider"] = !s.cfg.NoDefinition
	caps["referencesProvider"] = !s.cfg.NoReferences
	if !s.cfg.NoRename {
		caps["renameProvider"] = map[string]any{"prepareProvider": false}
	}
	if s.cfg.PullDiagnostics {
		provider := map[string]any{"interFileDependencies": false, "workspaceDiagnostics": false}
		if s.cfg.DiagnosticIdentifier != "" {
			provider["identifier"] = s.cfg.DiagnosticIdentifier
		}
		caps["diagnosticProvider"] = provider
	}
	s.reply(id, map[string]any{"capabilities": caps, "serverInfo": map[string]string{"name": "fixture"}})
}

func (s *server) afterInitialized() {
	if len(s.cfg.ConfigSections) > 0 {
		items := []map[string]string{}
		for _, section := range s.cfg.ConfigSections {
			items = append(items, map[string]string{"section": section})
		}
		s.request("workspace/configuration", map[string]any{"items": items})
	}
	if s.cfg.ProbeClient {
		s.request("workspace/applyEdit", map[string]any{"edit": map[string]any{"changes": map[string]any{}}})
		s.request("client/registerCapability", map[string]any{"registrations": []any{map[string]any{"id": "1", "method": "workspace/didChangeWatchedFiles"}}})
		s.request("window/showMessageRequest", map[string]any{"type": 1, "message": "pick", "actions": []any{map[string]string{"title": "run"}}})
		s.request("workspace/executeCommand", map[string]any{"command": "rm"})
		s.request("workspace/workspaceFolders", nil)
	}
}

func (s *server) didOpen(params json.RawMessage) {
	var p struct {
		TextDocument struct {
			URI     string `json:"uri"`
			Version int    `json:"version"`
			Text    string `json:"text"`
		} `json:"textDocument"`
	}
	_ = json.Unmarshal(params, &p)
	s.mu.Lock()
	s.docs[p.TextDocument.URI] = &document{text: p.TextDocument.Text, version: p.TextDocument.Version}
	s.mu.Unlock()
	s.recordText(p.TextDocument.URI)
	s.publish(p.TextDocument.URI)
}

func (s *server) didChange(params json.RawMessage) {
	var p struct {
		TextDocument struct {
			URI     string `json:"uri"`
			Version int    `json:"version"`
		} `json:"textDocument"`
		ContentChanges []struct {
			Range *rng   `json:"range"`
			Text  string `json:"text"`
		} `json:"contentChanges"`
	}
	_ = json.Unmarshal(params, &p)
	s.mu.Lock()
	doc := s.docs[p.TextDocument.URI]
	if doc == nil {
		s.mu.Unlock()
		s.record(map[string]any{"event": "change_unopened"})
		return
	}
	for _, change := range p.ContentChanges {
		if change.Range == nil {
			doc.text = change.Text
			continue
		}
		start, ok1 := offsetOf(doc.text, change.Range.Start)
		end, ok2 := offsetOf(doc.text, change.Range.End)
		if !ok1 || !ok2 {
			s.mu.Unlock()
			s.record(map[string]any{"event": "bad_change_range"})
			return
		}
		doc.text = doc.text[:start] + change.Text + doc.text[end:]
	}
	doc.version = p.TextDocument.Version
	s.mu.Unlock()
	s.recordText(p.TextDocument.URI)
	s.publish(p.TextDocument.URI)
}

func (s *server) recordText(uri string) {
	s.mu.Lock()
	doc := s.docs[uri]
	sum := sha256.Sum256([]byte(doc.text))
	version := doc.version
	s.mu.Unlock()
	s.record(map[string]any{"event": "text", "uri": uri, "version": version, "sha256": hex.EncodeToString(sum[:])})
}

type pos struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type rng struct {
	Start pos `json:"start"`
	End   pos `json:"end"`
}

// lineStarts splits on LF, CRLF and CR, independently of the client.
func lineStarts(text string) (starts, ends []int) {
	starts = []int{0}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' || text[i] == '\r' {
			ends = append(ends, i)
			if text[i] == '\r' && i+1 < len(text) && text[i+1] == '\n' {
				i++
			}
			starts = append(starts, i+1)
		}
	}
	ends = append(ends, len(text))
	return starts, ends
}

func offsetOf(text string, p pos) (int, bool) {
	starts, ends := lineStarts(text)
	if p.Line < 0 || p.Line >= len(starts) {
		return 0, false
	}
	offset, units := starts[p.Line], 0
	for units < p.Character {
		if offset >= ends[p.Line] {
			return 0, false
		}
		r, size := utf8.DecodeRuneInString(text[offset:])
		units++
		if r >= 0x10000 {
			units++
		}
		offset += size
	}
	return offset, units == p.Character
}

func posOf(text string, offset int) pos {
	starts, _ := lineStarts(text)
	line := sort.Search(len(starts), func(i int) bool { return starts[i] > offset }) - 1
	units := 0
	for _, r := range text[starts[line]:offset] {
		units++
		if r >= 0x10000 {
			units++
		}
	}
	return pos{Line: line, Character: units}
}

func isIdent(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// wordAt returns the identifier covering offset.
func wordAt(text string, offset int) string {
	start, end := offset, offset
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:start])
		if !isIdent(r) {
			break
		}
		start -= size
	}
	for end < len(text) {
		r, size := utf8.DecodeRuneInString(text[end:])
		if !isIdent(r) {
			break
		}
		end += size
	}
	return text[start:end]
}

// occurrences finds whole-word matches of word in text.
func occurrences(text, word string) []rng {
	var out []rng
	if word == "" {
		return out
	}
	for i := 0; ; {
		j := strings.Index(text[i:], word)
		if j < 0 {
			return out
		}
		start := i + j
		end := start + len(word)
		before, _ := utf8.DecodeLastRuneInString(text[:start])
		after, _ := utf8.DecodeRuneInString(text[end:])
		if (start == 0 || !isIdent(before)) && (end == len(text) || !isIdent(after)) {
			out = append(out, rng{Start: posOf(text, start), End: posOf(text, end)})
		}
		i = end
	}
}

type target struct {
	TextDoc  struct{ URI string }              `json:"textDocument"`
	Position pos                               `json:"position"`
	NewName  string                            `json:"newName"`
	Context  struct{ IncludeDeclaration bool } `json:"context"`
}

func (s *server) word(params json.RawMessage) (target, string, string, bool) {
	var t target
	_ = json.Unmarshal(params, &t)
	s.mu.Lock()
	doc := s.docs[t.TextDoc.URI]
	s.mu.Unlock()
	if doc == nil {
		return t, "", "", false
	}
	offset, ok := offsetOf(doc.text, t.Position)
	if !ok {
		return t, "", "", false
	}
	return t, doc.text, wordAt(doc.text, offset), true
}

func (s *server) definition(params json.RawMessage) any {
	t, text, word, ok := s.word(params)
	if !ok {
		return nil
	}
	found := occurrences(text, word)
	if len(found) == 0 {
		return nil
	}
	if s.cfg.LocationLinks {
		_, ends := lineStarts(text)
		line := found[0].Start.Line
		whole := rng{Start: pos{Line: line}, End: posOf(text, ends[line])}
		return []any{map[string]any{"targetUri": t.TextDoc.URI, "targetRange": whole, "targetSelectionRange": found[0]}}
	}
	return map[string]any{"uri": t.TextDoc.URI, "range": found[0]}
}

// files maps URIs to current text: open overlays first, then disk files
// under the root with the requested document's extension.
func (s *server) files(ext string) map[string]string {
	s.mu.Lock()
	out := map[string]string{}
	for uri, doc := range s.docs {
		out[uri] = doc.text
	}
	rootDir := s.rootDir
	s.mu.Unlock()
	_ = filepath.WalkDir(rootDir, func(p string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(p) != ext {
			return nil
		}
		uri := (&url.URL{Scheme: "file", Path: p}).String()
		if _, open := out[uri]; !open {
			if raw, err := os.ReadFile(p); err == nil {
				out[uri] = string(raw)
			}
		}
		return nil
	})
	return out
}

func (s *server) references(params json.RawMessage) any {
	t, _, word, ok := s.word(params)
	if !ok {
		return nil
	}
	u, _ := url.Parse(t.TextDoc.URI)
	files := s.files(filepath.Ext(u.Path))
	uris := make([]string, 0, len(files))
	for uri := range files {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	locations := []any{}
	for _, uri := range uris {
		for i, r := range occurrences(files[uri], word) {
			if i == 0 && uri == t.TextDoc.URI && !t.Context.IncludeDeclaration {
				continue
			}
			locations = append(locations, map[string]any{"uri": uri, "range": r})
		}
	}
	return locations
}

func (s *server) rename(params json.RawMessage) any {
	t, _, word, ok := s.word(params)
	if !ok {
		return nil
	}
	u, _ := url.Parse(t.TextDoc.URI)
	files := s.files(filepath.Ext(u.Path))
	uris := make([]string, 0, len(files))
	for uri := range files {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	changes := map[string][]any{}
	var documentChanges []any
	for _, uri := range uris {
		var edits []any
		for _, r := range occurrences(files[uri], word) {
			edits = append(edits, map[string]any{"range": r, "newText": t.NewName})
		}
		if len(edits) == 0 {
			continue
		}
		changes[uri] = edits
		s.mu.Lock()
		var version any
		if doc := s.docs[uri]; doc != nil {
			version = doc.version
		}
		s.mu.Unlock()
		documentChanges = append(documentChanges, map[string]any{"textDocument": map[string]any{"uri": uri, "version": version}, "edits": edits})
	}
	if s.cfg.RenameForm == "changes" {
		return map[string]any{"changes": changes}
	}
	return map[string]any{"documentChanges": documentChanges}
}

// diagnosticsFor reports ERR and WARN tokens.
func diagnosticsFor(text string) []any {
	out := []any{}
	for _, token := range []struct {
		word     string
		severity int
	}{{"ERR", 1}, {"WARN", 2}} {
		for _, r := range occurrences(text, token.word) {
			out = append(out, map[string]any{"range": r, "severity": token.severity, "source": "fixture", "code": "F" + strconv.Itoa(token.severity), "message": token.word + " found"})
		}
	}
	return out
}

func (s *server) publish(uri string) {
	if s.cfg.Push == "none" || s.cfg.PullDiagnostics {
		return
	}
	s.mu.Lock()
	doc := s.docs[uri]
	text, version := doc.text, doc.version
	s.mu.Unlock()
	params := map[string]any{"uri": uri, "diagnostics": diagnosticsFor(text)}
	switch s.cfg.Push {
	case "versioned":
		params["version"] = version
	case "future":
		params["version"] = version + 5
	case "older":
		params["version"] = version - 1
	}
	send := func() { s.send(map[string]any{"method": "textDocument/publishDiagnostics", "params": params}) }
	if s.cfg.PushDelayMS > 0 {
		go func() {
			time.Sleep(time.Duration(s.cfg.PushDelayMS) * time.Millisecond)
			send()
		}()
		return
	}
	send()
}

func (s *server) pull(params json.RawMessage) any {
	var p struct {
		TextDocument     struct{ URI string } `json:"textDocument"`
		PreviousResultID string               `json:"previousResultId"`
	}
	_ = json.Unmarshal(params, &p)
	s.mu.Lock()
	doc := s.docs[p.TextDocument.URI]
	text := ""
	if doc != nil {
		text = doc.text
	}
	s.mu.Unlock()
	sum := sha256.Sum256([]byte(text))
	resultID := hex.EncodeToString(sum[:8])
	if p.PreviousResultID == resultID {
		return map[string]any{"kind": "unchanged", "resultId": resultID}
	}
	return map[string]any{"kind": "full", "resultId": resultID, "items": diagnosticsFor(text)}
}
