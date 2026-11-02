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

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/engine/validate"
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
				Timeout int    `json:"timeout_seconds"`
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

// TestDurationArgumentsNameTheirUnit runs the agent loop's argument
// validation. A benchmark model sent wait {"timeout":30000}, milliseconds for
// a seconds field; the field now says seconds, and the retired name or an
// out-of-range value fails before the tool runs instead of being ignored.
func TestDurationArgumentsNameTheirUnit(t *testing.T) {
	controller := &serviceController{}
	tools := map[ToolName]types.Tool{
		BashTool:  newBashTool(t.TempDir(), ToolsOptions{Work: controller}).Tool,
		WaitTool:  newManagementTool(WaitTool, controller).Tool,
		EvalTool:  newEvalTool(nil).Tool,
		DebugTool: newDebugTool("", nil).Tool,
	}
	gh, _ := NewTool(GHTool, t.TempDir(), ToolsOptions{})
	tools[GHTool] = gh.Tool
	for _, test := range []struct {
		tool ToolName
		args string
		want string
	}{
		{WaitTool, `{"names":["marker"],"timeout_seconds":30}`, ""},
		{WaitTool, `{"names":["marker"],"timeout":30000}`, "timeout"},
		{WaitTool, `{"names":["marker"],"timeout_seconds":30000}`, "300"},
		{BashTool, `{"command":"true","name":"marker","timeout_seconds":60}`, ""},
		{BashTool, `{"command":"true","timeout":120000}`, "timeout"},
		{BashTool, `{"command":"true","timeout_seconds":0}`, "timeout_seconds"},
		{EvalTool, `{"language":"py","code":"1","timeout_seconds":0}`, ""},
		{EvalTool, `{"language":"py","code":"1","timeout":5}`, "timeout"},
		{DebugTool, `{"action":"threads","timeout_seconds":5}`, ""},
		{DebugTool, `{"action":"threads","timeout":5}`, "timeout"},
		{GHTool, `{"op":"run_watch","run":"1","timeout_seconds":60}`, ""},
		{GHTool, `{"op":"run_watch","run":"1","timeout":60}`, "timeout"},
	} {
		_, err := validate.ValidateToolArguments(tools[test.tool], types.ContentBlock{Type: types.BlockToolCall, ID: "call", Name: string(test.tool), Arguments: json.RawMessage(test.args)})
		if test.want == "" {
			if err != nil {
				t.Errorf("%s %s: %v", test.tool, test.args, err)
			}
			continue
		}
		// The error echoes the arguments; only the reason before them counts.
		reason := ""
		if err != nil {
			reason, _, _ = strings.Cut(err.Error(), "Received arguments")
		}
		if !strings.Contains(reason, test.want) {
			t.Errorf("%s %s: error %v, want a reason naming %q", test.tool, test.args, err, test.want)
		}
	}
}
