package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// An editor that sends SIGTERM keeps its end of stdin open while it waits for
// the agent to exit; `tempest serve` must exit without waiting for EOF.
func TestServeStdioExitsOnSIGTERMWhileStdinStaysOpen(t *testing.T) {
	testServeStdioExitsWithStdinOpen(t, syscall.SIGTERM)
}

func TestServeStdioExitsOnSIGINTWhileStdinStaysOpen(t *testing.T) {
	testServeStdioExitsWithStdinOpen(t, os.Interrupt)
}

func testServeStdioExitsWithStdinOpen(t *testing.T, signal os.Signal) {
	t.Helper()
	tmp := t.TempDir()
	exe := filepath.Join(tmp, "harness-test")
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("build harness: %v\n%s", err, out)
	}
	cmd := exec.Command(exe, "serve", "-session-dir", filepath.Join(tmp, "sessions"), "-shutdown-timeout", "1s")
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
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
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		if err := stdin.Close(); err != nil {
			t.Logf("close stdin: %v", err)
		}
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			if err := cmd.Process.Kill(); err != nil {
				t.Logf("kill: %v", err)
			}
			<-exited
		}
	})
	if _, err := stdin.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(stdout).ReadBytes('\n'); err != nil {
		t.Fatalf("initialize response: %v", err)
	}
	if err := cmd.Process.Signal(signal); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
		if waitErr != nil {
			t.Fatalf("serve exit after %s: %v", signal, waitErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("serve still running 5s after %s while stdin stayed open; it exits only when stdin reaches EOF", signal)
	}
}
