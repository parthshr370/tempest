package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/config"
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

// Journals the CLI wrote before it recorded model specs hold the bare model
// id; resuming one with the same configured model keeps the configured route.
func TestSessionModelReadsBareJournaledID(t *testing.T) {
	hermeticCLI(t)
	configured := "openai:legacy-model@http://127.0.0.1:9/v1"
	t.Setenv("HARNESS_MODEL", configured)
	cfg, err := (config.Config{WorkingDir: t.TempDir()}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for journaled, want := range map[string]string{
		"legacy-model":                configured,
		"openrouter:~vendor/a-model":  "openrouter:~vendor/a-model",
		"anthropic:claude-sonnet-4-5": "anthropic:claude-sonnet-4-5",
	} {
		store, err := journal.Create(t.Context(), dir, t.TempDir(), journal.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.AppendModelChange(t.Context(), journaled, "default"); err != nil {
			t.Fatal(err)
		}
		path := store.Path()
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if got, err := sessionModel(t.Context(), cfg, path); err != nil || got != want {
			t.Fatalf("journaled %q: model = %q, %v; want %q", journaled, got, err, want)
		}
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
