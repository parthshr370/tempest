package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestServerCommandsHonorEnableTask(t *testing.T) {
	workspace := t.TempDir()
	binary := filepath.Join(workspace, "tempest")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build tempest: %v\n%s", err, output)
	}
	// Every concurrent caller after task admission gets a terminal response.
	script := filepath.Join(workspace, "faux.json")
	if err := os.WriteFile(script, []byte(`[{"toolCalls":[{"id":"task-call","name":"task","arguments":{"prompt":"reply done","subagent_type":"explore"}}]},{"text":"done"},{"text":"done"},{"text":"done"},{"text":"done"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"serve", "mcp"} {
		for _, test := range []struct {
			name, env string
			flag      bool
			enabled   bool
		}{
			{"env=false", "false", false, false},
			{"env=true", "true", false, true},
			// The flag overrides the environment, as in one-shot mode.
			{"flag", "false", true, true},
		} {
			enabled := test.enabled
			t.Run(command+"/"+test.name, func(t *testing.T) {
				sessions := t.TempDir()
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				args := []string{command, "-faux-script", script, "-session-dir", sessions, "-mcp-config", "", "-permission-mode", "bypass"}
				if test.flag {
					args = append(args, "-enable-task")
				}
				process := exec.CommandContext(ctx, binary, args...)
				process.Dir = workspace
				process.Env = append(os.Environ(), "HOME="+workspace, "HARNESS_ENABLE_TASK="+test.env)
				input, err := process.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				output, err := process.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err = process.Start(); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := input.Close(); err != nil {
						t.Logf("close protocol input: %v", err)
					}
					if process.ProcessState == nil {
						cancel()
						if err := process.Wait(); err != nil {
							t.Logf("stop unfinished server: %v", err)
						}
					}
				}()
				scanner := bufio.NewScanner(output)
				readReply := func(identifier int) json.RawMessage {
					t.Helper()
					for scanner.Scan() {
						var frame struct {
							ID     int             `json:"id"`
							Result json.RawMessage `json:"result"`
							Error  json.RawMessage `json:"error"`
						}
						if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
							t.Fatal(err)
						}
						if frame.ID == identifier {
							if len(frame.Error) != 0 {
								t.Fatalf("RPC %d failed: %s", identifier, frame.Error)
							}
							return frame.Result
						}
					}
					t.Fatalf("server ended before RPC %d: %v", identifier, scanner.Err())
					return nil
				}
				send := func(request string) {
					t.Helper()
					if _, err := fmt.Fprintln(input, request); err != nil {
						t.Fatal(err)
					}
				}
				if command == "serve" {
					send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
					readReply(1)
					send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":` + strconv.Quote(workspace) + `}}`)
					var created struct {
						SessionID string `json:"sessionId"`
					}
					if err := json.Unmarshal(readReply(2), &created); err != nil {
						t.Fatal(err)
					}
					send(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":` + strconv.Quote(created.SessionID) + `,"prompt":[{"type":"text","text":"delegate"}]}}`)
				} else {
					send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`)
					readReply(1)
					send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
					send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"tempest_run","arguments":{"prompt":"delegate"}}}`)
				}
				readReply(3)
				if err := process.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				if err := process.Wait(); err != nil {
					t.Fatalf("server shutdown: %v", err)
				}
				loaded := loadOnlyJournal(t, sessions)
				if got := len(loaded.Jobs); got != boolInt(enabled) {
					t.Fatalf("EnableTask=%t admitted %d task jobs", enabled, got)
				}
			})
		}
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
