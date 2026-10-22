package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCLIMCPRealBinary(t *testing.T) {
	tmp := t.TempDir()
	exe := filepath.Join(tmp, "tempest")
	if output, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("build tempest: %v\n%s", err, output)
	}
	if err := os.Mkdir(filepath.Join(tmp, "approval-target"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(tmp, "faux.json")
	if err := os.WriteFile(script, []byte(`[{"toolCalls":[{"id":"first-tool","name":"bash","arguments":{"command":"rmdir approval-target && mkdir -p approved"}}]},{"text":"binary answer"},{"toolCalls":[{"id":"cancel-tool","name":"bash","arguments":{"command":"sleep 30"}}]},{"text":"must not finish"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(tmp, "sessions")
	command := exec.Command(exe, "mcp", "-faux-script", script, "-session-dir", sessionDir, "-max-turns", "4", "-permission-timeout", "1s", "-shutdown-timeout", "2s", "-mcp-config", "")
	command.Dir = tmp
	command.Env = append(os.Environ(), "HARNESS_MCP_CONFIG="+filepath.Join(tmp, "must-not-load.json"))
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := false
	defer func() {
		if !exited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	type frame struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
		Raw    json.RawMessage `json:"-"`
	}
	frames := make(chan frame, 128)
	readDone := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			var f frame
			if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
				readDone <- fmt.Errorf("nonprotocol stdout %q: %w", scanner.Text(), err)
				close(frames)
				return
			}
			f.Raw = bytes.Clone(scanner.Bytes())
			frames <- f
		}
		readDone <- scanner.Err()
		close(frames)
	}()
	write := func(value any) {
		t.Helper()
		data, _ := json.Marshal(value)
		if _, err := fmt.Fprintf(input, "%s\n", data); err != nil {
			t.Fatal(err)
		}
	}
	request := func(id int, method string, params any) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	}
	next := func() frame {
		t.Helper()
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatalf("stdio EOF; stderr=%s", stderr.String())
			}
			t.Logf("stdio frame: %s", f.Raw)
			return f
		case <-time.After(5 * time.Second):
			t.Fatal("stdio frame timeout")
			return frame{}
		}
	}
	request(1, "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"elicitation": map[string]any{}}, "clientInfo": map[string]string{"name": "tempest-real-binary-test", "version": "1"}})
	if f := next(); string(f.ID) != "1" || !bytes.Contains(f.Result, []byte("2025-11-25")) {
		t.Fatalf("initialize=%+v", f)
	}
	write(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	request(2, "tools/list", map[string]any{})
	if f := next(); string(f.ID) != "2" || !bytes.Contains(f.Result, []byte("tempest_run")) {
		t.Fatalf("list=%+v", f)
	}
	request(3, "tools/call", map[string]any{"name": "tempest_run", "arguments": map[string]string{"prompt": "use the tool"}, "_meta": map[string]string{"progressToken": "first"}})
	var sessionID string
	var elicited bool
	progress := 0
	for {
		f := next()
		if f.Method == "elicitation/create" {
			elicited = true
			if !bytes.Contains(f.Params, []byte(`"mode":"form"`)) {
				t.Fatal("November mode missing")
			}
			write(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": map[string]any{"action": "accept", "content": map[string]bool{"allow": true}}})
			continue
		}
		if f.Method == "notifications/progress" {
			var p struct {
				Progress int `json:"progress"`
			}
			_ = json.Unmarshal(f.Params, &p)
			if p.Progress <= progress {
				t.Fatal("nonincreasing progress")
			}
			progress = p.Progress
			continue
		}
		if string(f.ID) != "3" {
			t.Fatalf("unexpected frame=%+v", f)
		}
		var result struct {
			Structured struct {
				SessionID  string `json:"sessionId"`
				RunID      string `json:"runId"`
				Text       string `json:"text"`
				StopReason string `json:"stopReason"`
			} `json:"structuredContent"`
			IsError bool `json:"isError"`
		}
		if json.Unmarshal(f.Result, &result) != nil || result.IsError || result.Structured.Text != "binary answer" || result.Structured.RunID == "" {
			t.Fatalf("call result=%s", f.Result)
		}
		sessionID = result.Structured.SessionID
		break
	}
	if !elicited || progress == 0 {
		t.Fatalf("elicitation=%v progress=%d", elicited, progress)
	}
	if _, err := os.Stat(filepath.Join(tmp, "approved")); err != nil {
		t.Fatalf("approved tool didn't run: %v", err)
	}
	request(4, "tools/call", map[string]any{"name": "tempest_run", "arguments": map[string]string{"prompt": "cancel this run", "sessionId": sessionID}, "_meta": map[string]string{"progressToken": "cancel"}})
	for {
		f := next()
		if f.Method == "notifications/progress" {
			break
		}
		if string(f.ID) == "4" {
			t.Fatal("run completed before cancel")
		}
	}
	write(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": 4}})
	request(5, "ping", map[string]any{})
	for {
		f := next()
		if string(f.ID) == "4" {
			t.Fatal("cancelled call replied")
		}
		if string(f.ID) == "5" {
			break
		}
	}
	_ = input.Close()
	for f := range frames {
		if string(f.ID) == "4" || f.Method == "notifications/progress" {
			t.Fatalf("late cancelled frame=%+v", f)
		}
	}
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("tempest mcp exit: %v stderr=%s", err, stderr.String())
	}
	exited = true
	loaded := loadOnlyJournal(t, sessionDir)
	if loaded.LastRun == nil || loaded.LastRun.StopReason != "aborted" {
		t.Fatalf("cancel journal=%+v", loaded.LastRun)
	}
	if strings.Contains(stderr.String(), "error") {
		t.Logf("stderr: %s", stderr.String())
	}
	t.Run("SIGTERM keeps stdin open", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sigDir := filepath.Join(tmp, "signal-sessions")
		cmd := exec.CommandContext(ctx, exe, "mcp", "-faux-script", script, "-session-dir", sigDir, "-shutdown-timeout", "2s")
		cmd.Dir = tmp
		in, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		defer in.Close()
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var diagnostic bytes.Buffer
		cmd.Stderr = &diagnostic
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		}()
		scan := bufio.NewScanner(out)
		fmt.Fprintln(in, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"elicitation":{}}}}`)
		if !scan.Scan() {
			t.Fatal("initialize missing")
		}
		fmt.Fprintln(in, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		fmt.Fprintln(in, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"tempest_run","arguments":{"prompt":"wait for approval"}}}`)
		if !scan.Scan() || !strings.Contains(scan.Text(), "elicitation/create") {
			t.Fatalf("expected pending elicitation: %s", scan.Text())
		}
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		for scan.Scan() {
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("SIGTERM exit=%v stderr=%s", err, diagnostic.String())
		}
		if ctx.Err() != nil {
			t.Fatal("SIGTERM required forced termination")
		}
		state := loadOnlyJournal(t, sigDir)
		if state.LastRun == nil || state.LastRun.StopReason != "aborted" {
			t.Fatalf("SIGTERM journal=%+v", state.LastRun)
		}
	})
	t.Run("one-shot stream flags", func(t *testing.T) {
		textScript := filepath.Join(tmp, "text.json")
		if err := os.WriteFile(textScript, []byte(`[{"text":"stream binary answer"}]`), 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-p", "stream", "-faux-script", textScript, "-session-dir", filepath.Join(tmp, "stream-sessions"), "-print", "json", "-output-format", "stream-json", "-mcp-config", "")
		cmd.Dir = tmp
		cmd.Env = append(os.Environ(), "HARNESS_MCP_CONFIG="+filepath.Join(tmp, "must-not-load.json"))
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
		if len(lines) < 3 || !bytes.Contains(lines[len(lines)-2], []byte(`"status":"finished"`)) {
			t.Fatalf("stream frames=%s", out)
		}
		var report runReportOutput
		if err := json.Unmarshal(lines[len(lines)-1], &report); err != nil || report.Text != "stream binary answer" {
			t.Fatalf("terminal=%s err=%v", lines[len(lines)-1], err)
		}
		for _, line := range lines {
			if !json.Valid(line) {
				t.Fatalf("non-NDJSON: %s", line)
			}
		}
	})
}
