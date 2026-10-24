package tempest

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/prompt"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
)

func checkpointGit(t *testing.T, cwd string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	b, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("git %v: %v: %s", args, e, b)
	}
	return b
}
func checkpointSession(t *testing.T, cwd, dir string, f *faux.Faux) *Session {
	t.Helper()
	s, e := Open(context.Background(), Options{Cwd: cwd, SessionDir: dir, New: &NewSession{PermissionMode: PermissionMode("bypass")}, Stack: &StackConfig{Model: f.Model(), StreamFn: f.StreamSimple, ContextFiles: []prompt.ContextFile{}}})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := s.Close(context.Background()); e != nil {
			t.Error(e)
		}
	})
	return s
}
func TestCheckpointRewindPreservesGitAndForksConversation(t *testing.T) {
	ctx := context.Background()
	cwd, dir := t.TempDir(), t.TempDir()
	checkpointGit(t, cwd, "init", "--template=")
	if e := os.WriteFile(filepath.Join(cwd, "a"), []byte("original\n"), 0755); e != nil {
		t.Fatal(e)
	}
	checkpointGit(t, cwd, "add", "a")
	checkpointGit(t, cwd, "-c", "user.name=Test", "-c", "user.email=test@example.org", "commit", "-qm", "initial")
	if e := os.WriteFile(filepath.Join(cwd, "staged"), []byte("index bytes\n"), 0644); e != nil {
		t.Fatal(e)
	}
	checkpointGit(t, cwd, "add", "staged")
	head := checkpointGit(t, cwd, "rev-parse", "HEAD")
	index, e := os.ReadFile(filepath.Join(cwd, ".git", "index"))
	if e != nil {
		t.Fatal(e)
	}
	f := faux.New(faux.Options{})
	f.AppendResponses(faux.Respond(faux.Text("before")))
	s := checkpointSession(t, cwd, dir, f)
	if _, e = s.Prompt(ctx, Prompt{Text: "checkpoint boundary"}); e != nil {
		t.Fatal(e)
	}
	cp, e := s.CreateCheckpoint(ctx, "save original")
	if e != nil {
		t.Fatal(e)
	}
	f.AppendResponses(faux.Respond(faux.ToolCall("bash", json.RawMessage(`{"command":"printf changed > a; chmod 600 a; printf created > new"}`), "change")), faux.Respond(faux.Text("discard me")))
	result, e := s.Prompt(ctx, Prompt{Text: "discard this turn"})
	if e != nil || result.StopReason != agent.StopCompleted {
		t.Fatalf("result=%+v err=%v", result, e)
	}
	before, e := os.ReadFile(s.core.store.Path())
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Rewind(ctx, RewindOptions{Checkpoint: cp.ID}); e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(filepath.Join(cwd, "a"))
	if e != nil || string(data) != "original\n" {
		t.Fatalf("a=%q err=%v", data, e)
	}
	info, e := os.Stat(filepath.Join(cwd, "a"))
	if e != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("mode=%v err=%v", info, e)
	}
	if _, e = os.Stat(filepath.Join(cwd, "new")); !os.IsNotExist(e) {
		t.Fatalf("created file survived: %v", e)
	}
	if !bytes.Equal(head, checkpointGit(t, cwd, "rev-parse", "HEAD")) {
		t.Fatal("HEAD changed")
	}
	got, e := os.ReadFile(filepath.Join(cwd, ".git", "index"))
	if e != nil || !bytes.Equal(index, got) {
		t.Fatalf("user index changed: %v", e)
	}
	after, e := os.ReadFile(s.core.store.Path())
	if e != nil || !bytes.HasPrefix(after, before) || !bytes.Contains(after, []byte(`"type":"rewind"`)) {
		t.Fatalf("journal was not append-only: %v", e)
	}
	loaded, e := journal.Load(ctx, s.core.store.Path())
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(loaded.Messages)
	if strings.Contains(string(raw), "discard this turn") || strings.Contains(string(raw), "discard me") {
		t.Fatalf("discarded branch visible: %s", raw)
	}
	f.AppendResponses(faux.Respond(faux.Text("resumed")))
	if _, e = s.Prompt(ctx, Prompt{Text: "new branch"}); e != nil {
		t.Fatal(e)
	}
	if e = s.Close(ctx); e != nil {
		t.Fatal(e)
	}
	resumed, e := Open(ctx, Options{Cwd: cwd, SessionDir: dir, Resume: &ResumeSession{Path: s.core.store.Path()}, Stack: &StackConfig{Model: f.Model(), StreamFn: f.StreamSimple}})
	if e != nil {
		t.Fatal(e)
	}
	defer resumed.Close(ctx)
	raw, _ = json.Marshal(resumed.core.agent.State().Messages)
	if strings.Contains(string(raw), "discard this turn") || !strings.Contains(string(raw), "new branch") {
		t.Fatalf("resume projection=%s", raw)
	}
}

func TestCheckpointOutsideChangesRequireForce(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	if e := os.WriteFile(filepath.Join(cwd, "a"), []byte("original"), 0644); e != nil {
		t.Fatal(e)
	}
	f := faux.New(faux.Options{})
	s := checkpointSession(t, cwd, t.TempDir(), f)
	cp, e := s.CreateCheckpoint(ctx, "before external edit")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(cwd, "a"), []byte("user edit"), 0600); e != nil {
		t.Fatal(e)
	}
	// A later agent edit must not make the earlier outside edit look agent-owned.
	f.AppendResponses(faux.Respond(faux.ToolCall("write", json.RawMessage(`{"path":"b","content":"agent"}`), "write")), faux.Respond(faux.Text("done")))
	if _, e = s.Prompt(ctx, Prompt{Text: "edit another file"}); e != nil {
		t.Fatal(e)
	}
	if e = s.Rewind(ctx, RewindOptions{Checkpoint: cp.ID}); e == nil || !strings.Contains(e.Error(), "outside") {
		t.Fatalf("outside edit accepted: %v", e)
	}
	b, _ := os.ReadFile(filepath.Join(cwd, "a"))
	if string(b) != "user edit" {
		t.Fatal("refused rewind changed bytes")
	}
	if e = s.Rewind(ctx, RewindOptions{Checkpoint: cp.ID, Force: true}); e != nil {
		t.Fatal(e)
	}
	b, _ = os.ReadFile(filepath.Join(cwd, "a"))
	if string(b) != "original" {
		t.Fatalf("forced bytes=%q", b)
	}
}

func TestCheckpointModelRewindAndIgnoredFiles(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	os.WriteFile(filepath.Join(cwd, ".gitignore"), []byte("ignored\n"), 0644)
	os.WriteFile(filepath.Join(cwd, "ignored"), []byte("keep"), 0644)
	f := faux.New(faux.Options{})
	f.AppendResponses(
		faux.Respond(faux.ToolCall("checkpoint", json.RawMessage(`{"goal":"investigate"}`), "save")),
		faux.Respond(faux.ToolCall("write", json.RawMessage(`{"path":"scratch","content":"temporary"}`), "write")),
		faux.Respond(faux.ToolCall("rewind", json.RawMessage(`{"report":"learned the answer"}`), "restore")),
		faux.Respond(faux.Text("retained finding")),
	)
	s := checkpointSession(t, cwd, t.TempDir(), f)
	r, e := s.Prompt(ctx, Prompt{Text: "investigate and rewind"})
	if e != nil || r.StopReason != agent.StopCompleted {
		t.Fatalf("result=%+v err=%v", r, e)
	}
	if _, e = os.Stat(filepath.Join(cwd, "scratch")); !os.IsNotExist(e) {
		t.Fatalf("scratch survived: %v", e)
	}
	b, _ := os.ReadFile(filepath.Join(cwd, "ignored"))
	if string(b) != "keep" {
		t.Fatalf("ignored bytes=%q", b)
	}
	loaded, e := journal.Load(ctx, s.core.store.Path())
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(loaded.Messages)
	if !strings.Contains(string(raw), "learned the answer") || strings.Contains(string(raw), "temporary") {
		t.Fatalf("rewound transcript=%s", raw)
	}
}

func TestCheckpointPreservesPreviouslyIgnoredFilesWhenRulesChange(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	for path, content := range map[string]string{".gitignore": "ignored\n", "ignored": "user bytes"} {
		if err := os.WriteFile(filepath.Join(cwd, path), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	provider := faux.New(faux.Options{})
	session := checkpointSession(t, cwd, t.TempDir(), provider)
	saved, err := session.CreateCheckpoint(ctx, "ignored baseline")
	if err != nil {
		t.Fatal(err)
	}
	provider.AppendResponses(faux.Respond(faux.ToolCall("write", json.RawMessage(`{"path":".gitignore","content":""}`), "change-ignore")), faux.Respond(faux.Text("done")))
	if _, err = session.Prompt(ctx, Prompt{Text: "change ignore rules"}); err != nil {
		t.Fatal(err)
	}
	if err = session.Rewind(ctx, RewindOptions{Checkpoint: saved.ID}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(cwd, "ignored"))
	if err != nil || string(content) != "user bytes" {
		t.Fatalf("previously ignored file lost: %q %v", content, err)
	}
}
