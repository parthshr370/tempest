package tempest

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/engine/stream"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/durable"
)

// requestRecorder keeps the first user message of every provider request.
type requestRecorder struct {
	mu    sync.Mutex
	first []string
}

func (r *requestRecorder) stream(f *faux.Faux) agent.StreamFn {
	return func(ctx context.Context, model types.Model, request types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
		raw, err := json.Marshal(request.Messages[0])
		r.mu.Lock()
		if err != nil {
			r.first = append(r.first, "marshal: "+err.Error())
		} else {
			r.first = append(r.first, string(raw))
		}
		r.mu.Unlock()
		return f.StreamSimple(ctx, model, request, options)
	}
}

func (r *requestRecorder) firstMessages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.first...)
}

func gitWorkspace(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if output, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	return dir
}

func journaledListing(t *testing.T, s *Session) string {
	t.Helper()
	page, err := s.core.store.Read(context.Background(), durable.Query{Kind: durable.DocumentKind, Namespace: "_tempest/agent", ID: "selection", Limit: 1})
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("agent selection: %v %d", err, len(page.Records))
	}
	choice, err := decodeAgentSelection(page.Records[0].Value)
	if err != nil {
		t.Fatal(err)
	}
	return choice.WorkspaceListing
}

// A new session journals its listing; resume sends the same first-message
// bytes from the journal even after the workspace changed, never a new
// listing.
func TestWorkspaceListingIsJournaledAndReusedOnResume(t *testing.T) {
	cwd := gitWorkspace(t, "main.go", "README.md")
	sessions := t.TempDir()
	f := faux.New(faux.Options{})
	recorder := &requestRecorder{}
	f.SetResponses(faux.Respond(faux.Text("first")))
	s, err := Open(context.Background(), Options{SessionDir: sessions, Cwd: cwd, New: &NewSession{}, Stack: &StackConfig{Model: f.Model(), StreamFn: recorder.stream(f)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(context.Background(), Prompt{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	listing := journaledListing(t, s)
	if !strings.HasPrefix(listing, `<workspace_files source="git ls-files" paths="2">`) || !strings.Contains(listing, "\nREADME.md\nmain.go\n") {
		t.Fatalf("journaled listing:\n%s", listing)
	}
	path := s.core.store.Path()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "added_after_start.go"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	f.SetResponses(faux.Respond(faux.Text("second")))
	resumed, err := Open(context.Background(), Options{Resume: &ResumeSession{Path: path}, SessionDir: sessions, Stack: &StackConfig{Model: f.Model(), StreamFn: recorder.stream(f)}})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close(context.Background())
	if _, err := resumed.Prompt(context.Background(), Prompt{Text: "again"}); err != nil {
		t.Fatal(err)
	}
	first := recorder.firstMessages()
	if len(first) != 2 || first[0] != first[1] {
		t.Fatalf("first message differs across resume:\n%s", strings.Join(first, "\n"))
	}
	var user types.UserMessage
	if err := json.Unmarshal([]byte(first[1]), &user); err != nil {
		t.Fatal(err)
	}
	if len(user.Content.Blocks) != 2 || user.Content.Blocks[0].Text != listing || user.Content.Blocks[1].Text != "hello" {
		t.Fatalf("resumed first message = %s", first[1])
	}
	if strings.Contains(first[1], "added_after_start.go") || journaledListing(t, resumed) != listing {
		t.Fatal("resume re-listed the workspace")
	}
}

func TestWorkspaceListingOffSwitchSendsNoListing(t *testing.T) {
	cwd := gitWorkspace(t, "main.go")
	f := faux.New(faux.Options{})
	recorder := &requestRecorder{}
	f.SetResponses(faux.Respond(faux.Text("ok")))
	stack := &StackConfig{Model: f.Model(), StreamFn: recorder.stream(f), ListingPolicy: config.WorkspaceListing{Disabled: true}}
	s, err := Open(context.Background(), Options{SessionDir: t.TempDir(), Cwd: cwd, New: &NewSession{}, Stack: stack})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if _, err := s.Prompt(context.Background(), Prompt{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	first := recorder.firstMessages()
	if len(first) != 1 || strings.Contains(first[0], "workspace_files") || journaledListing(t, s) != "" {
		t.Fatalf("listing sent with workspace_listing off: %v", first)
	}
}

func TestWorkspaceListingSettingTurnsOffHostSessions(t *testing.T) {
	t.Setenv("HARNESS_WORKSPACE_LISTING", "false")
	script := filepath.Join(t.TempDir(), "faux.json")
	if err := os.WriteFile(script, []byte(`[{"text":"answer"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	host, err := OpenHost(t.Context(), HostOptions{SessionDir: t.TempDir(), FauxScript: script})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	s, err := host.Create(t.Context(), AgentOptions{Cwd: gitWorkspace(t, "main.go")})
	if err != nil {
		t.Fatal(err)
	}
	if listing := journaledListing(t, s); listing != "" {
		t.Fatalf("listing taken with HARNESS_WORKSPACE_LISTING=false:\n%s", listing)
	}
}

// A fork of the same workspace carries the parent's journaled listing.
func TestWorkspaceListingForkKeepsParentSnapshot(t *testing.T) {
	t.Setenv("HARNESS_WORKSPACE_LISTING", "true")
	script := filepath.Join(t.TempDir(), "faux.json")
	if err := os.WriteFile(script, []byte(`[{"text":"answer"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	host, err := OpenHost(t.Context(), HostOptions{SessionDir: t.TempDir(), FauxScript: script})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	cwd := gitWorkspace(t, "main.go")
	parent, err := host.Create(t.Context(), AgentOptions{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parent.Prompt(t.Context(), Prompt{Text: "inherited"}); err != nil {
		t.Fatal(err)
	}
	listing := journaledListing(t, parent)
	if !strings.Contains(listing, "\nmain.go\n") {
		t.Fatalf("parent listing:\n%s", listing)
	}
	if err := os.WriteFile(filepath.Join(cwd, "added_before_fork.go"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	page, err := parent.Read(t.Context(), durable.Query{Kind: durable.EntryKind, Namespace: "_tempest/journal", Reverse: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	cut := ""
	for _, record := range page.Records {
		var entry struct{ Type, ID string }
		if err := json.Unmarshal(record.Value, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Type == "message" {
			cut = entry.ID
			break
		}
	}
	child, err := host.Fork(t.Context(), parent.ID(), cut, ForkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := journaledListing(t, child); got != listing {
		t.Fatalf("fork listing:\n%s\nparent:\n%s", got, listing)
	}
	placed := child.core.agent.TransformContext(t.Context(), child.core.agent.State().Messages)
	user, ok := placed[0].(types.UserMessage)
	if !ok || len(user.Content.Blocks) == 0 || user.Content.Blocks[0].Text != listing {
		t.Fatalf("fork first message = %#v", placed[0])
	}
}

// delegateOnce opens a session in cwd, runs change after the session took its
// listing, then has the session delegate one general task. It returns the
// parent's journaled listing, the leading text block of the child's first
// provider request, and the child job's isolation.
func delegateOnce(t *testing.T, cwd string, change func()) (parentListing, childLeading, isolation string) {
	t.Helper()
	t.Setenv("HARNESS_WORKSPACE_LISTING", "true")
	root, child := faux.New(faux.Options{}), faux.New(faux.Options{})
	root.SetResponses(faux.Respond(faux.ToolCall("task", json.RawMessage(`{"prompt":"child probe","subagent_type":"general"}`), "task1")), faux.Respond(faux.Text("root")), faux.Respond(faux.Text("root done")))
	captured := make(chan string, 1)
	child.SetResponses(func(request types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
		leading := ""
		if user, ok := request.Messages[0].(types.UserMessage); ok && len(user.Content.Blocks) > 0 {
			leading = user.Content.Blocks[0].Text
		}
		captured <- leading
		return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("child done")}}, nil
	})
	dispatch := func(ctx context.Context, model types.Model, request types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
		if strings.Contains(request.SystemPrompt, "You are a delegated child.") {
			return child.StreamSimple(ctx, model, request, options)
		}
		return root.StreamSimple(ctx, model, request, options)
	}
	owner, err := Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: cwd, New: &NewSession{PermissionMode: permission.ModeBypass}, Stack: &StackConfig{Model: root.Model(), StreamFn: dispatch, EnableTask: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	change()
	if _, err := owner.Prompt(t.Context(), Prompt{Text: "delegate"}); err != nil {
		t.Fatal(err)
	}
	select {
	case childLeading = <-captured:
	default:
		t.Fatal("child never ran")
	}
	jobs, err := owner.Jobs(t.Context(), "", 10)
	if err != nil || len(jobs.Jobs) != 1 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	details, err := owner.WaitJobs(t.Context(), []string{jobs.Jobs[0].ID}, 0)
	if err != nil || len(details) != 1 {
		t.Fatalf("details=%+v err=%v", details, err)
	}
	return journaledListing(t, owner), childLeading, details[0].Isolation
}

// An isolated worktree child lists its own worktree: a file committed after
// the parent took its listing is in the child's base commit and its listing.
func TestWorkspaceListingWorktreeChildListsItsOwnWorktree(t *testing.T) {
	cwd := gitWorkspace(t, "original.go")
	git := func(args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", cwd, "-c", "user.name=listing", "-c", "user.email=listing@example.com"}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, output)
		}
	}
	git("add", ".")
	git("commit", "-qm", "initial")
	parentListing, childLeading, isolation := delegateOnce(t, cwd, func() {
		if err := os.WriteFile(filepath.Join(cwd, "new_after_parent.go"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", ".")
		git("commit", "-qm", "after the parent's listing")
	})
	if isolation != "worktree" {
		t.Fatalf("isolation = %q", isolation)
	}
	if strings.Contains(parentListing, "new_after_parent.go") {
		t.Fatalf("parent listing was retaken:\n%s", parentListing)
	}
	if !strings.HasPrefix(childLeading, "<workspace_files ") || !strings.Contains(childLeading, "\nnew_after_parent.go\n") || !strings.Contains(childLeading, "\noriginal.go\n") {
		t.Fatalf("child listing is not its worktree's:\n%s", childLeading)
	}
}

// A child sharing the parent's directory sends the parent's listing bytes.
func TestWorkspaceListingSharedChildReusesParentBytes(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "original.go"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	parentListing, childLeading, isolation := delegateOnce(t, cwd, func() {
		if err := os.WriteFile(filepath.Join(cwd, "new_after_parent.go"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if isolation != "shared" {
		t.Fatalf("isolation = %q", isolation)
	}
	if parentListing == "" || childLeading != parentListing {
		t.Fatalf("shared child listing:\n%s\nparent:\n%s", childLeading, parentListing)
	}
}

// A worktree child journals its listing when its journal is created, and a
// resumed child sends those bytes even after its worktree changed.
func TestWorkspaceListingWorktreeChildResumeReusesJournaledBytes(t *testing.T) {
	const childID = "0123456789abcdef"
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "a.go"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	parentPath := filepath.Join(t.TempDir(), "parent.jsonl")
	cfg := StackConfig{WorkspaceListing: "parent bytes"}
	job := work.Job{Isolation: "worktree", Cwd: worktree}
	core := &sessionCore{logger: slog.Default()}

	store, err := journal.CreateChild(t.Context(), parentPath, childID, worktree, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := core.childWorkspaceListing(t.Context(), cfg, job, store, false)
	if closeErr := store.Close(); err != nil || closeErr != nil {
		t.Fatalf("listing: %v; close: %v", err, closeErr)
	}
	if !strings.Contains(first, "\na.go\n") {
		t.Fatalf("child listing:\n%s", first)
	}
	if err := os.WriteFile(filepath.Join(worktree, "b.go"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resumed, _, err := journal.OpenForResume(t.Context(), store.Path(), journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if again, err := core.childWorkspaceListing(t.Context(), cfg, job, resumed, true); err != nil || again != first {
		t.Fatalf("resumed child listing (%v):\n%s\nfirst:\n%s", err, again, first)
	}
	if shared, err := core.childWorkspaceListing(t.Context(), cfg, work.Job{Isolation: "shared", Cwd: worktree}, resumed, false); err != nil || shared != cfg.WorkspaceListing {
		t.Fatalf("shared child listing = %q, %v", shared, err)
	}
}
