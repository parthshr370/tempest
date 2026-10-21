package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBashDefaultTimeoutKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups require Unix")
	}
	cwd := t.TempDir()
	tool := newBashTool(cwd, ToolsOptions{BashTimeout: time.Second})
	started := time.Now()
	_, err := tool.Execute(context.Background(), "bash", json.RawMessage(`{"command":"printf '%s' $$ > pgid; exec sleep 30"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "Command timed out after 1 seconds") {
		t.Fatalf("timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("timeout returned after %s, want less than 2s", elapsed)
	}
	data, err := os.ReadFile(filepath.Join(cwd, "pgid"))
	if err != nil {
		t.Fatal(err)
	}
	pgid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("process group %d still exists: %v", pgid, err)
	}
}

func TestBashTimeoutKillsDescendantsHoldingPipes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tool := newBashTool(t.TempDir(), ToolsOptions{BashTimeout: time.Second})
	_, err := tool.Execute(ctx, "bash", json.RawMessage(`{"command":"sleep 30 & wait"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "Command timed out") || ctx.Err() != nil {
		t.Fatalf("descendant timeout: error=%v, outer context=%v", err, ctx.Err())
	}
}

func TestBashModelTimeoutOverridesDefault(t *testing.T) {
	for _, test := range []struct {
		name           string
		defaultTimeout time.Duration
		modelTimeout   int
		command        string
		wantError      bool
	}{
		{name: "shorten", defaultTimeout: time.Minute, modelTimeout: 1, command: "exec sleep 30", wantError: true},
		{name: "lengthen", defaultTimeout: time.Nanosecond, modelTimeout: 2, command: "printf done"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tool := newBashTool(t.TempDir(), ToolsOptions{BashTimeout: test.defaultTimeout})
			parameters := struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			}{Command: test.command, Timeout: test.modelTimeout}
			result, err := tool.Execute(context.Background(), "bash", mustJSON(t, parameters), nil)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("after %d seconds", test.modelTimeout)) {
					t.Fatalf("timeout override error = %v", err)
				}
			} else if err != nil || resultText(result) != "done" {
				t.Fatalf("timeout override result = %q, %v", resultText(result), err)
			}
		})
	}
}
