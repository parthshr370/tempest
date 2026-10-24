package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodingToolDoctorMissingBinaries(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var out bytes.Buffer
	printCodingToolChecks(&out)
	for _, want := range []string{"WARN ast_grep/ast_edit: not registered:", "require ast-grep", "WARN gh:", "gh auth login", "WARN sqlite3:", "-readonly", "WARN pdftotext:", "brew install poppler"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %s", want, out.String())
		}
	}
}

func TestCodingToolDoctorRequiresSQLiteSafeMode(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	sqlite := filepath.Join(bin, "sqlite3")
	if err := os.WriteFile(sqlite, []byte("#!/bin/sh\nprintf 'unknown option: -safe' >&2\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	printCodingToolChecks(&out)
	if strings.Contains(out.String(), "PASS sqlite3:") || !strings.Contains(out.String(), "WARN sqlite3: -safe -readonly support required") {
		t.Fatalf("unsafe sqlite PASS: %s", out.String())
	}
	if err := os.WriteFile(sqlite, []byte("#!/bin/sh\n[ \"$1 $2\" = '-safe -readonly' ] || exit 1\nprintf '1\\n'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	printCodingToolChecks(&out)
	if !strings.Contains(out.String(), "PASS sqlite3:") {
		t.Fatalf("supported sqlite failed: %s", out.String())
	}
}
