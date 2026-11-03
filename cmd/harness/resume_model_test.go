package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/config"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/session/journal"
)

// hermeticCLI clears the host's harness settings and credentials, so a run
// sees only what the test sets.
func hermeticCLI(t *testing.T) {
	t.Helper()
	for _, assignment := range os.Environ() {
		key, _, _ := strings.Cut(assignment, "=")
		if strings.HasPrefix(key, "HARNESS_") || strings.HasPrefix(key, "LYRA_") || strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "OPENAI_") || strings.HasPrefix(key, "OPENROUTER_") {
			t.Setenv(key, "")
		}
	}
	t.Setenv("HOME", t.TempDir())
}

// A -continue or -resume without -model stays on the model the session was
// started on: its provider, endpoint, budget price check and all. Only a
// model chosen for the command moves it.
func TestCLIResumeKeepsTheSessionsModel(t *testing.T) {
	hermeticCLI(t)
	openai, anthropic := newRequestCapture(t), newRequestCapture(t)
	t.Setenv("OPENAI_API_KEY", "resume-test-openai")
	t.Setenv("ANTHROPIC_API_KEY", "resume-test-anthropic")
	t.Setenv("ANTHROPIC_BASE_URL", anthropic.server.URL)
	t.Chdir(t.TempDir())
	sessionDir := t.TempDir()
	cli := func(args ...string) (int, string) {
		var stdout, stderr bytes.Buffer
		code := run(append([]string{"-session-dir", sessionDir, "-c", "retry_enabled=false"}, args...), &stdout, &stderr)
		return code, stderr.String()
	}
	saved := "openai:resume-model@" + openai.server.URL + "/v1"
	lastModel := func() string {
		t.Helper()
		requests := openai.requests()
		if len(requests) == 0 {
			t.Fatal("no request reached the session's endpoint")
		}
		last := requests[len(requests)-1]
		for _, model := range []string{"resume-model", "other-model"} {
			if strings.Contains(last, `"model":"`+model+`"`) {
				return model
			}
		}
		t.Fatalf("request names no expected model: %s", last)
		return ""
	}

	cli("-model", saved, "-p", "first")
	if got := lastModel(); got != "resume-model" {
		t.Fatalf("first run model = %s", got)
	}
	// A standing default (HARNESS_MODEL, settings) picks new sessions' model,
	// not a resumed one's.
	t.Setenv("HARNESS_MODEL", "claude-opus-4-8")
	_, stderr := cli("-continue", "-p", "second")
	if len(openai.requests()) != 2 || lastModel() != "resume-model" || len(anthropic.requests()) != 0 {
		t.Fatalf("-continue: endpoint requests=%d anthropic requests=%d\n%s", len(openai.requests()), len(anthropic.requests()), stderr)
	}
	// The saved model's price is unknown, so -max-cost is refused for it,
	// though the configured default's price is known.
	if code, stderr := cli("-continue", "-max-cost", "1", "-p", "priced"); code != 1 || !strings.Contains(stderr, "budget error") || len(openai.requests()) != 2 {
		t.Fatalf("-continue -max-cost: exit=%d requests=%d\n%s", code, len(openai.requests()), stderr)
	}
	cli("-continue", "-model", "openai:other-model@"+openai.server.URL+"/v1", "-p", "third")
	if got := lastModel(); got != "other-model" {
		t.Fatalf("-continue -model ran %s", got)
	}
	// The explicit move is journaled, so the next plain resume keeps it.
	path, err := journal.Recent(t.Context(), sessionDir, mustGetwd(t))
	if err != nil {
		t.Fatal(err)
	}
	cli("-resume", strings.TrimSuffix(filepath.Base(path), ".jsonl"), "-p", "fourth")
	if got := lastModel(); got != "other-model" || len(anthropic.requests()) != 0 {
		t.Fatalf("-resume ran %s, anthropic requests=%d", got, len(anthropic.requests()))
	}
}

// legacyJournal writes a session the way the CLI did before it journaled
// model specs: the bare model id, plus the run and reply records naming the
// provider (an empty provider writes neither, as for a session that never
// ran; onlyReply writes only the messages, as in journals without run
// records).
func legacyJournal(t *testing.T, dir, cwd, id, provider string, onlyReply bool) string {
	t.Helper()
	store, err := journal.Create(t.Context(), dir, cwd, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendModelChange(t.Context(), id, "default"); err != nil {
		t.Fatal(err)
	}
	if provider != "" {
		run := journal.Meta{RunID: "legacy-run"}
		entries := []journal.Entry{
			journal.RunStartedRecord{Meta: run, Model: id, Provider: provider},
			journal.MessageEntry{Meta: run, Message: ptypes.UserMessage{Content: ptypes.StringContent("first")}},
			journal.MessageEntry{Meta: run, Message: ptypes.AssistantMessage{Content: []ptypes.ContentBlock{ptypes.NewText("done")}, Provider: provider, Model: id, StopReason: ptypes.StopStop}},
			journal.RunFinishedRecord{Meta: run, StopReason: "completed", Turns: 1},
		}
		if onlyReply {
			entries = entries[1:3]
		}
		for _, entry := range entries {
			if err := store.Append(t.Context(), entry); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return store.Path()
}

// Journals the CLI wrote before it recorded model specs hold the bare model
// id. Resuming one takes the provider from the session's run (or reply)
// records, keeps the configured spec when it names the same route, and
// otherwise asks for -model rather than reading the id as Anthropic's.
func TestSessionModelReadsBareJournaledID(t *testing.T) {
	const endpointModel = "openai:legacy-model@http://127.0.0.1:9/v1"
	tests := []struct {
		name       string
		configured string
		journaled  string
		provider   string
		onlyReply  bool
		want       string
		notice     bool
		wantErr    string
	}{
		{name: "configured id, no records", configured: endpointModel, journaled: "legacy-model", want: endpointModel},
		{name: "configured route", configured: endpointModel, journaled: "legacy-model", provider: "openai", want: endpointModel},
		{name: "full spec", configured: endpointModel, journaled: "openrouter:~vendor/a-model", want: "openrouter:~vendor/a-model"},
		{name: "anthropic spec", configured: endpointModel, journaled: "anthropic:claude-sonnet-4-5", want: "anthropic:claude-sonnet-4-5"},
		{name: "configured id, other provider", configured: endpointModel, journaled: "legacy-model", provider: "openrouter", want: "openrouter:legacy-model", notice: true},
		{name: "openai run", journaled: "gpt-4.1", provider: "openai", want: "openai:gpt-4.1", notice: true},
		{name: "openrouter run", journaled: "~google/gemini-flash-latest", provider: "openrouter", want: "openrouter:~google/gemini-flash-latest", notice: true},
		{name: "reply only", journaled: "gpt-4.1", provider: "openai", onlyReply: true, want: "openai:gpt-4.1", notice: true},
		{name: "id with a colon", journaled: "llama3:8b", provider: "openai", want: "openai:llama3:8b", notice: true},
		{name: "anthropic run", journaled: "claude-unlisted", provider: "anthropic", want: "claude-unlisted"},
		{name: "anthropic catalog id", journaled: "claude-opus-4-8", want: "claude-opus-4-8"},
		{name: "unknown route", journaled: "gpt-4.1", wantErr: "pass -model <provider>:gpt-4.1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hermeticCLI(t)
			if test.configured != "" {
				t.Setenv("HARNESS_MODEL", test.configured)
			}
			cfg, err := (config.Config{WorkingDir: t.TempDir()}).Resolve()
			if err != nil {
				t.Fatal(err)
			}
			path := legacyJournal(t, t.TempDir(), t.TempDir(), test.journaled, test.provider, test.onlyReply)
			var stderr bytes.Buffer
			got, err := sessionModel(t.Context(), cfg, path, &stderr)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("model = %q, err = %v; want error containing %q", got, err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("model = %q, %v; want %q", got, err, test.want)
			}
			if notice := strings.Contains(stderr.String(), "endpoint was not recorded"); notice != test.notice {
				t.Fatalf("notice = %v, want %v: %q", notice, test.notice, stderr.String())
			}
		})
	}
}

// A plain resume of a pre-spec OpenAI session reaches OpenAI, not the
// Anthropic default the bare id would otherwise route to.
func TestCLIResumesLegacyOpenAISession(t *testing.T) {
	hermeticCLI(t)
	openai, anthropic := newRequestCapture(t), newRequestCapture(t)
	t.Setenv("OPENAI_API_KEY", "resume-test-openai")
	t.Setenv("OPENAI_BASE_URL", openai.server.URL+"/v1")
	t.Setenv("ANTHROPIC_API_KEY", "resume-test-anthropic")
	t.Setenv("ANTHROPIC_BASE_URL", anthropic.server.URL)
	t.Chdir(t.TempDir())
	sessionDir := t.TempDir()
	path := legacyJournal(t, sessionDir, mustGetwd(t), "resume-model", "openai", false)
	var stdout, stderr bytes.Buffer
	run([]string{"-session-dir", sessionDir, "-c", "retry_enabled=false", "-resume", strings.TrimSuffix(filepath.Base(path), ".jsonl"), "-p", "next"}, &stdout, &stderr)
	requests := openai.requests()
	if len(requests) != 1 || !strings.Contains(requests[0], `"model":"resume-model"`) || len(anthropic.requests()) != 0 {
		t.Fatalf("openai requests=%d anthropic requests=%d\n%s", len(requests), len(anthropic.requests()), stderr.String())
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return cwd
}
