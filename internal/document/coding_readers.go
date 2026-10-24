package document

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const maxCodingOutput = 8 << 20

// MaxCodingInput bounds each PDF, database and WAL snapshot before conversion.
const MaxCodingInput = 32 << 20

// ReadPDF extracts text without letting the converter resolve workspace paths.
func ReadPDF(ctx context.Context, data []byte) ([]byte, error) {
	if len(data) > MaxCodingInput {
		return nil, fmt.Errorf("PDF exceeds the %d MiB read limit", MaxCodingInput>>20)
	}
	binary, err := exec.LookPath("pdftotext")
	if err != nil {
		return nil, fmt.Errorf("PDF text extraction requires pdftotext on PATH (brew install poppler)")
	}
	return codingCommand(ctx, binary, []string{"-layout", "-", "-"}, bytes.NewReader(data), "")
}

// ReadSQLite queries a private snapshot, including a WAL when supplied. The CLI
// cannot modify the source database or use shell dot commands.
func ReadSQLite(ctx context.Context, data, wal []byte, table, query string, limit, offset int) ([]byte, error) {
	if len(data) > MaxCodingInput || len(wal) > MaxCodingInput {
		return nil, fmt.Errorf("SQLite database or WAL exceeds the %d MiB read limit", MaxCodingInput>>20)
	}
	binary, err := exec.LookPath("sqlite3")
	if err != nil {
		return nil, fmt.Errorf("SQLite reads require sqlite3 on PATH")
	}
	if query != "" {
		if err := ValidateReadSQL(query); err != nil {
			return nil, err
		}
	}
	if limit < 1 || limit > 1000 || offset < 0 {
		return nil, fmt.Errorf("SQLite limit must be 1..1000 and offset must be non-negative")
	}
	dir, err := os.MkdirTemp("", "tempest-sqlite-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	name := filepath.Join(dir, "snapshot.db")
	if err := os.WriteFile(name, data, 0600); err != nil {
		return nil, err
	}
	if len(wal) > 0 {
		if err := os.WriteFile(name+"-wal", wal, 0600); err != nil {
			return nil, err
		}
	}
	run := func(sql string) ([]byte, error) {
		return codingCommand(ctx, binary, []string{"-safe", "-readonly", "-batch", "-json", "-init", os.DevNull, "-cmd", "PRAGMA query_only=ON; PRAGMA trusted_schema=OFF;", name, sql}, nil, dir)
	}
	if query != "" {
		// A subquery caps output even when the caller omits LIMIT.
		query = strings.TrimSpace(query)
		query = strings.TrimSuffix(query, ";")
		return run(fmt.Sprintf("SELECT * FROM (%s\n) LIMIT %d OFFSET %d", query, limit, offset))
	}
	if table == "" {
		return run("SELECT name,type,sql FROM sqlite_schema WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%' ORDER BY name LIMIT 500")
	}
	quoted := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
	literal := `'` + strings.ReplaceAll(table, `'`, `''`) + `'`
	schema, err := run("SELECT name,type,sql FROM sqlite_schema WHERE name=" + literal + " AND type IN ('table','view')")
	if err != nil {
		return nil, err
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(schema, &rows); err != nil || len(rows) == 0 {
		return nil, fmt.Errorf("SQLite table %q not found", table)
	}
	values, err := run("SELECT * FROM " + quoted + " LIMIT " + strconv.Itoa(limit) + " OFFSET " + strconv.Itoa(offset))
	if err != nil {
		return nil, err
	}
	return []byte("Schema:\n" + string(schema) + "\nRows:\n" + string(values)), nil
}

// ValidateReadSQL accepts one SELECT, including read-only CTEs. Quoted text and
// comments are tokenized so semicolons in values do not become statements.
// The read-only connection, query_only and CLI safe mode are independent guards.
func ValidateReadSQL(query string) error {
	leadingKeyword := ""
	ended := false
	for i := 0; i < len(query); {
		c := query[i]
		if unicode.IsSpace(rune(c)) {
			i++
			continue
		}
		if c == '-' && i+1 < len(query) && query[i+1] == '-' {
			j := strings.IndexByte(query[i:], '\n')
			if j < 0 {
				break
			}
			i += j + 1
			continue
		}
		if c == '/' && i+1 < len(query) && query[i+1] == '*' {
			j := strings.Index(query[i+2:], "*/")
			if j < 0 {
				return fmt.Errorf("unterminated SQL comment")
			}
			i += j + 4
			continue
		}
		if ended {
			return fmt.Errorf("SQLite query must contain one read-only SELECT statement")
		}
		if c == ';' {
			ended = true
			i++
			continue
		}
		if c == '\'' || c == '"' || c == '`' || c == '[' {
			close := c
			if c == '[' {
				close = ']'
			}
			i++
			closed := false
			for i < len(query) {
				if query[i] == close {
					i++
					if i < len(query) && query[i] == close && close != ']' {
						i++
						continue
					}
					closed = true
					break
				}
				i++
			}
			if !closed {
				return fmt.Errorf("unterminated SQL quoted value")
			}
			continue
		}
		if c == '.' && leadingKeyword == "" {
			return fmt.Errorf("SQLite shell commands are not allowed")
		}
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' {
			start := i
			for i < len(query) && (query[i] >= 'a' && query[i] <= 'z' || query[i] >= 'A' && query[i] <= 'Z' || query[i] >= '0' && query[i] <= '9' || query[i] == '_') {
				i++
			}
			if leadingKeyword == "" {
				leadingKeyword = strings.ToUpper(query[start:i])
			}
			continue
		}
		i++
	}
	if leadingKeyword != "SELECT" && leadingKeyword != "WITH" {
		return fmt.Errorf("SQLite query must be a read-only SELECT or WITH SELECT")
	}
	return nil
}

type codingOutput struct{ bytes.Buffer }

func (b *codingOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxCodingOutput {
		return 0, fmt.Errorf("document output exceeds %d MiB; select fewer rows", maxCodingOutput>>20)
	}
	return b.Buffer.Write(p)
}

func codingCommand(ctx context.Context, binary string, args []string, stdin io.Reader, dir string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = stdin
	cmd.Dir = dir
	var out, stderr codingOutput
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", filepath.Base(binary), err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}
