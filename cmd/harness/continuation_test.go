package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIResumeWithoutPromptContinuesExistingRun(t *testing.T) {
	for _, mode := range []string{"resume", "continue"} {
		t.Run(mode, func(t *testing.T) {
			for _, assignment := range os.Environ() {
				key, _, _ := strings.Cut(assignment, "=")
				if strings.HasPrefix(key, "HARNESS_") || strings.HasPrefix(key, "LYRA_") {
					t.Setenv(key, "")
				}
			}
			t.Setenv("HOME", t.TempDir())
			t.Chdir(t.TempDir())
			sessionDir := t.TempDir()
			script := filepath.Join(t.TempDir(), "provider.json")
			if err := os.WriteFile(script, []byte(`[{"toolCalls":[{"id":"list","name":"ls","arguments":{}}]}]`), 0600); err != nil {
				t.Fatal(err)
			}
			common := []string{"-session-dir", sessionDir, "-faux-script", script, "-permission-mode", "bypass", "-output-format", "json"}
			var stdout, stderr bytes.Buffer
			first := append(append([]string{}, common...), "-p", "original input", "-max-turns", "1")
			if exit := run(first, &stdout, &stderr); exit != 0 {
				t.Fatalf("initial exit=%d stderr=%s", exit, stderr.String())
			}
			var initial struct {
				SessionID  string `json:"session_id"`
				StopReason string `json:"stop_reason"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &initial); err != nil {
				t.Fatal(err)
			}
			if initial.StopReason != "max_turns" {
				t.Fatalf("initial=%+v", initial)
			}
			if err := os.WriteFile(script, []byte(`[{"text":"continued original work"}]`), 0600); err != nil {
				t.Fatal(err)
			}
			resumed := append([]string{}, common...)
			if mode == "resume" {
				resumed = append(resumed, "-resume", initial.SessionID)
			} else {
				resumed = append(resumed, "-continue")
			}
			stdout.Reset()
			stderr.Reset()
			if exit := run(resumed, &stdout, &stderr); exit != 0 {
				t.Fatalf("continuation exit=%d stderr=%s", exit, stderr.String())
			}
			var result struct {
				SessionID  string `json:"session_id"`
				StopReason string `json:"stop_reason"`
				Text       string `json:"text"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.SessionID != initial.SessionID || result.Text != "continued original work" || result.StopReason != "completed" {
				t.Fatalf("continuation=%+v", result)
			}
			stdout.Reset()
			stderr.Reset()
			withPrompt := append(append([]string{}, common...), "-continue", "-p", "new input")
			if exit := run(withPrompt, &stdout, &stderr); exit != 0 {
				t.Fatalf("continue with prompt exit=%d stderr=%s", exit, stderr.String())
			}
		})
	}
}
