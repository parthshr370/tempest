package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/envlocal"
	"go.harness.dev/harness/internal/envrpc"
	"go.harness.dev/harness/tempest/environment"
)

func TestEnvironmentConformance(t *testing.T) {
	for _, adapter := range []string{"local", "remote"} {
		t.Run(adapter, func(t *testing.T) {
			workspace, host := t.TempDir(), t.TempDir()
			daemon, err := envlocal.New(workspace)
			if err != nil {
				t.Fatal(err)
			}
			if err = BindEnvironment(daemon, ToolsOptions{SessionDataDir: t.TempDir()}); err != nil {
				t.Fatal(err)
			}
			var lease environment.Lease = daemon
			if adapter == "remote" {
				clientStream, serverStream := net.Pipe()
				serving := make(chan error, 1)
				go func() {
					serving <- envrpc.Serve(context.Background(), serverStream, daemon, environment.Capabilities{environment.Files, environment.Search, environment.Exec, environment.Cancel})
				}()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				lease, err = envrpc.Connect(ctx, clientStream, daemon.Reference())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_ = lease.Close()
					if err := <-serving; err != nil && !errors.Is(err, context.Canceled) {
						t.Errorf("daemon close: %v", err)
					}
				})
			}
			t.Cleanup(func() { _ = daemon.Close() })
			all := AllTools(host, ToolsOptions{Environment: lease, SessionID: "conformance", SessionDataDir: t.TempDir()})
			var next atomic.Uint64
			invoke := func(name ToolName, input string) (agent.AgentToolResult, error) {
				tool, ok := all[name]
				if !ok {
					t.Fatalf("%s missing", name)
				}
				return tool.Execute(context.Background(), fmt.Sprintf("call-%d", next.Add(1)), json.RawMessage(input), nil)
			}
			must := func(name ToolName, input string) string {
				t.Helper()
				result, err := invoke(name, input)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				return resultText(result)
			}
			must(WriteTool, `{"path":"note.txt","content":"alpha\nbeta\n"}`)
			if _, err := os.Stat(filepath.Join(host, "note.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("workspace operation touched host cwd: %v", err)
			}
			text := must(ReadTool, `{"path":"note.txt:1-1"}`)
			if !strings.Contains(text, "1:alpha") {
				t.Fatalf("range read: %s", text)
			}
			if text := must(GrepTool, `{"path":".","pattern":"alpha","literal":true}`); !strings.Contains(text, "note.txt") {
				t.Fatalf("search: %s", text)
			}
			if text := must(FindTool, `{"path":".","pattern":"*.txt"}`); !strings.Contains(text, "note.txt") {
				t.Fatalf("find: %s", text)
			}
			if text := must(LsTool, `{"path":"."}`); !strings.Contains(text, "note.txt") {
				t.Fatalf("list: %s", text)
			}
			tag := tagOf(t, must(ReadTool, `{"path":"note.txt"}`), "note.txt")
			must(EditTool, fmt.Sprintf(`{"path":"note.txt","edits":[{"old_text":"alpha","new_text":"ALPHA","anchor":%q}]}`, tag))
			if _, err := invoke(EditTool, fmt.Sprintf(`{"path":"note.txt","edits":[{"old_text":"beta","new_text":"BETA","anchor":%q}]}`, tag)); err == nil {
				t.Fatal("stale anchor accepted")
			}
			must(WriteTool, `{"path":"other.txt","content":"other\n"}`)
			tagA := tagOf(t, must(ReadTool, `{"path":"note.txt"}`), "note.txt")
			tagB := tagOf(t, must(ReadTool, `{"path":"other.txt"}`), "other.txt")
			patch := fmt.Sprintf("[note.txt#%s]\nCUT 1.=1 @removed\n[other.txt#%s]\nPUT 99.=99 @removed\n", tagA, tagB)
			arguments, _ := json.Marshal(struct {
				Input string `json:"input"`
			}{patch})
			if _, err := invoke(HashlineTool, string(arguments)); err == nil {
				t.Fatal("invalid batch succeeded")
			}
			contents, err := os.ReadFile(filepath.Join(workspace, "note.txt"))
			if err != nil || string(contents) != "ALPHA\nbeta\n" {
				t.Fatalf("failed batch changed files: %q %v", contents, err)
			}
			outside := filepath.Join(t.TempDir(), "secret.txt")
			if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
				t.Fatal(err)
			}
			for _, name := range []ToolName{ReadTool, WriteTool} {
				input := `{"path":"escape","content":"escaped"}`
				if name == ReadTool {
					input = `{"path":"escape"}`
				}
				if _, err := invoke(name, input); err == nil {
					t.Fatalf("%s followed escaping symlink", name)
				}
			}
			if text := must(BashTool, `{"command":"printf command-ok"}`); !strings.Contains(text, "command-ok") {
				t.Fatalf("command: %s", text)
			}
			request, _ := json.Marshal(environment.ExecRequest{Argv: []string{"sh", "-c", "printf 123456789"}, MaxBytes: 4})
			result, err := lease.Call(context.Background(), environment.Call{Environment: lease.Reference(), Key: "argv", Operation: "exec_argv", Arguments: request})
			if err != nil {
				t.Fatal(err)
			}
			var output environment.ExecResult
			if err = json.Unmarshal(result, &output); err != nil {
				t.Fatal(err)
			}
			if output.Output != "1234" || !output.Truncated || output.ExitCode != 0 {
				t.Fatalf("argv output bound: %+v", output)
			}
			spilled, err := invoke(BashTool, `{"command":"i=0; while [ $i -lt 2500 ]; do printf 'artifact-line\\n'; i=$((i+1)); done"}`)
			if err != nil {
				t.Fatal(err)
			}
			var artifact struct {
				FullOutputPath string `json:"fullOutputPath"`
			}
			if err := json.Unmarshal(spilled.Details, &artifact); err != nil || artifact.FullOutputPath == "" {
				t.Fatalf("missing paged output artifact: %s %v", spilled.Details, err)
			}
			paged, _ := json.Marshal(struct {
				Path string `json:"path"`
			}{artifact.FullOutputPath + ":1-3"})
			if text := must(ReadTool, string(paged)); !strings.Contains(text, "artifact-line") {
				t.Fatalf("output paging: %s", text)
			}
			cancelCtx, cancel := context.WithCancel(context.Background())
			request, _ = json.Marshal(environment.ExecRequest{Argv: []string{"sh", "-c", "printf ready > cancel-ready; sleep 30"}})
			finished := make(chan error, 1)
			go func() {
				_, err := lease.Call(cancelCtx, environment.Call{Environment: lease.Reference(), Key: "cancel-me", Operation: "exec_argv", Arguments: request})
				finished <- err
			}()
			startDeadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(workspace, "cancel-ready")); err == nil {
					break
				}
				if time.Now().After(startDeadline) {
					cancel()
					t.Fatal("exec did not start")
				}
				time.Sleep(time.Millisecond)
			}
			deadline, stop := context.WithTimeout(context.Background(), 5*time.Second)
			cancellation, err := lease.Cancel(deadline, "cancel-me")
			stop()
			cancel()
			if err != nil || !cancellation.Confirmed {
				t.Fatalf("cancellation not confirmed: %+v %v", cancellation, err)
			}
			if err := <-finished; err == nil {
				t.Fatal("cancelled exec succeeded")
			}
			wrong := lease.Reference()
			wrong.Generation = "destroyed"
			if _, err := lease.Call(context.Background(), environment.Call{Environment: wrong, Key: "wrong", Operation: "read_bytes", Arguments: json.RawMessage(`{"path":"note.txt","limit":10}`)}); !errors.Is(err, environment.ErrIdentity) {
				t.Fatalf("identity mismatch: %v", err)
			}
			if adapter == "remote" {
				for _, name := range []ToolName{CheckpointTool, RewindTool, EvalTool, LSPDefinitionTool, GHCheckoutTool, ASTEditTool} {
					if _, err := NewTool(name, host, ToolsOptions{Environment: lease}); !errors.Is(err, environment.ErrUnsupported) {
						t.Fatalf("%s must reject unsupported remote service: %v", name, err)
					}
				}
			}
			retired := workspace + "-retired"
			if err := os.Rename(workspace, retired); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(retired) })
			if err := os.Mkdir(workspace, 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := invoke(WriteTool, `{"path":"replacement","content":"must not write"}`); !errors.Is(err, environment.ErrIdentity) {
				t.Fatalf("live root replacement was not fenced: %v", err)
			}
		})
	}
}
func TestRemoteCommandAndHashlineAllowOrdinaryURLs(t *testing.T) {
	for _, command := range []string{"echo https://example.com", "psql postgres://db.example/app"} {
		host, err := hostReferenceCall(BashTool, mustJSON(t, map[string]any{"command": command}))
		if err != nil || host {
			t.Fatalf("bash %q host=%t err=%v", command, host, err)
		}
	}
	host, err := hostReferenceCall(HashlineTool, mustJSON(t, map[string]any{"input": "[file#ABCD]\nPUT 1.=1:\n+https://example.com\n"}))
	if err != nil || host {
		t.Fatalf("hashline URL content host=%t err=%v", host, err)
	}
	_, err = hostReferenceCall(HashlineTool, mustJSON(t, map[string]any{"input": "[local://notes.md#ABCD]\nREM\n"}))
	if err == nil || !strings.Contains(err.Error(), "host artifact URLs") {
		t.Fatalf("hashline internal reference error = %v", err)
	}
}

func TestEditDetailsBoundedAfterSuccessfulMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.txt")
	if err := os.WriteFile(path, []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tool, err := NewTool(EditTool, dir, ToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executeTool(t, tool, map[string]any{"path": "large.txt", "edits": []map[string]any{{"old_text": "old", "new_text": strings.Repeat("new line\n", 5000)}}})
	if err != nil || !strings.Contains(resultText(result), "Successfully replaced 1 block") {
		t.Fatalf("edit result = %q, %v", resultText(result), err)
	}
	var details editToolDetails
	if err := json.Unmarshal(result.Details, &details); err != nil {
		t.Fatal(err)
	}
	if len(details.Diff) > maxEditDetailBytes || len(details.Patch) > maxEditDetailBytes || details.FirstChangedLine == nil {
		t.Fatalf("edit details were not bounded/preserved: diff=%d patch=%d first=%v", len(details.Diff), len(details.Patch), details.FirstChangedLine)
	}
	if !strings.Contains(details.Diff, "truncated") || !strings.Contains(details.Patch, "truncated") {
		t.Fatalf("bounded details lack truncation markers: diff=%q patch=%q", details.Diff[len(details.Diff)-64:], details.Patch[len(details.Patch)-64:])
	}
}
