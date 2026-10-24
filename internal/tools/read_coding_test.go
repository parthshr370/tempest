package tools

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/document"
)

func TestReadSQLiteSelectorsFakeBinary(t *testing.T) {
	bin, cwd := t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin)
	codingFakeBinary(t, bin, "sqlite3", `[ "$#" = 10 ]
[ "$1 $2 $3 $4 $5 $6 $7 $8" = '-safe -readonly -batch -json -init /dev/null -cmd PRAGMA query_only=ON; PRAGMA trusted_schema=OFF;' ]
case "${10}" in
 "SELECT name,type,sql FROM sqlite_schema WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%' ORDER BY name LIMIT 500") printf '[{"name":"items"}]';;
 "SELECT name,type,sql FROM sqlite_schema WHERE name='items' AND type IN ('table','view')") printf '[{"sql":"CREATE TABLE items(value)"}]';;
 'SELECT * FROM "items" LIMIT 2 OFFSET 3') printf '[{"value":"paged"}]';;
 'SELECT * FROM (SELECT value FROM items
) LIMIT 1000 OFFSET 0') printf '[{"value":"query result"}]';;
 *) printf 'bad SQL %s' "${10}" >&2;exit 81;;esac
`)
	writeFile(t, filepath.Join(cwd, "data.db"), "SQLite format 3\x00fixture")
	mustReadSelector(t, cwd, "data.db", "items")
	mustReadSelector(t, cwd, "data.db:items?limit=2&offset=3", "CREATE TABLE", "paged")
	mustReadSelector(t, cwd, "data.db?q=SELECT value FROM items", "query result")
	for _, path := range []string{"data.db?q=DELETE FROM items", "data.db?q=", "data.db?q=SELECT 1&q=SELECT 2", "data.db:items?limit=0", "data.db:items?offset=-1", "data.db?where=x"} {
		if _, err := readSelector(t, cwd, path); err == nil {
			t.Errorf("accepted invalid SQLite selector %s", path)
		}
	}
}
func TestReadPDFFakeBinaryAndPaging(t *testing.T) {
	bin, cwd := t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin)
	codingFakeBinary(t, bin, "pdftotext", `[ "$*" = '-layout - -' ]
/bin/cat > /dev/null
printf 'first page\nsecond page\nthird page\n'
`)
	writeFile(t, filepath.Join(cwd, "paper.pdf"), "%PDF-1.7\nfixture")
	text := mustReadSelector(t, cwd, "paper.pdf:2-2", "second page")
	if strings.Contains(text, "first page") || strings.Contains(text, "third page") {
		t.Fatalf("PDF paging: %s", text)
	}
}
func TestReadSQLiteRealWAL(t *testing.T) {
	binary, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is not installed")
	}
	cwd := t.TempDir()
	path := filepath.Join(cwd, "app.db")
	cmd := exec.CommandContext(t.Context(), binary, path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Wait() })
	if _, err := stdin.Write([]byte("PRAGMA journal_mode=WAL;\nCREATE TABLE items(id INTEGER PRIMARY KEY, value TEXT);\nINSERT INTO items VALUES(1,'WAL committed row');\n.print ready\n")); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if scanner.Text() == "ready" {
			break
		}
	}
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Fatal(err)
	}
	mustReadSelector(t, cwd, "app.db:items", "WAL committed row", "CREATE TABLE")
	mustReadSelector(t, cwd, "app.db?q=SELECT value FROM items", "WAL committed row")
}

func TestReadSQLiteRawSQL(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip(err)
	}
	cwd := t.TempDir()
	if out, err := exec.Command(sqlite, filepath.Join(cwd, "app.db"), "CREATE TABLE t(a,b);INSERT INTO t VALUES(2,'42');").CombinedOutput(); err != nil {
		t.Fatalf("fixture: %s %v", out, err)
	}
	for _, path := range []string{
		"app.db?q=SELECT a+length(b) AS total FROM t WHERE b LIKE '4%' AND 'a&b'='a&b'",
		"app.db?limit=1&offset=0&q=SELECT a+length(b) AS total FROM t WHERE b LIKE '4%'",
	} {
		text := mustReadSelector(t, cwd, path, `"total":4`)
		if strings.Contains(text, `"b":2`) {
			t.Fatalf("SQL operator decoded as space: %s", text)
		}
	}
}

func TestReadCodingInputsBoundedBeforeConversion(t *testing.T) {
	cwd := t.TempDir()
	for _, name := range []string{"large.db", "large.pdf", "small.db-wal"} {
		file, err := os.Create(filepath.Join(cwd, name))
		if err != nil {
			t.Fatal(err)
		}
		err = file.Truncate(document.MaxCodingInput + 1)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("sparse fixture: %v %v", err, closeErr)
		}
	}
	writeFile(t, filepath.Join(cwd, "small.db"), "SQLite format 3\x00fixture")
	for _, name := range []string{"large.db", "large.pdf", "small.db"} {
		if _, err := readSelector(t, cwd, name); err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("unbounded %s: %v", name, err)
		}
	}
}
