package checkpoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest/durable"
)

func bindEmpty(manager *Manager) {
	manager.Bind(Runtime{Messages: func() []types.Message { return nil }, Changed: func([]types.Message) {}})
}
func writeWorkspace(t *testing.T, manager *Manager, path, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(manager.cwd, path), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func observe(t *testing.T, manager *Manager) {
	t.Helper()
	files, err := manager.scan(t.Context(), manager.state.Expected)
	if err != nil {
		t.Fatal(err)
	}
	manager.state.Expected = files
}

func TestCaptureFailureNeverChangesToolResult(t *testing.T) {
	for _, post := range []bool{false, true} {
		t.Run(fmt.Sprint("post=", post), func(t *testing.T) {
			manager, _ := testManager(t)
			bindEmpty(manager)
			saved, err := manager.Create(t.Context(), "before gap", nil)
			if err != nil {
				t.Fatal(err)
			}
			originalPath := os.Getenv("PATH")
			missingGit := t.TempDir()
			wantErr := errors.New("original tool error")
			wantResult := agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("original bytes")}, Details: json.RawMessage(`{"original":true}`)}
			ran := false
			tool := agent.AgentTool{Tool: types.Tool{Name: "bash"}, Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
				ran = true
				writeWorkspace(t, manager, "effect", "ran")
				if post {
					t.Setenv("PATH", missingGit)
				}
				return wantResult, wantErr
			}}
			if !post {
				t.Setenv("PATH", missingGit)
			}
			result, err := manager.Wrap([]agent.AgentTool{tool}, func(string) bool { return true })[0].Execute(t.Context(), "call", nil, nil)
			if !ran || err != wantErr || !reflect.DeepEqual(result, wantResult) {
				t.Fatalf("ran=%v result=%+v error=%v", ran, result, err)
			}
			t.Setenv("PATH", originalPath)
			if _, err := manager.Rewind(t.Context(), saved.ID, "", true); err == nil || !strings.Contains(err.Error(), "capture gap") {
				t.Fatalf("gap crossed: %v", err)
			}
			if manager.state.Uncertain {
				t.Fatal("session stuck uncertain")
			}
			if _, err := manager.Create(t.Context(), "after gap", nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInterruptedCallDoesNotBrickResume(t *testing.T) {
	manager, store := testManager(t)
	bindEmpty(manager)
	saved, err := manager.Create(t.Context(), "before crash", nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.state.Uncertain = true
	if err := persist(manager, "state.json", manager.state); err != nil {
		t.Fatal(err)
	}
	writeWorkspace(t, manager, "crashed-write", "preserved")
	reopened, err := New(t.Context(), manager.cwd, store, manager.queue)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.state.Uncertain {
		t.Fatal("resume remained uncertain")
	}
	if _, err := reopened.Rewind(t.Context(), saved.ID, "", true); err == nil || !strings.Contains(err.Error(), "capture gap") {
		t.Fatalf("interruption gap crossed: %v", err)
	}
	if _, err := reopened.Create(t.Context(), "new baseline", nil); err != nil {
		t.Fatal(err)
	}
}

func TestMutatingCallsRunConcurrentlyAndListDoesNotWait(t *testing.T) {
	manager, _ := testManager(t)
	bindEmpty(manager)
	entered := make(chan string, 2)
	release := make(chan struct{})
	tool := agent.AgentTool{Tool: types.Tool{Name: "bash"}, Execute: func(_ context.Context, id string, _ json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		entered <- id
		<-release
		return agent.AgentToolResult{}, nil
	}}
	wrapped := manager.Wrap([]agent.AgentTool{tool}, func(string) bool { return true })[0]
	done := make(chan error, 2)
	for _, id := range []string{"first", "second"} {
		go func() { _, err := wrapped.Execute(t.Context(), id, nil, nil); done <- err }()
	}
	defer close(release)
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("tool execution serialized by checkpoint lock")
		}
	}
	listed := make(chan struct{})
	go func() { manager.List(); close(listed) }()
	select {
	case <-listed:
	case <-time.After(time.Second):
		t.Fatal("list waited on running tool")
	}
	// Let the deferred release happen before cleanup closes the journal.
	t.Cleanup(func() {
		for range 2 {
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
	})
}

func TestOnlyFileMutatorsAreWrapped(t *testing.T) {
	for _, name := range []string{"jobs", "cancel", "task", "message", "todo_write", "checkpoint", "rewind"} {
		if MutatesFiles(name, false) {
			t.Fatalf("orchestration tool %s wrapped", name)
		}
	}
	for _, name := range []string{"read", "wait", "logs", "lsp_rename", "ast_grep", "gh"} {
		if MutatesFiles(name, true) {
			t.Fatalf("read tool %s wrapped", name)
		}
	}
	for _, name := range []string{"bash", "eval", "write", "edit", "hashline", "integrate", "write_stdin", "mcp__test__call", "configured"} {
		if !MutatesFiles(name, false) {
			t.Fatalf("mutator %s not wrapped", name)
		}
	}
}

func TestDefaultRewindKeepsExplicitCheckpointsSeparate(t *testing.T) {
	manager, _ := testManager(t)
	bindEmpty(manager)
	writeWorkspace(t, manager, "file", "original")
	explicit, err := manager.Create(t.Context(), "investigate", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range maxCheckpoints + 1 {
		writeWorkspace(t, manager, "file", fmt.Sprint(i))
		if _, err := manager.capture(t.Context(), "automatic", nil, true); err != nil {
			t.Fatal(err)
		}
	}
	target, err := manager.target("")
	if err != nil || target.ID != explicit.ID {
		t.Fatalf("default target=%s err=%v", target.ID, err)
	}
	if len(manager.List()) != maxCheckpoints+1 {
		t.Fatal("automatic retention evicted explicit checkpoint")
	}
	if _, err := manager.Rewind(t.Context(), "", "findings", true); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(manager.cwd, "file"))
	if err != nil || string(content) != "original" {
		t.Fatalf("default rewind=%q %v", content, err)
	}
}

func TestRewindToolRefusalIsReturnedBeforeScheduling(t *testing.T) {
	manager, _ := testManager(t)
	bindEmpty(manager)
	writeWorkspace(t, manager, "file", "original")
	if _, err := manager.Create(t.Context(), "save", nil); err != nil {
		t.Fatal(err)
	}
	writeWorkspace(t, manager, "file", "outside")
	if _, err := manager.Tool("rewind").Execute(t.Context(), "rewind", []byte(`{"report":"findings"}`), nil); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("outside edit accepted: %v", err)
	}
	if manager.request != nil || manager.state.RewindRequest != "" {
		t.Fatal("refusal left a request")
	}
	if _, err := manager.Tool("rewind").Execute(t.Context(), "rewind", []byte(`{"report":"findings","force":true}`), nil); err == nil {
		t.Fatal("model could force outside edits")
	}
}

func TestPrepareRefusalContinuesAndClearsRequest(t *testing.T) {
	manager, store := testManager(t)
	bindEmpty(manager)
	writeWorkspace(t, manager, "file", "original")
	if _, err := manager.Create(t.Context(), "save", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Tool("rewind").Execute(t.Context(), "rewind", []byte(`{"report":"findings"}`), nil); err != nil {
		t.Fatal(err)
	}
	writeWorkspace(t, manager, "file", "racing outside edit")
	update, err := manager.Prepare(t.Context(), agent.ShouldStopAfterTurnContext{})
	if err != nil || update == nil {
		t.Fatalf("refusal killed run: %v", err)
	}
	if manager.state.RewindRequest != "" || manager.request != nil {
		t.Fatal("refusal retained request")
	}
	loaded, err := journal.Load(t.Context(), store.Path())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(loaded.Messages)
	if err != nil || !strings.Contains(string(encoded), "Rewind refused") {
		t.Fatalf("refusal not journaled: %s %v", encoded, err)
	}
}

func TestRewindAppendFailureRollsBackAndRetiresPending(t *testing.T) {
	manager, store := testManager(t)
	writeWorkspace(t, manager, "file", "original")
	saved, err := manager.Create(t.Context(), "save", nil)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspace(t, manager, "file", "agent edit")
	observe(t, manager)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Rewind(t.Context(), saved.ID, "", false); err == nil {
		t.Fatal("closed journal accepted rewind")
	}
	content, err := os.ReadFile(filepath.Join(manager.cwd, "file"))
	if err != nil || string(content) != "agent edit" {
		t.Fatalf("append failure lost workspace: %q %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(manager.dir, "pending.json")); !os.IsNotExist(err) {
		t.Fatalf("failed rewind left pending: %v", err)
	}
}

func TestRewindObjectFailureDoesNotLeavePending(t *testing.T) {
	manager, _ := testManager(t)
	writeWorkspace(t, manager, "file", "original")
	saved, err := manager.Create(t.Context(), "save", nil)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspace(t, manager, "file", "agent edit")
	observe(t, manager)
	version := manager.state.Snapshots[0].Files["file"]
	version.Object = strings.Repeat("f", 40)
	manager.state.Snapshots[0].Files["file"] = version
	if _, err := manager.Rewind(t.Context(), saved.ID, "", false); err == nil {
		t.Fatal("missing object accepted")
	}
	if _, err := os.Stat(filepath.Join(manager.dir, "pending.json")); !os.IsNotExist(err) {
		t.Fatalf("preparation failure left pending: %v", err)
	}
}

func TestCheckpointConversationStoredAsSharedGitBlob(t *testing.T) {
	manager, _ := testManager(t)
	messages := []types.Message{types.UserMessage{Content: types.StringContent(strings.Repeat("x", 1<<20))}}
	for range 3 {
		if _, err := manager.Create(t.Context(), "same conversation", messages); err != nil {
			t.Fatal(err)
		}
	}
	metadata, err := os.ReadFile(filepath.Join(manager.dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata) > 16<<10 || bytes.Contains(metadata, []byte(strings.Repeat("x", 100))) {
		t.Fatal("conversation copied into metadata")
	}
	if manager.state.Snapshots[0].Conversation != manager.state.Snapshots[2].Conversation {
		t.Fatal("conversation was not content-addressed")
	}
}

func TestNonGitRecreatedFilesStayVisible(t *testing.T) {
	manager, _ := testManager(t)
	writeWorkspace(t, manager, "a", "a")
	writeWorkspace(t, manager, "b", "b")
	if _, err := manager.Create(t.Context(), "first", nil); err != nil {
		t.Fatal(err)
	}
	delete(manager.state.Expected, "b")
	if _, err := manager.Create(t.Context(), "second", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.state.Snapshots[1].Files["b"]; !ok {
		t.Fatal("private index hid file absent from Expected")
	}
}

func TestGlobalIgnoreAndNestedRepositoryArePreserved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	ignore := filepath.Join(home, "ignore")
	if err := os.WriteFile(ignore, []byte("globally-ignored\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\nexcludesFile = "+ignore+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manager, _ := testManager(t)
	writeWorkspace(t, manager, "globally-ignored", "user bytes")
	nested := filepath.Join(manager.cwd, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "init", "--template=", nested)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", output, err)
	}
	if err := os.WriteFile(filepath.Join(nested, "user"), []byte("nested bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	saved, err := manager.Create(t.Context(), "skip nested", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(manager.state.Snapshots[0].Files) != 0 || saved.Skipped["nested/"] == "" && saved.Skipped["nested"] == "" {
		t.Fatalf("ignored or nested captured: %+v", saved)
	}
	if _, err := manager.Rewind(t.Context(), saved.ID, "", true); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(filepath.Join(manager.cwd, "globally-ignored")); err != nil || string(contents) != "user bytes" {
		t.Fatalf("global ignore lost: %q %v", contents, err)
	}
}

func TestGitAttributesDoNotTransformCheckpointBytes(t *testing.T) {
	manager, _ := testManager(t)
	writeWorkspace(t, manager, ".gitattributes", "* text eol=lf\n")
	writeWorkspace(t, manager, "file", "original\r\n")
	saved, err := manager.Create(t.Context(), "exact bytes", nil)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspace(t, manager, "file", "changed\n")
	observe(t, manager)
	if _, err := manager.Rewind(t.Context(), saved.ID, "", false); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(manager.cwd, "file"))
	if err != nil || string(contents) != "original\r\n" {
		t.Fatalf("attributes changed bytes: %q %v", contents, err)
	}
}

func TestMoreThanTenThousandFiles(t *testing.T) {
	manager, _ := testManager(t)
	const count = 10017
	for i := range count {
		writeWorkspace(t, manager, fmt.Sprintf("file-%05d", i), "content")
	}
	if _, err := manager.Create(t.Context(), "large tree", nil); err != nil {
		t.Fatal(err)
	}
	if got := len(manager.state.Snapshots[0].Files); got != count {
		t.Fatalf("captured %d of %d", got, count)
	}
}

func TestReadListDoesNotSettlePendingRequest(t *testing.T) {
	manager, store := testManager(t)
	if _, err := manager.Create(t.Context(), "save", nil); err != nil {
		t.Fatal(err)
	}
	manager.state.RewindRequest = "must-not-settle"
	if err := persist(manager, "state.json", manager.state); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadList(store.Path()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(store.Path())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read-only list changed journal")
	}
	metadata, err := os.ReadFile(filepath.Join(manager.dir, "state.json"))
	if err != nil || !bytes.Contains(metadata, []byte("must-not-settle")) {
		t.Fatal("read-only list changed metadata")
	}
}

// TestSnapshotTiming is opt-in because it reads a caller-selected real workspace.
func TestSnapshotTiming(t *testing.T) {
	cwd := os.Getenv("TEMPEST_CHECKPOINT_TIMING_CWD")
	if cwd == "" {
		t.Skip("set TEMPEST_CHECKPOINT_TIMING_CWD for snapshot measurement")
	}
	store, err := journal.Create(t.Context(), t.TempDir(), cwd, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := New(t.Context(), cwd, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		start := time.Now()
		saved, err := manager.Create(t.Context(), "snapshot timing", nil)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("snapshot=%d elapsed=%s files=%d skipped=%v", i, elapsed, len(manager.state.Snapshots[len(manager.state.Snapshots)-1].Files), saved.Skipped)
	}
}

func TestRewindStopsRuntimeBeforeRestoringFiles(t *testing.T) {
	manager, _ := testManager(t)
	writeWorkspace(t, manager, "file", "original")
	saved, err := manager.Create(t.Context(), "save", nil)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspace(t, manager, "file", "investigation")
	observe(t, manager)
	stopped := false
	manager.Bind(Runtime{Rewound: func() {
		contents, err := os.ReadFile(filepath.Join(manager.cwd, "file"))
		if err != nil || string(contents) != "investigation" {
			t.Fatalf("runtime stopped after restore: %q %v", contents, err)
		}
		stopped = true
	}})
	if _, err := manager.Rewind(t.Context(), saved.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Fatal("rewind retained runtime state")
	}
}

func TestPendingCommittedForkPreservesLaterConversation(t *testing.T) {
	manager, store := testManager(t)
	writeWorkspace(t, manager, "file", "original")
	saved, err := manager.Create(t.Context(), "save", nil)
	if err != nil {
		t.Fatal(err)
	}
	before := manager.state.Expected
	entry, err := journal.NewRewindEntry(saved.ID, "findings", []types.Message{types.UserMessage{Content: types.StringContent("retained")}})
	if err != nil {
		t.Fatal(err)
	}
	transaction := pending{Entry: entry, Before: before, After: before, JournalSeq: store.Position().Seq}
	if err := persist(manager, "pending.json", transaction); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(t.Context(), entry); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(t.Context(), types.UserMessage{Content: types.StringContent("newer user input")}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(t.Context(), manager.cwd, store, manager.queue); err != nil {
		t.Fatal(err)
	}
	loaded, err := journal.Load(t.Context(), store.Path())
	if err != nil || len(loaded.Messages) != 2 {
		t.Fatalf("recovery lost later conversation: %+v %v", loaded, err)
	}
}

func TestPendingLegacyEOFCompletesAfterMigration(t *testing.T) {
	manager, store := testManager(t)
	writeWorkspace(t, manager, "file", "original")
	saved, err := manager.Create(t.Context(), "save", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.AppendMessage(t.Context(), types.UserMessage{Content: types.StringContent("before rewind")}); err != nil {
		t.Fatal(err)
	}
	loaded, err := journal.Load(t.Context(), store.Path())
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.Read(t.Context(), durable.Query{Kind: durable.EntryKind, Namespace: "_tempest/journal", History: true})
	if err != nil {
		t.Fatal(err)
	}
	header := loaded.Header
	header.Version = 3
	raw, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	var legacy bytes.Buffer
	legacy.Write(raw)
	legacy.WriteByte('\n')
	for _, record := range page.Records {
		legacy.Write(record.Value)
		legacy.WriteByte('\n')
	}
	writeWorkspace(t, manager, "file", "investigation")
	observe(t, manager)
	before := manager.state.Expected
	after := manager.state.Snapshots[len(manager.state.Snapshots)-1].Files
	writeWorkspace(t, manager, "file", "original")
	entry, err := journal.NewRewindEntry(saved.ID, "findings", []types.Message{types.UserMessage{Content: types.StringContent("retained")}})
	if err != nil {
		t.Fatal(err)
	}
	transaction := pending{Entry: entry, Before: before, After: after, JournalOffset: int64(legacy.Len())}
	if err = persist(manager, "pending.json", transaction); err != nil {
		t.Fatal(err)
	}
	path := store.Path()
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, legacy.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		resumed, _, err := journal.OpenForResume(t.Context(), path, journal.Options{})
		if err != nil {
			t.Fatal(err)
		}
		recovered, err := New(t.Context(), manager.cwd, resumed, manager.queue)
		if err != nil {
			resumed.Close()
			t.Fatal(err)
		}
		loaded, err := resumed.Load(t.Context())
		if err != nil || len(loaded.Messages) != 1 {
			resumed.Close()
			t.Fatalf("legacy pending rewind abandoned: %+v %v", loaded, err)
		}
		message, ok := loaded.Messages[0].(types.UserMessage)
		if !ok || message.Content.Text != "retained" {
			resumed.Close()
			t.Fatalf("workspace/transcript diverged: %+v", loaded.Messages)
		}
		if recovered.state.Uncertain {
			resumed.Close()
			t.Fatal("successful migrated recovery marked uncertain")
		}
		if err = resumed.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiscardRequestPreventsLaterTurnRewind(t *testing.T) {
	manager, _ := testManager(t)
	bindEmpty(manager)
	if _, err := manager.Create(t.Context(), "save", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Tool("rewind").Execute(t.Context(), "rewind", []byte(`{"report":"findings"}`), nil); err != nil {
		t.Fatal(err)
	}
	manager.DiscardRequest()
	if manager.request != nil || manager.state.RewindRequest != "" {
		t.Fatal("request survived run end")
	}
	update, err := manager.Prepare(t.Context(), agent.ShouldStopAfterTurnContext{})
	if update != nil || err != nil {
		t.Fatalf("abandoned rewind fired later: %+v %v", update, err)
	}
}

func TestFailedApplyRetiresPendingAndPreservesWorkspace(t *testing.T) {
	manager, _ := testManager(t)
	writeWorkspace(t, manager, "file", "original")
	saved, err := manager.Create(t.Context(), "save", nil)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspace(t, manager, "file", "agent edit")
	observe(t, manager)
	if err := os.Chmod(manager.cwd, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(manager.cwd, 0700)
	if _, err := manager.Rewind(t.Context(), saved.ID, "", false); err == nil {
		t.Fatal("unwritable workspace accepted restore")
	}
	if _, err := os.Stat(filepath.Join(manager.dir, "pending.json")); !os.IsNotExist(err) {
		t.Fatalf("failed apply left pending: %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(manager.cwd, "file"))
	if err != nil || string(contents) != "agent edit" {
		t.Fatalf("failed apply changed bytes: %q %v", contents, err)
	}
}

func TestRefusedPrepareDoesNotBecomeInterruptedOnResume(t *testing.T) {
	manager, store := testManager(t)
	bindEmpty(manager)
	writeWorkspace(t, manager, "file", "original")
	if _, err := manager.Create(t.Context(), "save", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Tool("rewind").Execute(t.Context(), "rewind", []byte(`{"report":"findings"}`), nil); err != nil {
		t.Fatal(err)
	}
	writeWorkspace(t, manager, "file", "outside")
	if _, err := manager.Prepare(t.Context(), agent.ShouldStopAfterTurnContext{}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(t.Context(), manager.cwd, store, manager.queue); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(store.Path())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refused prepare produced a false interruption notice")
	}
}

func TestRestoreApprovalIsPerModelCallAndOutsideLocks(t *testing.T) {
	manager, _ := testManager(t)
	if err := os.Mkdir(filepath.Join(manager.cwd, ".harness"), 0700); err != nil {
		t.Fatal(err)
	}
	writeWorkspace(t, manager, ".harness/settings.json", "original")
	saved, err := manager.Create(t.Context(), "save settings", nil)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspace(t, manager, ".harness/settings.json", "investigation")
	observe(t, manager)
	asks := 0
	manager.Bind(Runtime{
		Messages: func() []types.Message { return nil },
		Changed:  func([]types.Message) {},
		ApproveRestore: func(_ context.Context, callID string, paths []string) error {
			asks++
			if callID != "restore-settings" || !reflect.DeepEqual(paths, []string{".harness/settings.json"}) {
				t.Fatalf("approval call=%s paths=%v", callID, paths)
			}
			manager.List()
			return os.ErrPermission
		},
	})
	if _, err := manager.Tool("rewind").Execute(t.Context(), "restore-settings", []byte(`{"report":"findings"}`), nil); err != nil {
		t.Fatal(err)
	}
	if update, err := manager.Prepare(t.Context(), agent.ShouldStopAfterTurnContext{}); err != nil || update == nil {
		t.Fatalf("approval refusal killed run: %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(manager.cwd, ".harness/settings.json"))
	if err != nil || string(contents) != "investigation" {
		t.Fatal("unapproved restore changed settings")
	}
	if asks != 1 {
		t.Fatalf("asks=%d", asks)
	}
	if _, err := manager.Rewind(t.Context(), saved.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if asks != 1 {
		t.Fatal("explicit operator action unexpectedly used model approval")
	}
}

func TestWorkspaceLockDoesNotFollowSymlinks(t *testing.T) {
	manager, _ := testManager(t)
	if _, err := manager.Create(t.Context(), "initialize lock", nil); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(os.TempDir(), fmt.Sprintf("tempest-locks-%d", os.Getuid()), fmt.Sprintf("%x.lock", sha256.Sum256([]byte(manager.cwd))))
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, lockPath); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(lockPath)
	if err := manager.lock(func() error { return nil }); err == nil {
		t.Fatal("workspace lock followed symlink")
	}
	contents, err := os.ReadFile(outside)
	if err != nil || string(contents) != "untouched" {
		t.Fatal("lock altered symlink target")
	}
}

func TestCommittedRewindSurvivesMetadataWriteFailure(t *testing.T) {
	manager, store := testManager(t)
	writeWorkspace(t, manager, "file", "original")
	saved, err := manager.Create(t.Context(), "save", nil)
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspace(t, manager, "file", "investigation")
	observe(t, manager)
	statePath := filepath.Join(manager.dir, "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Rewind(t.Context(), saved.ID, "committed finding", false); err != nil {
		t.Fatalf("committed rewind reported failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(manager.dir, "pending.json")); err != nil {
		t.Fatal("lost pending recovery metadata")
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(t.Context(), types.UserMessage{Content: types.StringContent("newer input")}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(t.Context(), manager.cwd, store, manager.queue); err != nil {
		t.Fatal(err)
	}
	loaded, err := journal.Load(t.Context(), store.Path())
	if err != nil || len(loaded.Messages) != 2 {
		t.Fatalf("metadata recovery discarded later input: %+v %v", loaded, err)
	}
}

func TestASTPreviewSkipsCaptureButApplyIsRewindable(t *testing.T) {
	manager, _ := testManager(t)
	bindEmpty(manager)
	writeWorkspace(t, manager, "source.js", "before\n")
	saved, err := manager.Create(t.Context(), "before AST", nil)
	if err != nil {
		t.Fatal(err)
	}
	tool := agent.AgentTool{Tool: types.Tool{Name: "ast_edit"}, Execute: func(_ context.Context, _ string, args json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		var input struct {
			Apply bool `json:"apply"`
		}
		if err := json.Unmarshal(args, &input); err != nil {
			return agent.AgentToolResult{}, err
		}
		if input.Apply {
			writeWorkspace(t, manager, "source.js", "after\n")
		}
		return agent.AgentToolResult{}, nil
	}}
	wrapped := manager.Wrap([]agent.AgentTool{tool}, func(string) bool { return true })[0]
	before := len(manager.List())
	if _, err := wrapped.Execute(t.Context(), "preview", json.RawMessage(`{}`), nil); err != nil {
		t.Fatal(err)
	}
	if len(manager.List()) != before || manager.state.Uncertain {
		t.Fatal("preview captured a mutating call")
	}
	if _, err := wrapped.Execute(t.Context(), "apply", json.RawMessage(`{"apply":true}`), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Rewind(t.Context(), saved.ID, "", true); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(manager.cwd, "source.js"))
	if err != nil || string(content) != "before\n" {
		t.Fatalf("AST apply was not rewindable: %q %v", content, err)
	}
}

func TestGitHubCheckoutInvalidatesUnrestorableCheckpoint(t *testing.T) {
	manager, _ := testManager(t)
	bindEmpty(manager)
	saved, err := manager.Create(t.Context(), "before worktree", nil)
	if err != nil {
		t.Fatal(err)
	}
	tool := agent.AgentTool{Tool: types.Tool{Name: "gh_checkout"}, Execute: func(context.Context, string, json.RawMessage, agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		return agent.AgentToolResult{}, nil
	}}
	if _, err := manager.Wrap([]agent.AgentTool{tool}, func(string) bool { return true })[0].Execute(t.Context(), "checkout", json.RawMessage(`{}`), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Rewind(t.Context(), saved.ID, "", true); err == nil || !strings.Contains(err.Error(), "capture gap") {
		t.Fatalf("rewind pretends to restore Git metadata: %v", err)
	}
}
