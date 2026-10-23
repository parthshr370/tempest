package refs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/skills"
	"go.harness.dev/harness/internal/work"
)

const (
	taskJobID = "job_0123456789abcdef0123456789abcdef"
	bashJobID = "job_fedcba9876543210fedcba9876543210"
	childID   = "00000000000000c1"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		raw, want, code string
	}{
		{"local://", "local://", ""},
		{"local://a/./b/", "local://a/b", ""},
		{"local://a%20b.md", "local://a%20b.md", ""},
		{"skill://alpha", "skill://alpha", ""},
		{"skill://alpha/", "skill://alpha/", ""},
		{"skill://alpha/notes/x.md", "skill://alpha/notes/x.md", ""},
		{"job://", "job://", ""},
		{"job://" + taskJobID, "job://" + taskJobID, ""},
		{"job://" + taskJobID + "/", "job://" + taskJobID + "/", ""},
		{"job://" + taskJobID + "/output.log", "job://" + taskJobID + "/output.log", ""},
		{"history://" + childID, "history://" + childID, ""},
		{"history://" + taskJobID + "/", "history://" + taskJobID, ""},
		{"local://../x", "", CodeEscape},
		{"local://a/../../x", "", CodeEscape},
		{"skill://alpha/%2e%2e/x", "", CodeEscape},
		{"local:///etc/passwd", "", CodeInvalid},
		{`local://a\b`, "", CodeInvalid},
		{"local://a%00", "", CodeInvalid},
		{"local://a?q=1", "", CodeInvalid},
		{"local://a#x", "", CodeInvalid},
		{"local://%zz", "", CodeInvalid},
		{"skill://", "", CodeInvalid},
		{"job://job_1", "", CodeInvalid},
		{"job://../x", "", CodeInvalid},
		{"history://abc", "", CodeInvalid},
		{"history://" + childID + "/x", "", CodeInvalid},
		{"file:///etc/passwd", "", CodeInvalid},
	} {
		ref, err := Parse(tc.raw)
		if tc.code != "" {
			if !errors.Is(err, &Error{Code: tc.code}) {
				t.Errorf("Parse(%q) error = %v, want %s", tc.raw, err, tc.code)
			}
			continue
		}
		if err != nil || ref.String() != tc.want {
			t.Errorf("Parse(%q) = %q, %v; want %q", tc.raw, ref.String(), err, tc.want)
		}
	}
	if IsURL("skillz://x") || IsURL("README.md") || !IsURL("history://") {
		t.Fatal("IsURL misclassified a scheme")
	}
}

// skillFixture makes skill alpha with a body, a nested file, an escaping
// symlink and a FIFO.
func skillFixture(t *testing.T) (string, []skills.Skill) {
	t.Helper()
	base := filepath.Join(t.TempDir(), "alpha")
	outside := t.TempDir()
	write(t, filepath.Join(base, "SKILL.md"), "skill body\n")
	write(t, filepath.Join(base, "notes", "a b.md"), "note\n")
	write(t, filepath.Join(outside, "secret.md"), "secret\n")
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(base, "escape.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("SKILL.md", filepath.Join(base, "alias.md")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(base, "stream"), 0o600); err != nil {
		t.Fatal(err)
	}
	return base, []skills.Skill{{Name: "alpha", FilePath: filepath.Join(base, "SKILL.md"), BaseDir: base}}
}

func TestSkillURLs(t *testing.T) {
	base, items := skillFixture(t)
	resolver := mustNew(t, Config{Skills: items})
	ctx := context.Background()

	for uri, want := range map[string]string{"skill://alpha": "skill body\n", "skill://alpha/notes/a%20b.md": "note\n", "skill://alpha/alias.md": "skill body\n"} {
		resource, err := resolver.Read(ctx, uri)
		if err != nil || string(resource.Data) != want || resource.Dir {
			t.Fatalf("Read(%s) = %q, %v", uri, resource.Data, err)
		}
		if strings.Contains(resource.URI, base) {
			t.Fatalf("URI leaks the skill directory: %s", resource.URI)
		}
	}
	listing, err := resolver.Read(ctx, "skill://alpha/")
	if err != nil || !listing.Dir || !hasEntry(listing.Entries, "notes", true) || !hasEntry(listing.Entries, "SKILL.md", false) || hasEntry(listing.Entries, "escape.md", false) {
		t.Fatalf("skill directory listing = %+v, %v", listing, err)
	}

	for uri, code := range map[string]string{
		"skill://alpha/escape.md": CodeEscape,
		"skill://alpha/../x":      CodeEscape,
		"skill://alpha/missing":   CodeNotFound,
		"skill://beta":            CodeNotFound,
	} {
		if _, err := resolver.Read(ctx, uri); !errors.Is(err, &Error{Code: code}) {
			t.Fatalf("Read(%s) error = %v, want %s", uri, err, code)
		}
	}
	if _, err := resolver.Read(ctx, "skill://beta"); err == nil || !strings.Contains(err.Error(), "available: alpha") {
		t.Fatalf("unknown skill error should list skills: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := resolver.Read(ctx, "skill://alpha/stream")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, &Error{Code: CodeTarget}) {
			t.Fatalf("FIFO read error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reading a FIFO blocked")
	}

	if _, err := mustNew(t, Config{}).Read(ctx, "skill://alpha"); !errors.Is(err, &Error{Code: CodeUnavailable}) {
		t.Fatalf("skill without skills error = %v", err)
	}
}

func TestLocalURLs(t *testing.T) {
	rootJournal := filepath.Join(t.TempDir(), "root.jsonl")
	resolver := mustNew(t, Config{RootJournal: rootJournal})
	ctx := context.Background()

	location, err := resolver.LocalTarget("local://notes/today.md")
	if err != nil {
		t.Fatal(err)
	}
	if location.Root != filepath.Join(rootJournal+".d", LocalDirName) || location.Name != filepath.Join("notes", "today.md") {
		t.Fatalf("local target = %+v", location)
	}
	if info, err := os.Stat(location.Root); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("local directory = %v, %v; want private 0700", info, err)
	}
	write(t, location.Abs(), "hello\n")
	resource, err := resolver.Read(ctx, "local://notes/today.md")
	if err != nil || string(resource.Data) != "hello\n" {
		t.Fatalf("read back = %q, %v", resource.Data, err)
	}
	listing, err := resolver.Read(ctx, "local://")
	if err != nil || !listing.Dir || !hasEntry(listing.Entries, "notes", true) {
		t.Fatalf("local listing = %+v, %v", listing, err)
	}

	for uri, code := range map[string]string{"skill://alpha/x": CodeUnsupported, "job://": CodeUnsupported, "local://": CodeTarget, "local://../x": CodeEscape} {
		if _, err := resolver.LocalTarget(uri); !errors.Is(err, &Error{Code: code}) {
			t.Fatalf("LocalTarget(%s) error = %v, want %s", uri, err, code)
		}
	}

	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret"), "secret\n")
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(location.Root, "leak")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "missing"), filepath.Join(location.Root, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(location.Root, "outdir")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Read(ctx, "local://leak"); !errors.Is(err, &Error{Code: CodeEscape}) {
		t.Fatalf("read through escaping symlink error = %v", err)
	}
	for _, uri := range []string{"local://leak", "local://dangling", "local://outdir/new.txt"} {
		if _, err := resolver.HostPath(uri); !errors.Is(err, &Error{Code: CodeEscape}) {
			t.Fatalf("HostPath(%s) error = %v, want escape", uri, err)
		}
	}
	if path, err := resolver.HostPath("local://fresh/new.txt"); err != nil || path != filepath.Join(location.Root, "fresh", "new.txt") {
		t.Fatalf("HostPath for a new file = %q, %v", path, err)
	}

	// A local:// directory replaced by a symlink is refused outright.
	other := filepath.Join(t.TempDir(), "other.jsonl")
	if err := os.MkdirAll(other+".d", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(other+".d", LocalDirName)); err != nil {
		t.Fatal(err)
	}
	if _, err := mustNew(t, Config{RootJournal: other}).Read(ctx, "local://secret"); !errors.Is(err, &Error{Code: CodeEscape}) {
		t.Fatalf("symlinked local root error = %v", err)
	}

	for _, uri := range []string{"local://x", "job://", "history://"} {
		if _, err := mustNew(t, Config{}).Read(ctx, uri); !errors.Is(err, &Error{Code: CodeUnavailable}) {
			t.Fatalf("unjournaled Read(%s) error = %v", uri, err)
		}
	}
}

// sessionTree writes a root journal with a finished task job (child
// transcript included) and a finished bash job with an output log.
func sessionTree(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	cwd := t.TempDir()
	root, err := journal.Create(ctx, t.TempDir(), cwd, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fact := func(kind journal.EntryType, payload any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := root.AppendWorkFact(ctx, work.Fact{Kind: string(kind), RunID: "r", Payload: raw}); err != nil {
			t.Fatal(err)
		}
	}
	childJournal := root.Path() + ".d/children/" + childID + "/session.jsonl"
	fact(journal.KindJobStarted, journal.JobStarted{JobID: taskJobID, IdentityDigest: strings.Repeat("a", 64), CallID: "message:0", ToolCallID: "t1", LogicalRunID: "r", Kind: "task", Spec: json.RawMessage(`{"prompt":"survey","subagent_type":"explore","description":"survey the repo"}`), Environment: &journal.JobEnvironment{Model: "m", Provider: "p"}, ChildSessionID: childID, ChildJournal: childJournal, Cwd: cwd})
	fact(journal.KindJobFinished, journal.JobFinished{JobID: taskJobID, State: "completed", FinishedAt: time.Now(), ResultPreview: "all good", Cleanup: "none"})
	fact(journal.KindJobStarted, journal.JobStarted{JobID: bashJobID, IdentityDigest: strings.Repeat("b", 64), CallID: "message:1", ToolCallID: "t2", LogicalRunID: "r", Kind: "bash", Spec: json.RawMessage(`{"command":"make test","timeout_seconds":60}`), Cwd: cwd, OutputPath: root.Path() + ".d/jobs/" + bashJobID + "/output.log"})
	exit := 0
	fact(journal.KindJobFinished, journal.JobFinished{JobID: bashJobID, State: "completed", FinishedAt: time.Now(), ExitCode: &exit, Output: &work.Artifact{Path: root.Path() + ".d/jobs/" + bashJobID + "/output.log", Bytes: 6}, Cleanup: "none"})
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	write(t, root.Path()+".d/jobs/"+bashJobID+"/output.log", "PASS\nok\n")

	child, err := journal.CreateChild(ctx, root.Path(), childID, cwd, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := child.AppendMessage(ctx, types.UserMessage{Content: types.StringContent("survey the repo please")}); err != nil {
		t.Fatal(err)
	}
	if err := child.AppendMessage(ctx, types.AssistantMessage{Content: []types.ContentBlock{types.NewText("the repo is tidy")}}); err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	return root.Path()
}

func TestJobAndHistoryURLsAreScopedToTheSessionTree(t *testing.T) {
	rootJournal := sessionTree(t)
	ctx := context.Background()
	// A child resolves against its root journal, exactly like the root.
	child := mustNew(t, Config{RootJournal: rootJournal})

	expect := map[string][]string{
		"job://":                             {"job://" + taskJobID + "  task  completed  survey the repo", "job://" + bashJobID + "  bash  completed  make test"},
		"job://" + bashJobID:                 {"State: completed", "Exit code: 0", "Command: make test", "job://" + bashJobID + "/output.log"},
		"job://" + taskJobID:                 {"Transcript: history://" + childID, "all good"},
		"job://" + bashJobID + "/output.log": {"PASS\nok\n"},
		"history://":                         {"history://" + childID + "  job://" + taskJobID},
		"history://" + taskJobID:             {"Child session: " + childID, "survey the repo please", "the repo is tidy"},
		"history://" + childID:               {"Job: job://" + taskJobID, "the repo is tidy"},
		"job://" + bashJobID + "/":           {},
	}
	for uri, wants := range expect {
		resource, err := child.Read(ctx, uri)
		if err != nil {
			t.Fatalf("Read(%s): %v", uri, err)
		}
		for _, want := range wants {
			if !strings.Contains(string(resource.Data), want) {
				t.Fatalf("Read(%s) = %q, missing %q", uri, resource.Data, want)
			}
		}
	}
	if listing, _ := child.Read(ctx, "job://"+bashJobID+"/"); !listing.Dir || !hasEntry(listing.Entries, "output.log", false) {
		t.Fatalf("artifact listing = %+v", listing)
	}

	// A job artifact symlinked to the child journal must not expose it.
	jobDir := rootJournal + ".d/jobs/" + bashJobID
	if err := os.Symlink(rootJournal+".d/children/"+childID+"/session.jsonl", filepath.Join(jobDir, "changes.patch")); err != nil {
		t.Fatal(err)
	}
	if _, err := child.Read(ctx, "job://"+bashJobID+"/changes.patch"); !errors.Is(err, &Error{Code: CodeEscape}) {
		t.Fatalf("symlinked artifact error = %v", err)
	}
	if _, err := child.Read(ctx, "history://"+bashJobID); !errors.Is(err, &Error{Code: CodeTarget}) {
		t.Fatalf("history of a bash job error = %v", err)
	}

	// Another session tree sees none of it.
	other := mustNew(t, Config{RootJournal: sessionlessJournal(t)})
	for _, uri := range []string{"job://" + bashJobID, "job://" + bashJobID + "/output.log", "history://" + taskJobID, "history://" + childID} {
		if _, err := other.Read(ctx, uri); !errors.Is(err, &Error{Code: CodeNotFound}) {
			t.Fatalf("other tree Read(%s) error = %v, want not found", uri, err)
		}
	}
}

func sessionlessJournal(t *testing.T) string {
	t.Helper()
	store, err := journal.Create(context.Background(), t.TempDir(), t.TempDir(), journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return store.Path()
}

func TestExpandCommand(t *testing.T) {
	// A quote in the skill directory proves the replacement is shell-safe.
	base := filepath.Join(t.TempDir(), "it's skills", "alpha")
	write(t, filepath.Join(base, "SKILL.md"), "skill body\n")
	rootJournal := filepath.Join(t.TempDir(), "root.jsonl")
	resolver := mustNew(t, Config{Skills: []skills.Skill{{Name: "alpha", FilePath: filepath.Join(base, "SKILL.md"), BaseDir: base}}, RootJournal: rootJournal})
	local := filepath.Join(rootJournal+".d", LocalDirName)
	q := func(p string) string { return shellQuote(p) }
	skillFile := q(filepath.Join(base, "SKILL.md"))

	for _, tc := range []struct{ command, want string }{
		{"cat skill://alpha/SKILL.md", "cat " + skillFile},
		{"cat 'skill://alpha/SKILL.md'", "cat " + skillFile},
		{`cat "skill://alpha/SKILL.md"|wc -l`, "cat " + skillFile + "|wc -l"},
		{"ls skill://alpha", "ls " + q(base)},
		{`echo "$(cat skill://alpha/SKILL.md)"`, `echo "$(cat ` + skillFile + `)"`},
		{"echo `cat skill://alpha/SKILL.md`", "echo `cat " + skillFile + "`"},
		{"cat local://in.md > local://out.md", "cat " + q(filepath.Join(local, "in.md")) + " > " + q(filepath.Join(local, "out.md"))},
		{`echo "see skill://alpha/SKILL.md for details"`, `echo "see skill://alpha/SKILL.md for details"`},
		{"echo 'skill://alpha/SKILL.md is docs'", "echo 'skill://alpha/SKILL.md is docs'"},
		{"x=skill://alpha/SKILL.md", "x=skill://alpha/SKILL.md"},
		{`echo \skill://alpha`, `echo \skill://alpha`},
		{"echo 'skill://alpha'x", "echo 'skill://alpha'x"},
		{"curl https://example.com/skill://alpha", "curl https://example.com/skill://alpha"},
	} {
		got, err := resolver.ExpandCommand(tc.command)
		if err != nil || got != tc.want {
			t.Errorf("ExpandCommand(%q) = %q, %v\nwant %q", tc.command, got, err, tc.want)
		}
	}

	out, err := exec.Command("/bin/sh", "-c", mustExpand(t, resolver, "cat skill://alpha/SKILL.md")).Output()
	if err != nil || string(out) != "skill body\n" {
		t.Fatalf("expanded command ran as %q, %v", out, err)
	}

	for command, code := range map[string]string{
		"cat skill://beta/x":            CodeNotFound,
		"cat skill://alpha/../../etc/x": CodeEscape,
		"cat local://*.md":              CodeUnsupported,
		"cat local://$HOME":             CodeUnsupported,
		"cat skill://alpha/SKILL.md?x":  CodeUnsupported,
		"cat local://a#b":               CodeInvalid,
	} {
		got, err := resolver.ExpandCommand(command)
		if !errors.Is(err, &Error{Code: code}) || got != command {
			t.Errorf("ExpandCommand(%q) = %q, %v; want unchanged with %s", command, got, err, code)
		}
	}
	// Resolvable words still expand next to one that fails.
	got, err := resolver.ExpandCommand("cat skill://alpha/SKILL.md skill://beta/x")
	if err == nil || got != "cat "+skillFile+" skill://beta/x" {
		t.Fatalf("partial expansion = %q, %v", got, err)
	}
	if _, err := mustNew(t, Config{}).ExpandCommand("cat local://x"); !errors.Is(err, &Error{Code: CodeUnavailable}) {
		t.Fatalf("unjournaled local expansion error = %v", err)
	}
}

func TestChild(t *testing.T) {
	for _, tc := range []struct{ base, name, want string }{
		{"local://", "a/b.md", "local://a/b.md"},
		{"skill://alpha/", "notes/x.md", "skill://alpha/notes/x.md"},
		{"job://" + bashJobID + "/", "output.log", "job://" + bashJobID + "/output.log"},
		{"local://dir", "x y.md", "local://dir/x%20y.md"},
	} {
		if got := Child(tc.base, tc.name); got != tc.want {
			t.Errorf("Child(%q, %q) = %q, want %q", tc.base, tc.name, got, tc.want)
		}
	}
}

func mustExpand(t *testing.T, resolver *Resolver, command string) string {
	t.Helper()
	expanded, err := resolver.ExpandCommand(command)
	if err != nil {
		t.Fatal(err)
	}
	return expanded
}

func mustNew(t *testing.T, cfg Config) *Resolver {
	t.Helper()
	resolver, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func hasEntry(entries []Entry, name string, dir bool) bool {
	for _, entry := range entries {
		if entry.Name == name && entry.Dir == dir {
			return true
		}
	}
	return false
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
