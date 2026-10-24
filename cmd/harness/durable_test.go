package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest"
	"go.harness.dev/harness/tempest/durable"
)

func TestRequestIDReturnsOriginalCLIOutcome(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("original")), faux.Respond(faux.Text("must not run")))
	session := streamSession(t, f, f.StreamSimple, tempest.Budget{})
	var first, second bytes.Buffer
	a, err := executeOneShot(context.Background(), session, tempest.Prompt{Text: "hello"}, "json", &first, io.Discard, 256, "stable")
	if err != nil {
		t.Fatal(err)
	}
	b, err := executeOneShot(context.Background(), session, tempest.Prompt{Text: "hello"}, "json", &second, io.Discard, 256, "stable")
	if err != nil {
		t.Fatal(err)
	}
	if a.RunID == "" || a.RunID != b.RunID || a.LastAssistantText != "original" || b.LastAssistantText != "original" {
		t.Fatalf("first=%+v second=%+v", a, b)
	}
	if _, err := executeOneShot(context.Background(), session, tempest.Prompt{Text: "changed"}, "json", io.Discard, io.Discard, 256, "stable"); err == nil {
		t.Fatal("changed prompt accepted")
	}
	opts, err := parseCommandOptions("", []string{"-p", "hello", "-resume", session.ID(), "-request-id", "stable"}, io.Discard)
	if err != nil || opts.RequestID != "stable" {
		t.Fatalf("options=%+v err=%v", opts, err)
	}
}

func TestCLIRequestIDRetryWithoutSession(t *testing.T) {
	for _, assignment := range os.Environ() {
		key, _, _ := strings.Cut(assignment, "=")
		if strings.HasPrefix(key, "HARNESS_") || strings.HasPrefix(key, "LYRA_") {
			t.Setenv(key, "")
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	sessionDir := t.TempDir()
	script := filepath.Join(t.TempDir(), "provider.json")
	writeScript := func(text string) {
		t.Helper()
		raw, _ := json.Marshal([]map[string]string{{"text": text}})
		if err := os.WriteFile(script, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"-session-dir", sessionDir, "-faux-script", script, "-output-format", "json", "-request-id", "stable", "-p", "original input"}
	invoke := func(args []string) runReport {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if exit := run(args, &stdout, &stderr); exit != 0 {
			t.Fatalf("exit=%d stderr=%s stdout=%s", exit, &stderr, &stdout)
		}
		var report runReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		return report
	}
	writeScript("original answer")
	first := invoke(args)
	writeScript("provider must not run again")
	second := invoke(args)
	if first.SessionID == "" || first.SubmissionID == "" || first.SessionID != second.SessionID || first.SubmissionID != second.SubmissionID || second.Text != "original answer" {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	loaded := loadOnlyJournal(t, sessionDir)
	if len(loaded.Runs) != 1 {
		t.Fatalf("runs=%d, want 1", len(loaded.Runs))
	}
	path, err := journal.Resolve(context.Background(), sessionDir, loaded.Header.Cwd, first.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + ".catalog"); err != nil {
		t.Fatal(err)
	}
	withoutIndex := invoke(args)
	if withoutIndex.SubmissionID != first.SubmissionID || withoutIndex.Text != first.Text {
		t.Fatalf("missing index lost receipt: %+v", withoutIndex)
	}
	changed := append([]string{}, args...)
	changed[len(changed)-1] = "changed input"
	var stdout, stderr bytes.Buffer
	if exit := run(changed, &stdout, &stderr); exit == 0 || !strings.Contains(stderr.String(), "conflict") {
		t.Fatalf("changed input: exit=%d stderr=%s", exit, &stderr)
	}
	if len(loadOnlyJournal(t, sessionDir).Runs) != 1 {
		t.Fatal("conflicting retry created work")
	}
	// Seed the durable cut left by a crash after acceptance, before dispatch.
	store, _, err := journal.OpenForResume(context.Background(), path, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	staleIndex, err := os.ReadFile(path + ".catalog")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(tempest.Prompt{Text: "accepted input"})
	hash, err := durable.SubmissionHash("input", payload)
	if err != nil {
		t.Fatal(err)
	}
	accepted := durable.Submission{ID: "accepted-submission", RequestID: "pending", State: "accepted", RunID: "accepted-run", RequestHash: hash, Placement: "input", Payload: payload}
	value, _ := json.Marshal(accepted)
	_, err = store.Commit(context.Background(), durable.Commit{ID: "admit:pending", Changes: []durable.Change{{SessionID: first.SessionID, Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", ID: "pending", Version: 1, Value: value}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// The journal commit can survive even if its catalog update didn't.
	if err := os.WriteFile(path+".catalog", staleIndex, 0600); err != nil {
		t.Fatal(err)
	}
	pendingArgs := append([]string{}, args...)
	pendingArgs[len(pendingArgs)-3], pendingArgs[len(pendingArgs)-1] = "pending", "accepted input"
	resumed := invoke(pendingArgs)
	if resumed.SessionID != first.SessionID || resumed.SubmissionID != accepted.ID || resumed.Text != "provider must not run again" {
		t.Fatalf("accepted input wasn't resumed: %+v", resumed)
	}
	// A different cwd gets an independent key, just like -continue.
	t.Chdir(t.TempDir())
	other := invoke(args)
	if other.SessionID == first.SessionID || other.Text != "provider must not run again" {
		t.Fatalf("other cwd reused prior receipt: %+v", other)
	}
}

func TestCLIRequestIDConcurrentRetryWithoutSession(t *testing.T) {
	for _, assignment := range os.Environ() {
		key, _, _ := strings.Cut(assignment, "=")
		if strings.HasPrefix(key, "HARNESS_") || strings.HasPrefix(key, "LYRA_") {
			t.Setenv(key, "")
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	sessionDir := t.TempDir()
	script := filepath.Join(t.TempDir(), "provider.json")
	if err := os.WriteFile(script, []byte(`[{"text":"one answer"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-session-dir", sessionDir, "-faux-script", script, "-output-format", "json", "-request-id", "concurrent", "-p", "same input"}
	type result struct {
		exit           int
		stdout, stderr bytes.Buffer
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			var r result
			r.exit = run(args, &r.stdout, &r.stderr)
			results <- r
		}()
	}
	close(start)
	var reports [2]runReport
	for i := range reports {
		r := <-results
		if r.exit != 0 {
			t.Fatalf("exit=%d stderr=%s", r.exit, &r.stderr)
		}
		if err := json.Unmarshal(r.stdout.Bytes(), &reports[i]); err != nil {
			t.Fatal(err)
		}
	}
	if reports[0].SessionID == "" || reports[0].SessionID != reports[1].SessionID || reports[0].SubmissionID != reports[1].SubmissionID || reports[0].Text != "one answer" || reports[1].Text != "one answer" {
		t.Fatalf("reports=%+v", reports)
	}
	if len(loadOnlyJournal(t, sessionDir).Runs) != 1 {
		t.Fatal("concurrent retry created duplicate work")
	}
}
