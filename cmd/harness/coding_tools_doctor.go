package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"go.harness.dev/harness/internal/tools"
)

func printCodingToolChecks(out io.Writer) {
	if path, err := tools.ASTBinary(); err == nil {
		fmt.Fprintf(out, "PASS ast_grep/ast_edit: %s\n", path)
	} else {
		fmt.Fprintf(out, "WARN ast_grep/ast_edit: not registered: %v\n", err)
	}
	for _, item := range []struct{ name, missing string }{
		{"gh", "GitHub tools and issue://, pr:// reads require gh and gh auth login"},
		{"sqlite3", "SQLite reads require sqlite3 -safe -readonly (SQLite 3.37+)"},
		{"pdftotext", "PDF text reads require pdftotext (brew install poppler)"},
	} {
		if path, err := exec.LookPath(item.name); err == nil {
			if item.name == "sqlite3" {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				output, err := exec.CommandContext(ctx, path, "-safe", "-readonly", "-batch", "-init", os.DevNull, ":memory:", "SELECT 1").CombinedOutput()
				cancel()
				if err != nil || strings.TrimSpace(string(output)) != "1" {
					fmt.Fprintf(out, "WARN sqlite3: -safe -readonly support required (SQLite 3.37+): %s\n", strings.TrimSpace(string(output)))
					continue
				}
			}
			fmt.Fprintf(out, "PASS %s: %s\n", item.name, path)
		} else {
			fmt.Fprintf(out, "WARN %s: %s\n", item.name, item.missing)
		}
	}
}
