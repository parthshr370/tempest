package tempest

import (
	"context"
	"encoding/json"
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
	"go.harness.dev/harness/internal/provider/faux"
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
