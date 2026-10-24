package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/editbatch"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/envlocal"
	"go.harness.dev/harness/internal/prompt"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/toolio"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/environment"
)

func environmentReviewStack() *StackConfig {
	provider := faux.New(faux.Options{})
	return &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple, ContextFiles: []prompt.ContextFile{}}
}

func TestLocalJournalPreservesSymlinkCwd(t *testing.T) {
	root, sessions := t.TempDir(), t.TempDir()
	real, link := filepath.Join(root, "real"), filepath.Join(root, "link")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), Options{Cwd: link, SessionDir: sessions, Stack: environmentReviewStack(), New: &NewSession{MCPServers: []MCPServerConfig{}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if s.Snapshot().Cwd != link {
		t.Fatalf("journal cwd = %q, want %q", s.Snapshot().Cwd, link)
	}
	recent, err := journal.Recent(t.Context(), sessions, link)
	if err != nil || recent != s.core.store.Path() {
		t.Fatalf("recent = %q, %v", recent, err)
	}
}

func TestLocalResumeAfterWorkspaceRecreation(t *testing.T) {
	root, sessions := t.TempDir(), t.TempDir()
	cwd := filepath.Join(root, "project")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	stack := environmentReviewStack()
	s, err := Open(t.Context(), Options{Cwd: cwd, SessionDir: sessions, Stack: stack, New: &NewSession{MCPServers: []MCPServerConfig{}}})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(cwd, filepath.Join(root, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	resumed, err := Open(t.Context(), Options{SessionDir: sessions, Stack: stack, Resume: &ResumeSession{SessionID: id, MCPServers: []MCPServerConfig{}}})
	if err != nil {
		t.Fatalf("local conversation must survive a re-clone: %v", err)
	}
	defer resumed.Close(context.Background())
}

func TestRemoteMCPFailsWithCapabilityError(t *testing.T) {
	workspace := t.TempDir()
	s, err := Open(t.Context(), Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), Stack: environmentReviewStack(), New: &NewSession{MCPServers: []MCPServerConfig{{Name: "host", Transport: "stdio", Command: "/bin/cat"}}}, Environment: environment.Reference{ID: "sdk-remote", Cwd: "."}, EnvironmentResolver: testEnvironmentResolver(t, &workspace, false, nil)})
	if s != nil {
		_ = s.Close(context.Background())
		t.Fatal("remote host MCP unexpectedly opened")
	}
	if !errors.Is(err, environment.ErrUnsupported) || !strings.Contains(err.Error(), "MCP") {
		t.Fatalf("expected clear capability error, got %v", err)
	}
}

func TestRemoteJournalWithoutReferenceNeverResolvesLocalPath(t *testing.T) {
	_, err := resolveEnvironment(t.Context(), Options{}, &StackConfig{}, "env://missing/.")
	if !errors.Is(err, environment.ErrIdentity) {
		t.Fatalf("missing remote identity: %v", err)
	}
}

type reviewCloseLease struct {
	environment.Lease
	closed atomic.Int32
}

func (l *reviewCloseLease) Close() error { l.closed.Add(1); return nil }
func TestTimedOutCloseRetiresRemoteLease(t *testing.T) {
	lease := &reviewCloseLease{}
	s := &Session{core: &sessionCore{environment: lease, active: &Run{done: make(chan struct{}), cancel: func() {}}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("close: %v", err)
	}
	if lease.closed.Load() != 1 {
		t.Fatal("timed-out close leaked its environment lease")
	}
}

type reviewLookupLease struct {
	environment.Lease
	ref     environment.Reference
	keys    []string
	outcome environment.Outcome
	calls   int
}

func (l *reviewLookupLease) Reference() environment.Reference { return l.ref }
func (l *reviewLookupLease) Lookup(_ context.Context, key string) (environment.Outcome, error) {
	l.keys = append(l.keys, key)
	return l.outcome, nil
}
func (l *reviewLookupLease) Call(context.Context, environment.Call) (json.RawMessage, error) {
	l.calls++
	return nil, errors.New("must not replay")
}

func TestRemoteRecoveryLooksUpDurableReceiptWithoutReplay(t *testing.T) {
	store, err := journal.Create(t.Context(), t.TempDir(), "env://receipt/.", journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ref := environment.Reference{ID: "receipt", Generation: "generation", Cwd: "."}
	meta := journal.Meta{RunID: "interrupted-run"}
	for _, entry := range []journal.Entry{
		journal.RunStartedRecord{Meta: meta, Model: "faux"},
		journal.MessageEntry{Meta: meta, Message: types.AssistantMessage{Content: []types.ContentBlock{types.NewToolCall("provider-id", "write", json.RawMessage(`{"path":"file","content":"effect"}`))}, StopReason: types.StopToolUse}},
		journal.ToolStartedRecord{Meta: meta, ToolCallID: "provider-id", Tool: "write", CallID: "occurrence:0", Environment: ref.Key()},
	} {
		if err := store.Append(t.Context(), entry); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(agent.AgentToolResult{Content: []types.ContentBlock{types.NewText("original durable result")}})
	lease := &reviewLookupLease{ref: ref, outcome: environment.Outcome{Found: true, Complete: true, Result: result}}
	loaded, err = recoverEnvironmentResults(t.Context(), lease, store, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Interrupted) != 0 || len(lease.keys) != 1 || lease.keys[0] != store.ID()+":occurrence:0" || lease.calls != 0 {
		t.Fatalf("recovery state: interrupted=%+v keys=%v calls=%d", loaded.Interrupted, lease.keys, lease.calls)
	}
	message, ok := loaded.Messages[len(loaded.Messages)-1].(types.ToolResultMessage)
	if !ok || len(message.Content) != 1 || message.Content[0].Text != "original durable result" {
		t.Fatalf("recovered result: %#v", loaded.Messages)
	}
	reloaded, err := store.Load(t.Context())
	if err != nil || len(reloaded.Interrupted) != 0 {
		t.Fatalf("receipt wasn't durable: %+v, %v", reloaded, err)
	}
}

func TestRollbackStateFollowsWorkspaceNotDaemonName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workspace := t.TempDir()
	for _, name := range []string{"first-name", "second-name"} {
		local, err := envlocal.NewNamed(workspace, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := bindLocalEnvironment(local); err != nil {
			t.Fatal(err)
		}
		if err := local.Close(); err != nil {
			t.Fatal(err)
		}
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := os.ReadDir(filepath.Join(configDir, "harness", "environments"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 {
		t.Fatalf("same workspace orphaned rollback state across daemon names: %v", dirs)
	}
}

func TestRollbackGenerationChangePreservesRecordsAndBlocksRecovery(t *testing.T) {
	state := t.TempDir()
	queue, err := toolio.NewProcessFileMutationQueue(filepath.Join(state, "locks"))
	if err != nil {
		t.Fatal(err)
	}
	if err := recoverEnvironmentState("old-generation", state, queue); err != nil {
		t.Fatal(err)
	}
	retained := filepath.Join(editbatch.Dir(state), "interrupted", "evidence")
	if err := os.MkdirAll(filepath.Dir(retained), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(retained, []byte("before replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := recoverEnvironmentState("new-generation", state, queue); !errors.Is(err, environment.ErrIdentity) {
		t.Fatalf("replacement recovery: %v", err)
	}
	if data, err := os.ReadFile(retained); err != nil || string(data) != "before replacement" {
		t.Fatalf("lost evidence: %q %v", data, err)
	}
}

func TestPersistedEnvironmentInspectionDoesNotAcquireWriter(t *testing.T) {
	ref := environment.Reference{ID: "inspect", Generation: "pinned", Cwd: "."}
	store, err := journal.Create(t.Context(), t.TempDir(), environment.Location(ref), journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := journal.EnvironmentReference(t.Context(), store.Path()); !errors.Is(err, environment.ErrIdentity) {
		t.Fatalf("unidentified remote journal: %v", err)
	}
	raw, _ := json.Marshal(struct {
		Environment environment.Reference `json:"environment"`
	}{ref})
	if _, err := store.Commit(t.Context(), durable.Commit{ID: "agent", Changes: []durable.Change{{SessionID: store.ID(), Kind: durable.DocumentKind, Namespace: "_tempest/agent", ID: "selection", Version: 1, Value: raw}}}); err != nil {
		t.Fatal(err)
	}
	inspected, err := journal.EnvironmentReference(t.Context(), store.Path())
	if err != nil || inspected != ref {
		t.Fatalf("persisted reference while writer is live: %+v, %v", inspected, err)
	}
}

func TestRollbackRetainedConflictsContinueToBlockStartup(t *testing.T) {
	state := t.TempDir()
	queue, err := toolio.NewProcessFileMutationQueue(filepath.Join(state, "locks"))
	if err != nil {
		t.Fatal(err)
	}
	conflict := filepath.Join(editbatch.Dir(state), editbatch.ConflictsDirName, "retained")
	if err := os.MkdirAll(conflict, 0700); err != nil {
		t.Fatal(err)
	}
	if err := recoverEnvironmentState("generation", state, queue); err == nil || !strings.Contains(err.Error(), "operator attention") {
		t.Fatalf("retained conflict disappeared from startup diagnostics: %v", err)
	}
}
