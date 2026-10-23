package lsp

import (
	"testing"
)

func TestPositionConversions(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		model Position
		want  lspPosition
	}{
		{"ascii", "abc\ndef", Position{2, 2}, lspPosition{1, 1}},
		{"emoji counts two UTF-16 units", "a😀b", Position{1, 3}, lspPosition{0, 3}},
		{"after two emoji", "😀😀x", Position{1, 3}, lspPosition{0, 4}},
		{"combining mark counts separately", "e\u0301x", Position{1, 3}, lspPosition{0, 2}},
		{"tab is one column", "\tx", Position{1, 2}, lspPosition{0, 1}},
		{"BOM hidden from model but on the wire", "\ufeffabc", Position{1, 2}, lspPosition{0, 2}},
		{"BOM only affects line one", "\ufeffa\nbc", Position{2, 2}, lspPosition{1, 1}},
		{"CRLF is one line ending", "ab\r\ncd", Position{2, 2}, lspPosition{1, 1}},
		{"end of CRLF line", "ab\r\ncd", Position{1, 3}, lspPosition{0, 2}},
		{"lone CR ends a line", "ab\rcd", Position{2, 1}, lspPosition{1, 0}},
		{"empty file", "", Position{1, 1}, lspPosition{0, 0}},
		{"final newline EOF line", "ab\n", Position{2, 1}, lspPosition{1, 0}},
		{"EOF without newline", "ab", Position{1, 3}, lspPosition{0, 2}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ix := newLineIndex([]byte(test.text))
			offset, err := ix.modelOffset(test.model)
			if err != nil {
				t.Fatal(err)
			}
			if got := ix.utf16(offset); got != test.want {
				t.Fatalf("utf16 = %+v, want %+v", got, test.want)
			}
			back, err := ix.offset(test.want)
			if err != nil || back != offset {
				t.Fatalf("offset(%+v) = %d, %v; want %d", test.want, back, err, offset)
			}
			if got := ix.model(offset); got != test.model {
				t.Fatalf("model = %+v, want %+v", got, test.model)
			}
		})
	}
}

func TestPositionRejections(t *testing.T) {
	ix := newLineIndex([]byte("a😀\r\nb\n"))
	for _, p := range []Position{{0, 1}, {1, 0}, {1, 4}, {4, 1}, {3, 2}} {
		if _, err := ix.modelOffset(p); errorCode(err) != CodeInvalidPosition {
			t.Fatalf("model %+v: %v", p, err)
		}
	}
	// End of line is valid, one beyond is not; CRLF never adds columns.
	if _, err := ix.modelOffset(Position{1, 3}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []lspPosition{{0, 2}, {0, 4}, {1, 2}, {3, 0}, {-1, 0}} {
		if _, err := ix.offset(p); errorCode(err) != CodeProtocol {
			t.Fatalf("server %+v: %v", p, err)
		}
	}
	if _, _, err := ix.modelRange(lspRange{Start: lspPosition{1, 1}, End: lspPosition{0, 0}}); errorCode(err) != CodeProtocol {
		t.Fatalf("reversed range: %v", err)
	}
	if got := ix.end(); got != (lspPosition{2, 0}) {
		t.Fatalf("end = %+v", got)
	}
}

func TestURIContainment(t *testing.T) {
	root := "/work/repo"
	cases := map[string]string{
		"file:///work/repo/a.go":               "a.go",
		"file:///work/repo/dir/b%20c.go":       "dir/b c.go",
		"file://localhost/work/repo/a.go":      "a.go",
		"file://LOCALHOST/work/repo/a.go":      "a.go",
		"file:///work/repo/%F0%9F%98%80.go":    "😀.go",
		"file:///work/repo":                    "",
		"file:///work/repo/":                   "",
		"file:///work/repository/a.go":         "",
		"file:///work/repo/../secret.go":       "",
		"file:///work/repo/./a.go":             "",
		"file:///work/repo/a%2F..%2F..%2Fx":    "",
		"file:///work/repo/a.go?query":         "",
		"file:///work/repo/a.go#frag":          "",
		"file://evil/work/repo/a.go":           "",
		"file://user@/work/repo/a.go":          "",
		"file:///work/repo/bad%zz.go":          "",
		"file:///work/repo/nul%00.go":          "",
		"https://example.com/work/repo/a.go":   "",
		"untitled:Untitled-1":                  "",
		"jdt://contents/work/repo/a.go":        "",
		"file:work/repo/a.go":                  "",
		"/work/repo/a.go":                      "",
		"file:///etc/passwd":                   "",
		"file:///work/repo/sub/../../etc.go":   "",
		"file:///work/repo//double.go":         "",
		"FILE:///work/repo/upper-scheme.go":    "upper-scheme.go",
		"file:///work/repo/trailing/slash/":    "",
		"file:///work/repo/dir%2Fencoded.go":   "dir/encoded.go",
		"file:///work/repo/%2E%2E/escape.go":   "",
		"file:///work/repo/space%09tab.go":     "space\ttab.go",
		"file:///work/repo/a.go%3Fnot-query":   "a.go?not-query",
		"file:///work/repo/a.go%23not-frag":    "a.go#not-frag",
		"file:///work/repo/x?":                 "",
		"file:///work/repo/a.go;param":         "a.go;param",
		"file:///work/repo/%C3%A9t%C3%A9.yaml": "été.yaml",
	}
	for uri, want := range cases {
		got, ok := uriPath(root, uri)
		if ok != (want != "") || got != want {
			t.Errorf("%s = %q %v, want %q", uri, got, ok, want)
		}
	}
}
