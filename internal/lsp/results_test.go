package lsp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// locationList extracts [path,line,column,end_line,end_column] tuples.
func locationList(t *testing.T, value map[string]any) []string {
	t.Helper()
	var out []string
	for _, item := range value["locations"].([]any) {
		l := item.(map[string]any)
		entry := fmt.Sprintf("%s:%v:%v-%v:%v", l["path"], l["line"], l["column"], l["end_line"], l["end_column"])
		if target, ok := l["target_range"].(map[string]any); ok {
			entry += fmt.Sprintf(" target %v:%v-%v:%v", target["line"], target["column"], target["end_line"], target["end_column"])
		}
		out = append(out, entry)
	}
	return out
}

func counters(value map[string]any) (total, omitted, external int, truncated bool) {
	return int(value["total"].(float64)), int(value["omitted"].(float64)), int(value["external_locations_omitted"].(float64)), value["truncated"].(bool)
}

func TestLSPUnicodeAndContainment(t *testing.T) {
	t.Run("one-based scalar positions round-trip through the server", func(t *testing.T) {
		for name, test := range map[string]struct {
			source string
			query  Position
			want   string
		}{
			"ascii":           {"tag := 1\nx := tag\n", Position{2, 6}, "a.go:1:1-1:4"},
			"emoji":           {"/* 😀😀 */ tag\nx 😀 tag\n", Position{2, 5}, "a.go:1:10-1:13"},
			"BOM":             {"\ufeff😀 tag\ntag\n", Position{2, 1}, "a.go:1:3-1:6"},
			"CRLF":            {"a\r\n\r\n\t😀 tag\r\ntag\r\n", Position{4, 2}, "a.go:3:4-3:7"},
			"combining marks": {"e\u0301\u0301 tag\n tag", Position{2, 3}, "a.go:1:5-1:8"},
			"tabs":            {"\t\ttag\ntag\n", Position{2, 3}, "a.go:1:3-1:6"},
			"EOF":             {"x tag\n\ttag", Position{2, 5}, "a.go:1:3-1:6"},
		} {
			t.Run(name, func(t *testing.T) {
				h := startHarness(t, setup{})
				h.ws.write(t, "a.go", test.source)
				value := h.mustQuery(definitionAt("a.go", test.query.Line, test.query.Column))
				if got := locationList(t, value); !reflect.DeepEqual(got, []string{test.want}) {
					t.Fatalf("locations %v, want %s", got, test.want)
				}
			})
		}
	})
	t.Run("location links keep target_range", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"location_links": true}})
		h.ws.write(t, "a.go", "x 😀 tag()\ny := tag\n")
		value := h.mustQuery(definitionAt("a.go", 2, 6))
		if got := locationList(t, value); !reflect.DeepEqual(got, []string{"a.go:1:5-1:8 target 1:1-1:10"}) {
			t.Fatalf("link: %v", got)
		}
	})
	t.Run("external, uncontained and non-file URIs never become host reads", func(t *testing.T) {
		outside := t.TempDir()
		secretPath := filepath.Join(outside, "secret.go")
		if err := os.WriteFile(secretPath, []byte("package secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ws := newDirWorkspace(t)
		if err := os.Symlink(secretPath, filepath.Join(ws.root, "link.go")); err != nil {
			t.Fatal(err)
		}
		r := `{"start":{"line":0,"character":0},"end":{"line":0,"character":3}}`
		uris := []string{
			"file://" + secretPath,
			"file://evil.example" + ws.root + "/a.go",
			"${ROOT}/a.go?x=1",
			"${ROOT}/a.go#frag",
			"${ROOT}/../" + filepath.Base(outside) + "/secret.go",
			"https://example.com/a.go",
			"untitled:Untitled-1",
			"${ROOT}/link.go",
			"${ROOT}/a.go",
		}
		var items []string
		for _, uri := range uris {
			items = append(items, fmt.Sprintf(`{"uri":%q,"range":%s}`, uri, r))
		}
		h := startHarness(t, setup{ws: ws, fixture: map[string]any{"responses": map[string]any{"textDocument/references": json.RawMessage("[" + strings.Join(items, ",") + "]")}}})
		h.ws.write(t, "a.go", "tag\n")
		value := h.mustQuery(Query{Method: MethodReferences, Path: "a.go", Position: Position{1, 1}})
		total, omitted, external, _ := counters(value)
		if got := locationList(t, value); !reflect.DeepEqual(got, []string{"a.go:1:1-1:4"}) {
			t.Fatalf("locations %v", got)
		}
		// link.go is lexically inside but the contained read refuses its target.
		if total != len(uris) || external != len(uris)-2 || omitted != 1 {
			t.Fatalf("total %d omitted %d external %d", total, omitted, external)
		}
		for _, read := range ws.reads {
			if strings.Contains(read, "secret") || strings.HasPrefix(read, "..") || filepath.IsAbs(read) {
				t.Fatalf("host read through a server URI: %s", read)
			}
		}
	})
}

func TestLSPResultsAndBounds(t *testing.T) {
	r := func(line, start, end int) string {
		return fmt.Sprintf(`{"start":{"line":%d,"character":%d},"end":{"line":%d,"character":%d}}`, line, start, line, end)
	}
	t.Run("normalization of every definition shape", func(t *testing.T) {
		for name, test := range map[string]struct {
			response string
			want     []string
			total    int
		}{
			"null":   {`null`, nil, 0},
			"single": {`{"uri":"${ROOT}/a.go","range":` + r(1, 0, 3) + `}`, []string{"a.go:2:1-2:4"}, 1},
			"array sorted and deduplicated": {`[{"uri":"${ROOT}/b.go","range":` + r(0, 0, 1) + `},{"uri":"${ROOT}/a.go","range":` + r(1, 0, 3) + `},{"uri":"${ROOT}/a.go","range":` + r(0, 1, 2) + `},{"uri":"${ROOT}/a.go","range":` + r(1, 0, 3) + `}]`,
				[]string{"a.go:1:2-1:3", "a.go:2:1-2:4", "b.go:1:1-1:2"}, 4},
			"links":                    {`[{"targetUri":"${ROOT}/a.go","targetRange":` + r(1, 0, 4) + `,"targetSelectionRange":` + r(1, 1, 2) + `}]`, []string{"a.go:2:2-2:3 target 2:1-2:5"}, 1},
			"invalid position omitted": {`[{"uri":"${ROOT}/a.go","range":` + r(0, 0, 99) + `},{"uri":"${ROOT}/a.go","range":` + r(1, 0, 1) + `}]`, []string{"a.go:2:1-2:2"}, 2},
		} {
			t.Run(name, func(t *testing.T) {
				h := startHarness(t, setup{fixture: map[string]any{"responses": map[string]any{"textDocument/definition": json.RawMessage(test.response)}}})
				h.ws.write(t, "a.go", "abcd\nefgh\n")
				h.ws.write(t, "b.go", "x\n")
				value := h.mustQuery(definitionAt("a.go", 1, 1))
				got := locationList(t, value)
				total, omitted, external, _ := counters(value)
				if !reflect.DeepEqual(got, test.want) || total != test.total || total != len(got)+omitted+external {
					t.Fatalf("got %v total %d omitted %d external %d", got, total, omitted, external)
				}
			})
		}
	})
	t.Run("max_results and the encoded budget drop trailing entries only", func(t *testing.T) {
		name := `q"uo\te` + "\x01" + `.go`
		ws := newDirWorkspace(t)
		ws.write(t, name, strings.Repeat("x\n", 600))
		var items []string
		for line := range 600 {
			items = append(items, fmt.Sprintf(`{"uri":"${ROOT}/%s","range":%s}`, "q%22uo%5Cte%01.go", r(line, 0, 1)))
		}
		h := startHarness(t, setup{ws: ws, fixture: map[string]any{"responses": map[string]any{"textDocument/references": json.RawMessage("[" + strings.Join(items, ",") + "]")}}})
		h.ws.write(t, "a.go", "tag\n")
		result, err := h.m.Query(t.Context(), Query{Method: MethodReferences, Path: "a.go", Position: Position{1, 1}, MaxResults: 200})
		if err != nil {
			t.Fatal(err)
		}
		if cost := encodedCost(result.Value); cost > maxResultBytes {
			t.Fatalf("cost %d over budget", cost)
		}
		var value map[string]any
		_ = json.Unmarshal(result.Value, &value)
		got := locationList(t, value)
		total, omitted, external, truncated := counters(value)
		if !truncated || total != 600 || external != 0 || omitted != 600-len(got) || len(got) == 0 || len(got) > 200 {
			t.Fatalf("returned %d total %d omitted %d truncated %v", len(got), total, omitted, truncated)
		}
		for i, entry := range got {
			if want := fmt.Sprintf("%s:%d:1-%d:2", name, i+1, i+1); entry != want {
				t.Fatalf("entry %d = %q, want %q: trailing entries only may be dropped", i, entry, want)
			}
		}
		small := h.mustQuery(Query{Method: MethodReferences, Path: "a.go", Position: Position{1, 1}, MaxResults: 3})
		if got := locationList(t, small); len(got) != 3 || !small["truncated"].(bool) || small["omitted"].(float64) != 597 {
			t.Fatalf("max_results: %v", small)
		}
	})
	t.Run("decoded location cap", func(t *testing.T) {
		items := strings.TrimSuffix(strings.Repeat(`{"uri":"${ROOT}/a.go","range":`+r(0, 0, 1)+`},`, maxDecodedLocations+1), ",")
		h := startHarness(t, setup{fixture: map[string]any{"responses": map[string]any{"textDocument/references": json.RawMessage("[" + items + "]")}}})
		h.ws.write(t, "a.go", "tag\n")
		_, err := h.query(Query{Method: MethodReferences, Path: "a.go", Position: Position{1, 1}})
		wantCode(t, err, CodeOutputLimit)
	})
	t.Run("server errors are redacted and bounded", func(t *testing.T) {
		message := "failed with token=hunter2-secret " + strings.Repeat("y", 5000)
		h := startHarness(t, setup{
			fixture:  map[string]any{"errors": map[string]any{"textDocument/definition": map[string]any{"code": -32000, "message": message}}},
			settings: `{"apiKey":"settings-secret"}`,
		})
		h.ws.write(t, "a.go", "tag\n")
		_, err := h.query(definitionAt("a.go", 1, 1))
		wantCode(t, err, CodeServerError)
		if text := err.Error(); strings.Contains(text, "hunter2") || strings.Contains(text, "settings-secret") || len(text) > 1024 {
			t.Fatalf("leaky or unbounded error (%d bytes): %.200s", len(text), text)
		}
	})
	t.Run("content modified and cancelled server errors keep their meaning", func(t *testing.T) {
		h := startHarness(t, setup{fixture: map[string]any{"errors": map[string]any{
			"textDocument/definition": map[string]any{"code": -32801, "message": "modified"},
			"textDocument/references": map[string]any{"code": -32800, "message": "cancelled"},
		}}})
		h.ws.write(t, "a.go", "tag\n")
		_, err := h.query(definitionAt("a.go", 1, 1))
		wantCode(t, err, CodeContentModified)
		_, err = h.query(Query{Method: MethodReferences, Path: "a.go", Position: Position{1, 1}})
		wantCode(t, err, CodeCancelled)
		if h.count("go", "textDocument/definition") != 1 {
			t.Fatal("ContentModified must not be retried automatically")
		}
	})
}

func TestDiagnosticCacheBounds(t *testing.T) {
	index := newLineIndex([]byte("abc\n"))
	var raws []json.RawMessage
	for i := range maxDiagnosticsPerURI + 500 {
		severity := 1 + i%2
		raws = append(raws, json.RawMessage(fmt.Sprintf(`{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"severity":%d,"message":"m"}`, severity)))
	}
	raws = append(raws, json.RawMessage(`{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":99}},"severity":4,"message":"bad range"}`))
	entry := normalizeDiagnostics(raws, index)
	if len(entry.items) != maxDiagnosticsPerURI || entry.dropped["error"]+entry.dropped["warning"] != 500 || entry.dropped["hint"] != 1 {
		t.Fatalf("kept %d dropped %v", len(entry.items), entry.dropped)
	}
	long := normalizeDiagnostics([]json.RawMessage{json.RawMessage(`{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"message":"` + strings.Repeat("é", 3000) + `","code":{"bad":1},"source":"s"}`)}, index)
	item := long.items[0]
	if len(item.Message) > maxDiagnosticMessage || !strings.HasSuffix(item.Message, "…[truncated]") || item.Severity != "information" || item.Code != nil {
		t.Fatalf("bounded message: %d bytes, %q severity %s code %s", len(item.Message), item.Message[len(item.Message)-20:], item.Severity, item.Code)
	}
	codes := normalizeDiagnostics([]json.RawMessage{
		json.RawMessage(`{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"message":"a","code":42}`),
		json.RawMessage(`{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":2}},"message":"b","code":"E1"}`),
	}, index)
	if string(codes.items[0].Code) != "42" || string(codes.items[1].Code) != `"E1"` {
		t.Fatalf("codes %s %s", codes.items[0].Code, codes.items[1].Code)
	}
	// The server-wide budget evicts least recently used documents' entries.
	var state diagnosticsState
	state.init()
	docs := map[string]*openDoc{}
	for i := range 20 {
		rel := fmt.Sprintf("f%d.go", i)
		docs[rel] = &openDoc{lastUse: uint64(i)}
		state.store(state.push, rel, &diagEntry{size: maxDiagnosticURIBytes}, docs)
	}
	if state.bytes > maxDiagnosticBytes || state.push["f19.go"] == nil || state.push["f0.go"] != nil {
		t.Fatalf("budget %d, entries %d", state.bytes, len(state.push))
	}
}
