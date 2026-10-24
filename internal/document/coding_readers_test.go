package document

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func documentFake(t *testing.T, bin, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nset -eu\n"+body), 0755); err != nil {
		t.Fatal(err)
	}
}
func TestReadSQLiteFakeBinary(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	documentFake(t, bin, "sqlite3", `[ "$#" = 10 ]
[ "$1 $2 $3 $4 $5 $6 $7 $8" = '-safe -readonly -batch -json -init /dev/null -cmd PRAGMA query_only=ON; PRAGMA trusted_schema=OFF;' ]
case "$9" in */snapshot.db) ;; *) exit 81;; esac
case "${10}" in
 "SELECT name,type,sql FROM sqlite_schema WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%' ORDER BY name LIMIT 500") printf '[{"name":"items","sql":"CREATE TABLE items(value)"}]';;
 "SELECT name,type,sql FROM sqlite_schema WHERE name='items' AND type IN ('table','view')") printf '[{"name":"items","sql":"CREATE TABLE items(value)"}]';;
 'SELECT * FROM "items" LIMIT 2 OFFSET 1') printf '[{"value":2}]';;
 'SELECT * FROM (SELECT value FROM items
) LIMIT 2 OFFSET 1') printf '[{"value":2}]';;
 *) printf 'unexpected SQL: %s' "${10}" >&2;exit 82;;esac
`)
	data := []byte("SQLite format 3\x00snapshot")
	for _, test := range []struct{ table, query, want string }{{"", "", "CREATE TABLE"}, {"items", "", "Schema:"}, {"", "SELECT value FROM items", "value"}} {
		out, err := ReadSQLite(t.Context(), data, nil, test.table, test.query, 2, 1)
		if err != nil || !strings.Contains(string(out), test.want) {
			t.Fatalf("read %q %q: %s %v", test.table, test.query, out, err)
		}
	}
	if _, err := ReadSQLite(t.Context(), data, nil, "", "DELETE FROM items", 2, 0); err == nil {
		t.Fatal("accepted write SQL")
	}
}
func TestSQLiteReadSQLValidation(t *testing.T) {
	for _, query := range []string{"SELECT 1", "SELECT replace('ab','a','c')", "SELECT edit, release, analyze FROM items", "select ';', 'delete' -- update\n", `SELECT "update" FROM "items"`, `/* prefix */ WITH x AS (SELECT 1) SELECT * FROM x;`} {
		if err := ValidateReadSQL(query); err != nil {
			t.Errorf("rejected %q: %v", query, err)
		}
	}
	for _, query := range []string{"", ".shell touch /tmp/unsafe", "PRAGMA journal_mode=WAL", "SELECT 1; DELETE FROM items", "ATTACH '/tmp/x' AS x", "SELECT * FROM pragma_table_info('x'); DROP TABLE items", "SELECT 'unterminated", "SELECT 1 /* unterminated", "SELECT 1;\n.shell touch x"} {
		if err := ValidateReadSQL(query); err == nil {
			t.Errorf("accepted %q", query)
		}
	}
}
func TestReadSQLiteRealBinary(t *testing.T) {
	binary, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is not installed")
	}
	path := filepath.Join(t.TempDir(), "app.db")
	if out, err := exec.Command(binary, path, "CREATE TABLE items(id INTEGER PRIMARY KEY, value TEXT); INSERT INTO items VALUES(1,'first'),(2,'second');").CombinedOutput(); err != nil {
		t.Fatalf("create fixture: %s %v", out, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ table, query, want string }{{"", "", "items"}, {"items", "", "CREATE TABLE"}, {"", "SELECT replace('ab','a','c') AS value", "cb"}, {"", "WITH x AS (SELECT value FROM items WHERE id=2) SELECT * FROM x", "second"}} {
		out, err := ReadSQLite(t.Context(), before, nil, test.table, test.query, 20, 0)
		if err != nil || !strings.Contains(string(out), test.want) {
			t.Fatalf("read %s %s: %s %v", test.table, test.query, out, err)
		}
	}
	// Safe mode blocks I/O regardless of function spelling; SQL runs inside a
	// bounded SELECT on a read-only connection, not through a keyword blacklist.
	for _, query := range []string{`SELECT writefile('/tmp/tempest-sqlite-escape','bad')`, `SELECT readfile('/etc/passwd')`, `SELECT edit('text')`, `WITH x AS (SELECT 1) DELETE FROM items`, `SELECT "writefile"('/tmp/tempest-sqlite-escape','bad')`, `SELECT "readfile"('/etc/passwd')`, `SELECT "load_extension"('missing')`} {
		if _, err := ReadSQLite(t.Context(), before, nil, "", query, 20, 0); err == nil {
			t.Errorf("safe mode accepted %s", query)
		}
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("source database changed")
	}
}
func TestReadPDFFakeBinary(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	documentFake(t, bin, "pdftotext", `[ "$*" = '-layout - -' ]
IFS= read -r magic
[ "$magic" = '%PDF-1.7' ]
printf 'page one\npage two\n'
`)
	out, err := ReadPDF(t.Context(), []byte("%PDF-1.7\nsource\n"))
	if err != nil || string(out) != "page one\npage two\n" {
		t.Fatalf("PDF: %q %v", out, err)
	}
}
func TestCodingReaderMissingBinaries(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := ReadPDF(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "pdftotext") {
		t.Fatalf("missing PDF converter: %v", err)
	}
	if _, err := ReadSQLite(t.Context(), nil, nil, "", "", 20, 0); err == nil || !strings.Contains(err.Error(), "sqlite3") {
		t.Fatalf("missing SQLite CLI: %v", err)
	}
}
func TestReadPDFRealBinary(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext is not installed")
	}
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	content := "BT /F1 12 Tf 72 720 Td (Tempest PDF proof) Tj ET\n"
	objects := []string{`<< /Type /Catalog /Pages 2 0 R >>`, `<< /Type /Pages /Kids [3 0 R] /Count 1 >>`, `<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>`, `<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>`, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content)}
	for i, obj := range objects {
		offsets = append(offsets, pdf.Len())
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	out, err := ReadPDF(t.Context(), pdf.Bytes())
	if err != nil || !strings.Contains(string(out), "Tempest PDF proof") {
		t.Fatalf("real PDF: %s %v", out, err)
	}
}
