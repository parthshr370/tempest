package tools

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

type archiveFile struct {
	name    string
	content string
	symlink string
}

func writeZip(t *testing.T, path string, files []archiveFile) {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, file := range files {
		entry, err := writer.Create(file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(file.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, buffer.String())
}

func writeTar(t *testing.T, path string, gzipped bool, files []archiveFile) {
	t.Helper()
	var buffer bytes.Buffer
	var out io.Writer = &buffer
	var gz *gzip.Writer
	if gzipped {
		gz = gzip.NewWriter(&buffer)
		out = gz
	}
	tw := tar.NewWriter(out)
	for _, file := range files {
		header := &tar.Header{Name: file.name, Mode: 0o644, Size: int64(len(file.content)), Typeflag: tar.TypeReg}
		switch {
		case file.symlink != "":
			header = &tar.Header{Name: file.name, Linkname: file.symlink, Typeflag: tar.TypeSymlink}
		case strings.HasSuffix(file.name, "/"):
			header = &tar.Header{Name: file.name, Mode: 0o755, Typeflag: tar.TypeDir}
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(file.content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, path, buffer.String())
}

func readSelector(t *testing.T, cwd, path string) (string, error) {
	t.Helper()
	read, _ := NewTool(ReadTool, cwd, ToolsOptions{})
	result, err := executeTool(t, read, map[string]any{"path": path})
	return resultText(result), err
}

func mustReadSelector(t *testing.T, cwd, path string, wants ...string) string {
	t.Helper()
	text, err := readSelector(t, cwd, path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Fatalf("read %s = %q, missing %q", path, text, want)
		}
	}
	return text
}

func TestReadListsDirectories(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "b.txt"), "b")
	writeFile(t, filepath.Join(dir, "A.txt"), "a")
	writeFile(t, filepath.Join(dir, "sub", "c.txt"), "c")
	text := mustReadSelector(t, dir, ".", "[.#", "A.txt", "b.txt", "sub/")
	if strings.Index(text, "A.txt") > strings.Index(text, "b.txt") {
		t.Fatalf("listing is not sorted case-insensitively: %q", text)
	}
	ranged := mustReadSelector(t, dir, ".:3-3", "sub/")
	if strings.Contains(ranged, "A.txt") {
		t.Fatalf("line range on a listing = %q", ranged)
	}
	if _, err := readSelector(t, dir, "sub?q=.a"); err == nil {
		t.Fatal("JSON query on a directory succeeded")
	}
}

func TestReadArchives(t *testing.T) {
	dir := t.TempDir()
	files := []archiveFile{
		{name: "README.md", content: "readme line 1\nreadme line 2\n"},
		{name: "data/config.json", content: `{"name":"tempest","tags":["a","b"]}`},
		{name: "data/notes.txt", content: "one\ntwo\nthree\n"},
	}
	writeZip(t, filepath.Join(dir, "bundle.zip"), files)
	writeTar(t, filepath.Join(dir, "bundle.tar"), false, append(files, archiveFile{name: "data/", content: ""}, archiveFile{name: "link", symlink: "/etc/passwd"}))
	writeTar(t, filepath.Join(dir, "bundle.tgz"), true, files)
	writeTar(t, filepath.Join(dir, "bundle.tar.gz"), true, files)

	for _, archive := range []string{"bundle.zip", "bundle.tar", "bundle.tgz", "bundle.tar.gz"} {
		t.Run(archive, func(t *testing.T) {
			listing := mustReadSelector(t, dir, archive, "["+archive+"#", "README.md (28 bytes)", "data/config.json", "data/notes.txt")
			if strings.Contains(listing, "link") {
				t.Fatalf("listing shows a symlink entry: %q", listing)
			}
			member := mustReadSelector(t, dir, archive+":data/notes.txt:2-2", "["+archive+":data/notes.txt#", "two")
			if strings.Contains(member, "one") || strings.Contains(member, "three") {
				t.Fatalf("member line range = %q", member)
			}
			mustReadSelector(t, dir, archive+":./data/config.json?q=.tags[1]", `"b"`)
			mustReadSelector(t, dir, archive+":data", "data/config.json", "data/notes.txt")
			if _, err := readSelector(t, dir, archive+":missing.txt"); err == nil || !strings.Contains(err.Error(), `no member "missing.txt"`) {
				t.Fatalf("missing member error = %v", err)
			}
		})
	}
	if _, err := readSelector(t, dir, "bundle.zip?q=.a"); err == nil || !strings.Contains(err.Error(), "is an archive") {
		t.Fatalf("query on an archive error = %v", err)
	}
	writeFile(t, filepath.Join(dir, "broken.zip"), "not a zip")
	if _, err := readSelector(t, dir, "broken.zip"); err == nil || !strings.Contains(err.Error(), "not a readable zip archive") {
		t.Fatalf("broken zip error = %v", err)
	}
}

func TestReadArchiveMemberIsBounded(t *testing.T) {
	dir := t.TempDir()
	// Zeros compress to a few KiB, so the archive is small and the member is not.
	writeZip(t, filepath.Join(dir, "bomb.zip"), []archiveFile{{name: "zeros.bin", content: strings.Repeat("\x00", maxArchiveMemberBytes+1)}})
	if _, err := readSelector(t, dir, "bomb.zip:zeros.bin"); err == nil || !strings.Contains(err.Error(), "read limit") {
		t.Fatalf("oversized member error = %v", err)
	}
	mustReadSelector(t, dir, "bomb.zip", "zeros.bin (16777217 bytes)")
}

func TestReadExtractsOfficeDocuments(t *testing.T) {
	dir := t.TempDir()
	writeZip(t, filepath.Join(dir, "memo.docx"), []archiveFile{
		{name: "[Content_Types].xml", content: "<Types/>"},
		{name: "word/document.xml", content: `<w:document xmlns:w="urn:word"><w:body><w:p><w:r><w:t>Quarterly memo</w:t></w:r></w:p><w:p><w:r><w:t>Second paragraph</w:t></w:r></w:p></w:body></w:document>`},
	})
	mustReadSelector(t, dir, "memo.docx", "[memo.docx#", "Quarterly memo", "Second paragraph")
	writeFile(t, filepath.Join(dir, "broken.xlsx"), "PK not really")
	if _, err := readSelector(t, dir, "broken.xlsx"); err == nil {
		t.Fatal("corrupt xlsx read succeeded")
	}
}

func TestReadRefusesPDFAndSQLiteWithAPointer(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "paper.pdf"), "%PDF-1.7\n\x00binary")
	writeFile(t, filepath.Join(dir, "app.data"), "SQLite format 3\x00\x10\x00")
	if _, err := readSelector(t, dir, "paper.pdf"); err == nil || !strings.Contains(err.Error(), "pdftotext") {
		t.Fatalf("PDF error = %v", err)
	}
	if _, err := readSelector(t, dir, "app.data"); err == nil || !strings.Contains(err.Error(), "sqlite3") {
		t.Fatalf("SQLite error = %v", err)
	}
}

func TestReadJSONQueryThroughTheTool(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"web","scripts":{"build":"vite build","test":"vitest"},"workspaces":[{"name":"a"},{"name":"b","private":true}]}`)
	text := mustReadSelector(t, dir, "package.json?q=.workspaces[1].name", "[package.json?q=.workspaces[1].name#", `"b"`)
	if strings.Contains(text, `"a"`) {
		t.Fatalf("query output = %q", text)
	}
	mustReadSelector(t, dir, "package.json?q=.scripts | keys", `"build"`, `"test"`)
	if _, err := readSelector(t, dir, "package.json?q=.name.first"); err == nil || !strings.Contains(err.Error(), `cannot index string with "first"`) {
		t.Fatalf("type error = %v", err)
	}
	writeFile(t, filepath.Join(dir, "events.jsonl"), "{\"a\":1}\n{\"a\":2}\n")
	if _, err := readSelector(t, dir, "events.jsonl?q=.a"); err == nil || !strings.Contains(err.Error(), "single JSON value") {
		t.Fatalf("JSON Lines error = %v", err)
	}
}

func TestJSONQuerySubset(t *testing.T) {
	document := []byte(`{"a":{"b":[10,20,30]},"items":[{"name":"x","tags":["t1"]},{"name":"y"},3],"odd key":{"k":true},"s":"héllo","n":null,"neg":-4.5}`)
	for _, tc := range []struct{ query, want string }{
		{".", ""},
		{".a.b[1]", "20"},
		{".a.b.[0]", "10"},
		{".a.b[-1]", "30"},
		{".a.b[9]", "null"},
		{".a.b[]", "10\n20\n30"},
		{".items[]?.name?", "\"x\"\n\"y\""},
		{".missing[]?", "(query produced no results)"},
		{".items[0].tags[]", `"t1"`},
		{`."odd key".k`, "true"},
		{`.["odd key"]["k"]`, "true"},
		{".missing.deeper", "null"},
		{".a | keys", "[\n  \"b\"\n]"},
		{".items | length", "3"},
		{".s | length", "5"},
		{".n | length", "0"},
		{".neg | length", "4.5"},
		{".a.b | keys", "[\n  0,\n  1,\n  2\n]"},
		{".items[2].name?", "(query produced no results)"},
		{"keys | length", "6"},
	} {
		got, err := runJSONQuery(document, tc.query)
		if err != nil {
			t.Errorf("query %q: %v", tc.query, err)
			continue
		}
		if tc.want != "" && got != tc.want {
			t.Errorf("query %q = %q, want %q", tc.query, got, tc.want)
		}
	}
	for query, want := range map[string]string{
		".items[].name":  "cannot index number",
		".s[]":           "cannot iterate over string",
		".a.b.c":         "cannot index array",
		".s | keys":      "has no keys",
		"name":           "unsupported filter",
		".a |":           "empty filter",
		".a[x]":          "unsupported index",
		".a[":            "missing ]",
		`."open`:         "unterminated",
		".a.b[0] | .[0]": "cannot index number with a number",
	} {
		if _, err := runJSONQuery(document, query); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("query %q error = %v, want %q", query, err, want)
		}
	}
	if _, err := runJSONQuery([]byte("not json"), "."); err == nil {
		t.Error("query on invalid JSON succeeded")
	}
}

func TestParseReadTarget(t *testing.T) {
	for input, want := range map[string]readTarget{
		"a.txt":                        {path: "a.txt"},
		"x.ZIP:dir/a.json?q=.k":        {path: "x.ZIP", member: "dir/a.json", hasMember: true, query: ".k", hasQuery: true},
		"local://b.tar.gz:a":           {path: "local://b.tar.gz", member: "a", hasMember: true},
		"data.json?q=.a | keys":        {path: "data.json", query: ".a | keys", hasQuery: true},
		"notes.zipper:x":               {path: "notes.zipper:x"},
		"outer.zip:inner.tgz:file.txt": {path: "outer.zip", member: "inner.tgz:file.txt", hasMember: true},
	} {
		if got := parseReadTarget(input); got != want {
			t.Errorf("parseReadTarget(%q) = %+v, want %+v", input, got, want)
		}
	}
}
