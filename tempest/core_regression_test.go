package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/jobs"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/internal/work"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

func TestApplicationCommitIDsCannotReserveHostDecisions(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.Text("done")))
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	definition := durable.DocumentDefinition{Namespace: "app", Version: 1, Schema: json.RawMessage(`{"type":"integer"}`), Initial: json.RawMessage(`0`)}
	if err := registry.Install(t.Context(), extension.Definition{ID: "app", Revision: "v1", Documents: []durable.DocumentDefinition{definition}}); err != nil {
		t.Fatal(err)
	}
	owner, err := Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}, Registry: registry, Extensions: []extension.Selection{{ID: "app", Required: true}}})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	for i, id := range []string{"admit:victim", "journal:6", "memo:decision", "define:app", "extension:call:result"} {
		receipt, err := owner.Commit(t.Context(), durable.Commit{ID: id, Changes: []durable.Change{{Kind: durable.DocumentKind, Namespace: "app", ID: "default", Version: 1, Value: json.RawMessage(fmt.Sprint(i))}}})
		if err != nil || receipt.ID != id {
			t.Fatalf("public commit: %+v %v", receipt, err)
		}
		lookup, err := owner.Read(t.Context(), durable.Query{CommitID: id})
		if err != nil || len(lookup.Receipts) != 1 || lookup.Receipts[0] != receipt {
			t.Fatalf("receipt lookup: %+v %v", lookup, err)
		}
	}
	if _, err = owner.Submit(t.Context(), Input{RequestID: "victim", Prompt: Prompt{Text: "run"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.WaitSubmission(t.Context(), "victim"); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitAndFollowUpShareAdmissionNamespace(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.Text("done")))
	owner := openFaux(t, provider, nil)
	var group sync.WaitGroup
	results := make(chan error, 2)
	group.Add(2)
	go func() {
		defer group.Done()
		_, err := owner.Submit(t.Context(), Input{RequestID: "same", Prompt: Prompt{Text: "direct"}})
		results <- err
	}()
	go func() {
		defer group.Done()
		_, err := owner.FollowUp(t.Context(), QueuedInput{Key: "same", Text: "queued"})
		results <- err
	}()
	group.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if !errors.Is(err, durable.ErrConflict) && !errors.Is(err, ErrQueueConflict) {
			t.Fatal(err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("admissions under one key: %d", succeeded)
	}
}

func TestResumeIntersectsCurrentOperatorPolicy(t *testing.T) {
	provider := faux.New(faux.Options{})
	dir, cwd := t.TempDir(), t.TempDir()
	cfg := StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}
	owner, err := Open(t.Context(), Options{SessionDir: dir, Cwd: cwd, New: &NewSession{}, Stack: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	path := owner.core.store.Path()
	if err = owner.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	cfg.AllowedTools = []string{"read", "bash"}
	cfg.ExcludeTools = []string{"bash"}
	owner, err = Open(t.Context(), Options{SessionDir: dir, Cwd: cwd, Resume: &ResumeSession{Path: path}, Stack: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	for _, tool := range owner.core.agent.State().Tools {
		if tool.Name == "bash" {
			t.Fatal("resume erased current bash exclusion")
		}
		if tool.Name != "read" {
			t.Fatalf("resume erased current allowlist: %s", tool.Name)
		}
	}
}

func TestAgentSelectionCanReturnToEarlierValue(t *testing.T) {
	provider := faux.New(faux.Options{})
	owner := openFaux(t, provider, nil)
	cfg := StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}
	for _, reasoning := range []string{"low", "high", "low"} {
		cfg.Reasoning = reasoning
		if err := owner.persistAgent(t.Context(), Options{}, cfg, provider.Model().ID, nil); err != nil {
			t.Fatal(err)
		}
	}
	page, err := owner.core.store.Read(t.Context(), durable.Query{Kind: durable.DocumentKind, Namespace: "_tempest/agent", ID: "selection"})
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("selection: %+v %v", page, err)
	}
	var selection agentSelection
	if err = json.Unmarshal(page.Records[0].Value, &selection); err != nil || selection.Reasoning != "low" {
		t.Fatalf("selection did not return to A: %+v %v", selection, err)
	}
}

func TestPromptContextCancellationJoinsOwnedRun(t *testing.T) {
	provider := faux.New(faux.Options{})
	asked := make(chan struct{}, 1)
	owner := openFaux(t, provider, promptFunc(func(ctx context.Context, _ Ask) (Choice, error) {
		asked <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}))
	scriptBash(provider, "rm -rf "+filepath.Join(owner.Snapshot().Cwd, "never"))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan RunResult, 1)
	go func() {
		result, err := owner.Prompt(ctx, Prompt{Text: "wait"})
		if err != nil {
			t.Errorf("prompt: %v", err)
		}
		done <- result
	}()
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("permission was not requested")
	}
	cancel()
	select {
	case result := <-done:
		if result.StopReason != agent.StopAborted {
			t.Fatalf("not aborted: %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled Prompt did not join")
	}
	owner.core.mu.Lock()
	active := owner.core.active != nil
	owner.core.mu.Unlock()
	if active {
		t.Fatal("Prompt returned while its run remained active")
	}
}

func TestDurableSnapshotIsNewestBoundedMetadata(t *testing.T) {
	owner := openFaux(t, faux.New(faux.Options{}), nil)
	for i := range 105 {
		receipt := durable.Submission{ID: fmt.Sprintf("s-%03d", i), RequestID: fmt.Sprint(i), State: "completed", Placement: "input", Payload: json.RawMessage(`{"secret":"` + strings.Repeat("x", 12000) + `"}`), Answer: strings.Repeat("y", 12000)}
		if err := owner.saveSubmission(t.Context(), receipt, "admit:"+receipt.RequestID); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := owner.Snapshot()
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "_tempest/agent") || strings.Contains(string(raw), strings.Repeat("y", 100)) {
		t.Fatal("durable snapshot leaked private values")
	}
	if len(raw) > 256<<10 {
		t.Fatalf("snapshot not bounded: %d", len(raw))
	}
	if len(snapshot.Submissions) == 0 || snapshot.Submissions[0].ID != "s-104" {
		t.Fatalf("snapshot omitted newest receipt: %+v", snapshot.Submissions)
	}
}

func TestRequiredProcessSelectionSurvivesResume(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	fixture, err := filepath.Abs("extension/testdata/peer.py")
	if err != nil {
		t.Fatal(err)
	}
	provider := faux.New(faux.Options{})
	cfg := StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}
	dir, cwd := t.TempDir(), t.TempDir()
	owner, err := Open(t.Context(), Options{SessionDir: dir, Cwd: cwd, New: &NewSession{}, Stack: &cfg, ExtensionProcesses: []extension.ProcessOptions{{ID: "sample", Argv: []string{python, fixture}, Capabilities: []string{"read", "memo", "commit", "next"}}}})
	if err != nil {
		t.Fatal(err)
	}
	path := owner.core.store.Path()
	if err = owner.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), Options{SessionDir: dir, Cwd: cwd, Resume: &ResumeSession{Path: path}, Stack: &cfg})
	if err == nil {
		reopened.Close(t.Context())
		t.Fatal("required subprocess safety bundle silently dropped")
	}
	if !strings.Contains(err.Error(), "sample") {
		t.Fatalf("missing required selection not named: %v", err)
	}
}

func TestKeylessNamespacedQueueAndNamespaceAlias(t *testing.T) {
	owner := openFaux(t, faux.New(faux.Options{}), nil)
	namespaced := durable.WithClientNamespace(t.Context(), "alice")
	if _, err := owner.FollowUp(namespaced, QueuedInput{Text: "keyless"}); err != nil {
		t.Fatal(err)
	}
	if durable.SubmissionKey("", durable.SubmissionKey("alice", "x")) == durable.SubmissionKey("alice", "x") {
		t.Fatal("default request ID aliases authenticated namespace")
	}
}

func TestRecoveryRequiredDoesNotBurnSubmissionKey(t *testing.T) {
	owner := openFaux(t, faux.New(faux.Options{}), nil)
	jobID, childID, digest := jobs.Identity(owner.ID(), "unfinished")
	manager, err := jobs.New(work.Host{SessionID: owner.ID(), JournalPath: owner.core.store.Path(), Append: owner.core.store.AppendWorkFact, Enqueue: owner.core.queues.Enqueue}, jobs.Options{}, []work.Recovery{{Job: work.Job{ID: jobID, Kind: "task", State: "running", ChildSessionID: childID}, Invocation: work.Invocation{CallID: "unfinished"}, IdentityDigest: digest, Task: &work.TaskSpec{}, Environment: &work.TaskEnvironment{}, ChildJournal: "child.jsonl"}})
	if err != nil {
		t.Fatal(err)
	}
	original := owner.core.jobs
	owner.core.jobs = manager
	defer func() { owner.core.jobs = original }()
	if _, err = owner.Begin(t.Context(), Prompt{Text: "blocked"}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("Begin lost typed recovery error: %v", err)
	}
	if _, err = owner.Submit(t.Context(), Input{RequestID: "retryable", Prompt: Prompt{Text: "blocked"}}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("Submit lost typed recovery error: %v", err)
	}
	if _, err = owner.Submission(t.Context(), "retryable"); !errors.Is(err, ErrQueueNotFound) {
		t.Fatalf("recovery gate burned request key: %v", err)
	}
}

func TestContinuationSettlesOriginalLogicalSubmission(t *testing.T) {
	owner := openFaux(t, faux.New(faux.Options{}), nil)
	value := durable.Submission{ID: "receipt", RequestID: "original", State: "running", RunID: "first", LogicalRunID: "first", Placement: "input", Payload: json.RawMessage(`{"Text":"input"}`)}
	if err := owner.saveSubmission(t.Context(), value, "admit:original"); err != nil {
		t.Fatal(err)
	}
	owner.core.mu.Lock()
	owner.core.active = &Run{startID: "continued", done: make(chan struct{})}
	owner.core.mu.Unlock()
	defer func() { owner.core.mu.Lock(); owner.core.active = nil; owner.core.mu.Unlock() }()
	result := make(chan Submission, 1)
	go func() {
		receipt, err := owner.WaitSubmission(t.Context(), "original")
		if err != nil {
			t.Errorf("wait: %v", err)
		}
		result <- receipt
	}()
	if err := owner.core.store.Append(t.Context(), journal.RunStartedRecord{Meta: journal.Meta{RunID: "continued"}, Model: "faux", LogicalRunID: "first", ContinuationOf: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := owner.core.store.Append(t.Context(), journal.RunFinishedRecord{Meta: journal.Meta{RunID: "continued"}, StopReason: "completed"}); err != nil {
		t.Fatal(err)
	}
	owner.core.mu.Lock()
	close(owner.core.active.done)
	owner.core.active = nil
	owner.core.mu.Unlock()
	select {
	case receipt := <-result:
		if receipt.State != "completed" || receipt.RunID != "continued" {
			t.Fatalf("continuation lost receipt: %+v", receipt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("continuation waiter stuck")
	}
}

func TestResumeAcceptedLegacyPayloadWithoutSchema(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.Text("resumed")))
	owner := openFaux(t, provider, nil)
	value := durable.Submission{ID: "accepted", RequestID: "accepted", RunID: "accepted-run", State: "accepted", Placement: "input", Payload: json.RawMessage(`{"Text":"input","Media":null,"OutputSchema":null,"ContentIdentity":null}`)}
	if err := owner.saveSubmission(t.Context(), value, "admit:accepted"); err != nil {
		t.Fatal(err)
	}
	if err := owner.ResumePending(t.Context()); err != nil {
		t.Fatal(err)
	}
	receipt, err := owner.WaitSubmission(t.Context(), "accepted")
	if err != nil || receipt.State != "completed" || receipt.Answer != "resumed" {
		t.Fatalf("accepted resume: %+v %v", receipt, err)
	}
}

func TestSubmitReturnsCurrentRunningReceipt(t *testing.T) {
	provider := faux.New(faux.Options{})
	owner := openFaux(t, provider, promptFunc(func(ctx context.Context, _ Ask) (Choice, error) { <-ctx.Done(); return "", ctx.Err() }))
	scriptBash(provider, "rm -rf "+filepath.Join(owner.Snapshot().Cwd, "never"))
	receipt, err := owner.Submit(t.Context(), Input{RequestID: "current", Prompt: Prompt{Text: "wait"}})
	if err != nil || receipt.State != "running" || receipt.Revision < 2 {
		t.Fatalf("stale accepted receipt: %+v %v", receipt, err)
	}
	owner.Cancel()
	if _, err = owner.WaitSubmission(t.Context(), "current"); err != nil {
		t.Fatal(err)
	}
}
