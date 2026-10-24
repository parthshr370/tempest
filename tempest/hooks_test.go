package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/work"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
)

func TestHookDenialIsDurableAndReplayDoesNotRunCommands(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd, dir := t.TempDir(), t.TempDir()
	settings := HookSettings{Hooks: map[string][]HookGroup{
		"SessionStart": {{Hooks: []HookCommand{{Type: "command", Command: "printf x >> session-start-count"}}}},
		"PreToolUse":   {{Matcher: "write", Hooks: []HookCommand{{Type: "command", Command: "printf x >> pre-count; printf 'durable hook refusal' >&2; exit 2"}}}},
		"SessionEnd":   {{Hooks: []HookCommand{{Type: "command", Command: "printf x >> session-end-count"}}}},
	}}
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.ToolCall("write", json.RawMessage(`{"path":"must-not-exist","content":"no"}`), "blocked")), faux.Respond(faux.Text("done")))
	stack := StackConfig{Model: f.Model(), StreamFn: f.StreamSimple}
	s, err := Open(context.Background(), Options{SessionDir: dir, Cwd: cwd, New: &NewSession{}, Stack: &stack, Hooks: &settings})
	if err != nil {
		t.Fatal(err)
	}
	_, updates, stop := s.Watch(context.Background(), WatchOptions{Buffer: 128})
	defer stop()
	result, err := s.Prompt(context.Background(), Prompt{Text: "try writing"})
	if err != nil || result.Err != nil {
		t.Fatalf("%+v %v", result, err)
	}
	path := s.core.store.Path()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "must-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("denied tool executed: %v", err)
	}
	for _, name := range []string{"pre-count", "session-start-count", "session-end-count"} {
		data, err := os.ReadFile(filepath.Join(cwd, name))
		if err != nil || string(data) != "x" {
			t.Fatalf("%s=%s err=%v", name, data, err)
		}
	}
	reports := 0
	for update := range updates {
		if update.Kind == "tempest/hook" {
			reports++
		}
	}
	if reports != 3 {
		t.Fatalf("hook events=%d, want start/pre/end", reports)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	started := strings.Index(string(data), `"type":"tool_started"`)
	hook := strings.Index(string(data), `"hook_event_name":"PreToolUse"`)
	if started < 0 || hook < started || !strings.Contains(string(data), "durable hook refusal") {
		t.Fatalf("missing durable intent/denial audit: %s", data)
	}
	for range 2 {
		loaded, err := journal.Load(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(loaded.Messages)
		if !strings.Contains(string(raw), "durable hook refusal") {
			t.Fatalf("replayed denial missing: %s", raw)
		}
	}
	f2 := faux.New(faux.Options{})
	f2.SetResponses(faux.Respond(faux.Text("new live answer")))
	stack.Model, stack.StreamFn = f2.Model(), f2.StreamSimple
	resumed, err := Open(context.Background(), Options{SessionDir: dir, Cwd: cwd, Resume: &ResumeSession{Path: path}, Stack: &stack, Hooks: &settings})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close(context.Background())
	if _, err := resumed.Prompt(context.Background(), Prompt{Text: "a new live prompt"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pre-count", "session-start-count"} {
		data, err := os.ReadFile(filepath.Join(cwd, name))
		if err != nil || string(data) != "x" {
			t.Fatalf("resume reran %s: %s %v", name, data, err)
		}
	}
}

func TestSDKDoesNotRunUntrustedProjectHooks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, ".harness"), 0700); err != nil {
		t.Fatal(err)
	}
	config := `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"printf bad > project-ran"}]}]}}`
	if err := os.WriteFile(filepath.Join(cwd, ".harness", "settings.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("done")))
	stack := StackConfig{Model: f.Model(), StreamFn: f.StreamSimple}
	s, err := Open(context.Background(), Options{SessionDir: t.TempDir(), Cwd: cwd, New: &NewSession{}, Stack: &stack})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if _, err := s.Prompt(context.Background(), Prompt{Text: "go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "project-ran")); !os.IsNotExist(err) {
		t.Fatalf("untrusted cloned repository executed hook: %v", err)
	}
}

func TestStopContinuationCanLaunchBackgroundWork(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	settings := HookSettings{Hooks: map[string][]HookGroup{"Stop": {{Hooks: []HookCommand{{Type: "command", Command: `if [ ! -f stop-seen ]; then touch stop-seen; printf '%s' '{"decision":"block","reason":"run the remaining background job"}'; fi`}}}}}}
	f := faux.New(faux.Options{})
	f.SetResponses(
		faux.Respond(faux.Text("first answer")),
		faux.Respond(faux.ToolCall("bash", json.RawMessage(`{"command":"printf job-ran > resumed-job.txt","async":true}`), "after-stop")),
		faux.Respond(faux.Text("job launched")),
		faux.Respond(faux.Text("job settled")),
	)
	stack := StackConfig{Model: f.Model(), StreamFn: f.StreamSimple}
	s, err := Open(context.Background(), Options{SessionDir: t.TempDir(), Cwd: cwd, New: &NewSession{}, Stack: &stack, Hooks: &settings})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	result, err := s.Prompt(context.Background(), Prompt{Text: "finish the work"})
	if err != nil || result.Err != nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(cwd, "resumed-job.txt"))
	if err != nil || string(data) != "job-ran" {
		t.Fatalf("Stop continuation kept background admission sealed: %s err=%v", data, err)
	}
}

func TestFirstContinueRunsSessionStart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("done")))
	settings := HookSettings{Hooks: map[string][]HookGroup{"SessionStart": {{Hooks: []HookCommand{{Type: "command", Command: "printf x >> starts"}}}}}}
	s, err := Open(context.Background(), Options{Cwd: cwd, SessionDir: t.TempDir(), New: &NewSession{}, Stack: &StackConfig{Model: f.Model(), StreamFn: f.StreamSimple}, Hooks: &settings})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if _, err := s.core.queues.Enqueue(context.Background(), work.QueueInput{Kind: work.Steer, Text: "first queued prompt"}); err != nil {
		t.Fatal(err)
	}
	result, err := s.core.agent.Continue(context.Background())
	if err != nil || result.Err != nil {
		t.Fatalf("%+v %v", result, err)
	}
	raw, err := os.ReadFile(filepath.Join(cwd, "starts"))
	if err != nil || string(raw) != "x" {
		t.Fatalf("SessionStart missed first Continue: %s %v", raw, err)
	}
}

func TestRewrittenArgumentsAreDurableBeforeExecution(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.ToolCall("write", json.RawMessage(`{"path":"original","content":"old"}`), "call")))
	settings := HookSettings{Hooks: map[string][]HookGroup{"PreToolUse": {{Hooks: []HookCommand{{Type: "command", Command: `printf '%s' '{"hookSpecificOutput":{"updatedInput":{"path":"rewritten","content":"new"}}}'`}}}}}}
	s, err := Open(context.Background(), Options{Cwd: cwd, SessionDir: t.TempDir(), New: &NewSession{}, Stack: &StackConfig{Model: f.Model(), StreamFn: f.StreamSimple}, Hooks: &settings})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	s.core.agent.Subscribe(func(_ context.Context, event agent.AgentEvent) error {
		if event.Type == agent.EventToolPermission {
			return errors.New("crash after preparation")
		}
		return nil
	})
	_, _ = s.Prompt(context.Background(), Prompt{Text: "write"})
	loaded, err := journal.Load(context.Background(), s.core.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Interrupted) != 1 || !strings.Contains(string(loaded.Interrupted[0].Args), `"rewritten"`) {
		t.Fatalf("effective input lost: %+v", loaded.Interrupted)
	}
	if _, err := os.Stat(filepath.Join(cwd, "rewritten")); !os.IsNotExist(err) {
		t.Fatalf("tool ran past interrupted preparation: %v", err)
	}
}
