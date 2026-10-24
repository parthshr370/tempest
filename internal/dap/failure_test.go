package dap

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDAPLaunchRejectionReturnsBeforeDeadline(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	debugger, err := New(directory, directory, map[string]Adapter{"fake": {Command: executable, Args: []string{"-test.run=^TestDAPAdapterProcess$", "--", "dap-helper", filepath.Join(directory, "sequence")}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer debugger.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = debugger.Start(ctx, "launch", "fake", "program", map[string]json.RawMessage{"fail": json.RawMessage(`true`)})
	if err == nil || !strings.Contains(err.Error(), "operation not permitted") || ctx.Err() != nil {
		t.Fatalf("launch didn't return adapter failure immediately: %v", err)
	}
}

func TestDAPReverseAdapterExitReturnsBeforeDeadline(t *testing.T) {
	directory := t.TempDir()
	debugger, err := New(directory, directory, map[string]Adapter{
		"unavailable": {Command: "/bin/sh", Args: []string{"-c", "printf 'operation not permitted\\n' >&2; exit 3", "{address}"}, Transport: "reverse_tcp"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer debugger.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = debugger.Start(ctx, "launch", "unavailable", "program", nil)
	if err == nil || !strings.Contains(err.Error(), "operation not permitted") || ctx.Err() != nil {
		t.Fatalf("adapter exit didn't return its failure immediately: %v", err)
	}
}
