package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStreamJSONBrokenStdoutCancelsRunBeforeExit(t *testing.T) {
	directory := t.TempDir()
	binary := filepath.Join(directory, "tempest")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build tempest: %v\n%s", err, output)
	}
	script := filepath.Join(directory, "faux.json")
	responses := []struct {
		Text string `json:"text"`
	}{{Text: strings.Repeat("streaming response ", 20000)}}
	encoded, err := json.Marshal(responses)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "-p", "go", "-faux-script", script, "-session-dir", filepath.Join(directory, "sessions"), "-mcp-config", "", "-output-format", "stream-json")
	command.Dir = directory
	command.Env = append(os.Environ(), "HOME="+directory)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(stdout).ReadBytes('\n'); err != nil {
		cancel()
		_ = command.Wait()
		t.Fatal(err)
	}
	if err := stdout.Close(); err != nil {
		cancel()
		_ = command.Wait()
		t.Fatal(err)
	}
	err = command.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Errorf("broken stdout must cancel/join the run and exit 1, not die from SIGPIPE: %v", err)
	}
	journals, err := filepath.Glob(filepath.Join(directory, "sessions", "*", "*.jsonl"))
	if err != nil || len(journals) != 1 {
		t.Fatalf("expected one run journal: %v %v", journals, err)
	}
	journal, err := os.ReadFile(journals[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(journal, []byte(`"type":"run_finished"`)) {
		t.Error("broken stdout exited without joining and finishing the durable run")
	}
}
