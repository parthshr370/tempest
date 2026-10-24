package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.harness.dev/harness/internal/compaction"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

// TestConversationKillHookPeer is the before-request gate used by the CLI proof.
func TestConversationKillHookPeer(t *testing.T) {
	marker := os.Getenv("TEMPEST_CONVERSATION_KILL_HOOK")
	if marker == "" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	calls := 0
	for scanner.Scan() {
		var frame struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			os.Exit(2)
		}
		var result any = map[string]any{}
		switch frame.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "tempest-extension/1", "id": "kill-gate", "revision": "1", "capabilities": []string{}, "definition": map[string]any{"hooks": []any{map[string]any{"name": "gate", "event": "before_request", "replay": "safe"}}}}
		case "_tempest/extension/hooks/call":
			calls++
			if calls == 2 {
				if _, err := os.Stat(marker); os.IsNotExist(err) {
					if err := os.WriteFile(marker, []byte("held before retry provider admission"), 0600); err != nil {
						os.Exit(2)
					}
					// A killed host closes stdin, so the peer can't outlive the proof.
					io.Copy(io.Discard, os.Stdin)
					os.Exit(0)
				}
			}
			result = map[string]any{"kind": "continue"}
		}
		if len(frame.ID) > 0 {
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": result})
			fmt.Fprintln(os.Stdout, string(raw))
		}
	}
	os.Exit(0)
}

type conversationProofPeer struct {
	process *exec.Cmd
	input   io.WriteCloser
	replies chan json.RawMessage
	next    int
	logs    strings.Builder
}

func startConversationProofPeer(t *testing.T, binary, workspace, sessions, script, extensionConfig string) *conversationProofPeer {
	t.Helper()
	args := []string{"serve", "-session-dir", sessions, "-faux-script", script, "-mcp-config", "", "-permission-mode", "bypass"}
	if extensionConfig != "" {
		args = append(args, "-extension-config", extensionConfig)
	}
	process := exec.Command(binary, args...)
	process.Dir = workspace
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "HARNESS_") && !strings.HasPrefix(value, "LYRA_") && !strings.HasPrefix(value, "TEMPEST_") && !strings.HasPrefix(value, "HOME=") {
			process.Env = append(process.Env, value)
		}
	}
	process.Env = append(process.Env, "HOME="+workspace)
	input, err := process.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := process.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	peer := &conversationProofPeer{process: process, input: input, replies: make(chan json.RawMessage, 64)}
	process.Stderr = &peer.logs
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(peer.replies)
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), 8<<20)
		for scanner.Scan() {
			raw := append(json.RawMessage(nil), scanner.Bytes()...)
			var frame struct {
				ID json.RawMessage `json:"id"`
			}
			if json.Unmarshal(raw, &frame) == nil && len(frame.ID) > 0 {
				peer.replies <- raw
			}
		}
	}()
	t.Cleanup(func() { peer.stop(t, false) })
	peer.call(t, "initialize", map[string]any{"protocolVersion": 1}, false)
	return peer
}

func (p *conversationProofPeer) send(t *testing.T, method string, params any) int {
	t.Helper()
	p.next++
	id := p.next
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(p.input, string(raw)); err != nil {
		t.Fatal(err)
	}
	return id
}
func (p *conversationProofPeer) call(t *testing.T, method string, params any, allowError bool) json.RawMessage {
	t.Helper()
	id := p.send(t, method, params)
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for {
		select {
		case raw, ok := <-p.replies:
			if !ok {
				t.Fatalf("CLI closed during %s", method)
			}
			var frame struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(raw, &frame); err != nil {
				t.Fatal(err)
			}
			if frame.ID == id {
				if len(frame.Error) > 0 && !allowError {
					t.Fatalf("%s: %s", method, raw)
				}
				return raw
			}
		case <-timer.C:
			t.Fatalf("CLI timed out during %s", method)
		}
	}
}
func (p *conversationProofPeer) stop(t *testing.T, kill bool) {
	t.Helper()
	if p.process.ProcessState != nil {
		return
	}
	if kill {
		if err := p.process.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
	} else {
		p.input.Close()
	}
	err := p.process.Wait()
	if kill {
		if exit, ok := err.(*exec.ExitError); !ok || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
			t.Fatalf("not killed by SIGKILL: %v", err)
		}
	} else if err != nil {
		t.Fatalf("CLI shutdown: %v\n%s", err, p.logs.String())
	}
}

func conversationProofScript(t *testing.T, dir string, responses []fauxScriptResponse) string {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("script-%d.json", time.Now().UnixNano()))
	raw, err := json.Marshal(responses)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func conversationProofSeed(t *testing.T, peer *conversationProofPeer, sessions, cwd string, messages []types.Message) (string, string) {
	t.Helper()
	raw := peer.call(t, "session/new", map[string]any{"cwd": cwd}, false)
	var created struct {
		Result struct {
			SessionID string `json:"sessionId"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	id := created.Result.SessionID
	if id == "" {
		t.Fatal(string(raw))
	}
	peer.call(t, "session/close", map[string]any{"sessionId": id}, false)
	catalog, err := journal.Catalog(t.Context(), sessions, "")
	if err != nil || len(catalog) != 1 {
		t.Fatal(catalog, err)
	}
	path := catalog[0].Path
	store, _, err := journal.OpenForResume(t.Context(), path, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if err := store.AppendMessage(t.Context(), message); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	peer.call(t, "session/resume", map[string]any{"sessionId": id, "cwd": cwd}, false)
	return id, path
}

func conversationProofRecords(t *testing.T, path string) []durable.Record {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records []durable.Record
	for _, line := range strings.Split(string(raw), "\n")[1:] {
		if line == "" {
			continue
		}
		envelope, err := durable.DecodeEnvelope(path, []byte(line))
		if err != nil {
			continue
		}
		for _, change := range envelope.Commit.Changes {
			records = append(records, durable.Record{Change: change, Seq: envelope.Receipt.Seq, CommitID: envelope.Commit.ID})
		}
	}
	return records
}
func conversationProofWait(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("crash-boundary marker didn't arrive")
}
func conversationProofFacts(t *testing.T, path string) (started, finished, summaryFrames, foregroundFrames, placements, claims int) {
	t.Helper()
	records := conversationProofRecords(t, path)
	prompts := make(map[string]string)
	for _, record := range records {
		if record.Namespace == "_tempest/request-prompts" {
			var prompt struct {
				System string `json:"system"`
			}
			if err := json.Unmarshal(record.Value, &prompt); err != nil {
				t.Fatal(err)
			}
			prompts[record.ID] = prompt.System
		}
	}
	for _, record := range records {
		if record.Namespace == "_tempest/overflow" {
			claims++
		}
		if record.Namespace == "_tempest/requests" {
			var frame struct {
				PromptDigest string `json:"promptDigest"`
			}
			if err := json.Unmarshal(record.Value, &frame); err != nil {
				t.Fatal(err)
			}
			system, found := prompts[frame.PromptDigest]
			if !found {
				t.Fatalf("request frame has no immutable prompt: %s", record.Value)
			}
			if system == compaction.SummarizationSystemPrompt {
				summaryFrames++
			} else {
				foregroundFrames++
			}
		}
		if record.Namespace == "_tempest/journal" {
			var entry struct {
				Type string `json:"type"`
			}
			json.Unmarshal(record.Value, &entry)
			switch entry.Type {
			case "compaction_call_started":
				started++
			case "compaction_call_finished":
				finished++
			case "compaction":
				placements++
			}
		}
	}
	return
}

func TestConversationRealBinarySIGKILLReceipts(t *testing.T) {
	workspace := t.TempDir()
	binary := filepath.Join(workspace, "tempest")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	t.Run("held_background_summarizer", func(t *testing.T) {
		cwd, sessions := t.TempDir(), t.TempDir()
		var err error
		cwd, err = filepath.EvalSymlinks(cwd)
		if err != nil {
			t.Fatal(err)
		}
		gate := filepath.Join(cwd, "release-summary")
		script := conversationProofScript(t, cwd, []fauxScriptResponse{{Text: "paid history receipt"}, {Text: "held prefix summary", WaitForFile: gate}})
		peer := startConversationProofPeer(t, binary, cwd, sessions, script, "")
		messages := []types.Message{types.UserMessage{Content: types.StringContent("old history")}, types.AssistantMessage{Content: []types.ContentBlock{types.NewText("old answer")}}, types.UserMessage{Content: types.StringContent("turn prefix to summarize")}, types.AssistantMessage{Content: []types.ContentBlock{types.NewText(strings.Repeat("retained assistant tail ", 6000))}}}
		id, path := conversationProofSeed(t, peer, sessions, cwd, messages)
		raw := peer.call(t, "_tempest/durable/compact", map[string]any{"sessionId": id}, false)
		var admitted struct {
			Result extension.Task `json:"result"`
		}
		if err := json.Unmarshal(raw, &admitted); err != nil {
			t.Fatal(err)
		}
		conversationProofWait(t, func() bool {
			s, f, frames, _, _, _ := conversationProofFacts(t, path)
			return s == 2 && f == 1 && frames == 2
		})
		peer.stop(t, true)
		reopened := startConversationProofPeer(t, binary, cwd, sessions, conversationProofScript(t, cwd, []fauxScriptResponse{{Error: "paid receipt must not be dispatched again"}}), "")
		reopened.call(t, "session/resume", map[string]any{"sessionId": id, "cwd": cwd}, false)
		reopened.call(t, "_tempest/durable/resume_pending", map[string]any{"sessionId": id}, false)
		conversationProofWait(t, func() bool {
			raw := reopened.call(t, "_tempest/durable/get_task", map[string]any{"sessionId": id, "taskId": admitted.Result.ID}, false)
			var result struct {
				Result extension.Task `json:"result"`
			}
			json.Unmarshal(raw, &result)
			return result.Result.Status == "failed" || result.Result.Status == "parked"
		})
		s, f, frames, foreground, placements, claims := conversationProofFacts(t, path)
		if s != 2 || f != 1 || frames != 2 || foreground != 0 || placements != 0 || claims != 0 {
			t.Fatalf("paid or unknown phase replayed: %d %d %d %d %d %d", s, f, frames, foreground, placements, claims)
		}
		reopened.stop(t, false)
		t.Log("SIGKILL held summarizer: completed history receipt reused; started=2 finished=1 summaryRequests=2 after reopen; unknown prefix not replayed")
	})
	t.Run("after_overflow_recovery_before_retry", func(t *testing.T) {
		cwd, sessions := t.TempDir(), t.TempDir()
		var err error
		cwd, err = filepath.EvalSymlinks(cwd)
		if err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(cwd, "before-retry")
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		configPath := filepath.Join(cwd, "extensions.json")
		config, err := json.Marshal([]extension.ProcessOptions{{ID: "kill-gate", Argv: []string{self, "-test.run=^TestConversationKillHookPeer$"}, Env: []string{"TEMPEST_CONVERSATION_KILL_HOOK=" + marker}, SafeHandlers: []string{"hook:gate"}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, config, 0600); err != nil {
			t.Fatal(err)
		}
		script := conversationProofScript(t, cwd, []fauxScriptResponse{{Error: "context_length_exceeded"}, {Text: "paid overflow recovery"}, {Error: "retry should still be gated"}})
		peer := startConversationProofPeer(t, binary, cwd, sessions, script, configPath)
		messages := []types.Message{types.UserMessage{Content: types.StringContent(strings.Repeat("older first ", 5000))}, types.UserMessage{Content: types.StringContent(strings.Repeat("older second ", 5000))}, types.UserMessage{Content: types.StringContent(strings.Repeat("older third ", 5000))}}
		id, path := conversationProofSeed(t, peer, sessions, cwd, messages)
		peer.send(t, "session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]any{"type": "text", "text": "one durable overflow input"}}})
		conversationProofWait(t, func() bool { _, err := os.Stat(marker); return err == nil })
		s, f, summaries, foreground, placements, claims := conversationProofFacts(t, path)
		if s != 1 || f != 1 || summaries != 1 || foreground != 1 || placements != 1 || claims != 1 {
			t.Fatalf("not between recovery and retry: %d %d %d %d %d %d", s, f, summaries, foreground, placements, claims)
		}
		peer.stop(t, true)
		reopened := startConversationProofPeer(t, binary, cwd, sessions, conversationProofScript(t, cwd, []fauxScriptResponse{{Error: "context_length_exceeded"}, {Error: "a second retry is forbidden"}}), configPath)
		reopened.call(t, "session/resume", map[string]any{"sessionId": id, "cwd": cwd}, false)
		reopened.call(t, "tempest/continue", map[string]any{"sessionId": id}, true)
		reopened.call(t, "tempest/continue", map[string]any{"sessionId": id}, true)
		s, f, summaries, foreground, placements, claims = conversationProofFacts(t, path)
		if s != 1 || f != 1 || summaries != 1 || foreground > 2 || placements != 1 || claims != 1 {
			t.Fatalf("recovery receipt or retry duplicated: %d %d %d %d %d %d", s, f, summaries, foreground, placements, claims)
		}
		reopened.stop(t, false)
		t.Logf("SIGKILL after recovery before retry: summary receipt reused; summaryRequests=%d foregroundRequests=%d retryClaims=%d placements=%d", summaries, foreground, claims, placements)
	})
}
