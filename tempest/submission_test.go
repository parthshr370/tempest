package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest/durable"
)

func TestSubmissionConcurrentConsumedAndRestart(t *testing.T) {
	f := faux.New(faux.Options{})
	f.SetResponses(faux.Respond(faux.Text("one durable answer")))
	dir, cwd := t.TempDir(), t.TempDir()
	cfg := StackConfig{Model: f.Model(), StreamFn: f.StreamSimple}
	s, err := Open(t.Context(), Options{SessionDir: dir, Cwd: cwd, New: &NewSession{}, Stack: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	input := Input{RequestID: "caller-key", Prompt: Prompt{Text: "once"}}
	var wg sync.WaitGroup
	receipts := make(chan Submission, 24)
	failures := make(chan error, 24)
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipt, err := s.Submit(context.Background(), input)
			receipts <- receipt
			failures <- err
		}()
	}
	wg.Wait()
	close(receipts)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	id := ""
	runID := ""
	for receipt := range receipts {
		if id == "" {
			id, runID = receipt.ID, receipt.RunID
		}
		if receipt.ID != id || receipt.RunID != runID {
			t.Fatalf("different logical admissions: %+v", receipt)
		}
	}
	terminal, err := s.WaitSubmission(t.Context(), input.RequestID)
	if err != nil || terminal.State != "completed" || terminal.Answer != "one durable answer" || terminal.Result.StopReason != agent.StopCompleted {
		t.Fatalf("terminal: %+v %v", terminal, err)
	}
	if f.CallCount() != 1 {
		t.Fatalf("provider called %d times", f.CallCount())
	}
	changed := input
	changed.Prompt.Text = "different"
	if _, err = s.Submit(t.Context(), changed); !errors.Is(err, durable.ErrConflict) {
		t.Fatalf("changed payload: %v", err)
	}
	path := s.core.store.Path()
	s.Close(t.Context())
	for range 2 {
		s, err = Open(t.Context(), Options{SessionDir: dir, Cwd: cwd, Resume: &ResumeSession{Path: path}, Stack: &cfg})
		if err != nil {
			t.Fatal(err)
		}
		again, e := s.Submit(t.Context(), input)
		if e != nil || again.ID != id || again.Answer != terminal.Answer || again.Result.StopReason != terminal.Result.StopReason {
			t.Fatalf("restart receipt: %+v %v", again, e)
		}
		if f.CallCount() != 1 {
			t.Fatal("restart executed provider")
		}
		s.Close(t.Context())
	}
}

func TestSubmissionWithdrawalAndOriginalOrderIdentity(t *testing.T) {
	f := faux.New(faux.Options{})
	s := openFaux(t, f, nil)
	input := Input{RequestID: "queued", Placement: PlacementSteer, Prompt: Prompt{Text: "queued input", ContentIdentity: json.RawMessage(`[{"type":"text","text":"queued input"}]`)}}
	admitted, err := s.Submit(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Withdraw(t.Context(), input.RequestID); err != nil {
		t.Fatal(err)
	}
	again, err := s.Submit(t.Context(), input)
	if err != nil || again.ID != admitted.ID || again.State != "withdrawn" {
		t.Fatalf("withdrawn receipt reopened: %+v %v", again, err)
	}
	changed := input
	changed.Prompt.ContentIdentity = json.RawMessage(`[{"type":"text","text":"queued"},{"type":"text","text":" input"}]`)
	if _, err = s.Submit(t.Context(), changed); !errors.Is(err, durable.ErrConflict) {
		t.Fatalf("original block order not preserved: %v", err)
	}
	if f.CallCount() != 0 {
		t.Fatal("inspection executed provider")
	}
}

func TestSubmissionHookRefusalRetainsKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := faux.New(faux.Options{})
	cfg := StackConfig{Model: f.Model(), StreamFn: f.StreamSimple}
	settings := HookSettings{Hooks: map[string][]HookGroup{"UserPromptSubmit": {{Hooks: []HookCommand{{Type: "command", Command: "printf 'no' >&2; exit 2"}}}}}}
	s, err := Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{}, Stack: &cfg, Hooks: &settings})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	input := Input{RequestID: "denied", Prompt: Prompt{Text: "not approved"}}
	receipt, err := s.Submit(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := s.WaitSubmission(t.Context(), input.RequestID)
	if err != nil || !terminalSubmission(terminal.State) {
		t.Fatalf("refusal receipt: %+v %v", terminal, err)
	}
	again, err := s.Submit(t.Context(), input)
	if err != nil || again.ID != receipt.ID || again.State != terminal.State || f.CallCount() != 0 {
		t.Fatalf("refusal reopened: %+v %v", again, err)
	}
}

func TestCancelledSubmissionWaitDoesNotWithdraw(t *testing.T) {
	f := faux.New(faux.Options{})
	asked := make(chan Ask, 1)
	s := openFaux(t, f, promptFunc(func(ctx context.Context, a Ask) (Choice, error) { asked <- a; <-ctx.Done(); return "", ctx.Err() }))
	scriptBash(f, "rm -rf "+s.Snapshot().Cwd+"/never")
	input := Input{RequestID: "wait-cancel", Prompt: Prompt{Text: "use bash"}}
	receipt, err := s.Submit(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	ask := <-asked
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = s.WaitSubmission(ctx, input.RequestID); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait cancellation: %v", err)
	}
	prior, err := s.Submit(t.Context(), input)
	if err != nil || prior.ID != receipt.ID || prior.State == "withdrawn" {
		t.Fatalf("wait withdrew input: %+v %v", prior, err)
	}
	if err = s.Answer(ask.ToolCallID, permission.AllowOnce); err != nil {
		t.Fatal(err)
	}
	terminal, err := s.WaitSubmission(t.Context(), input.RequestID)
	if err != nil || terminal.State != "completed" {
		t.Fatalf("accepted work didn't finish: %+v %v", terminal, err)
	}
}

type pausingAdmissionStore struct {
	durable.Store
	after   bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *pausingAdmissionStore) AcquireSession(ctx context.Context, id string) (func(), error) {
	return s.Store.(durable.SessionLeaser).AcquireSession(ctx, id)
}

func (s *pausingAdmissionStore) Commit(ctx context.Context, c durable.Commit) (durable.Receipt, error) {
	if !strings.Contains(c.ID, "admit:") {
		return s.Store.Commit(ctx, c)
	}
	var r durable.Receipt
	var err error
	if s.after {
		r, err = s.Store.Commit(ctx, c)
		if err != nil {
			return r, err
		}
	}
	s.once.Do(func() { close(s.entered); <-s.release })
	if !s.after {
		return s.Store.Commit(ctx, c)
	}
	return r, err
}
func TestAdmissionAcknowledgmentBoundaries(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel_before_commit", true: "cancel_after_commit_before_response"}[after], func(t *testing.T) {
			backend := &pausingAdmissionStore{Store: durable.NewMemory(), after: after, entered: make(chan struct{}), release: make(chan struct{})}
			f := faux.New(faux.Options{})
			f.SetResponses(faux.Respond(faux.Text("done")))
			cfg := StackConfig{Model: f.Model(), StreamFn: f.StreamSimple}
			s, err := Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), Store: backend, New: &NewSession{}, Stack: &cfg})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background())
			ctx, cancel := context.WithCancel(t.Context())
			input := Input{RequestID: "paused", Prompt: Prompt{Text: "durable input"}}
			done := make(chan error, 1)
			go func() { _, err := s.Submit(ctx, input); done <- err }()
			<-backend.entered
			select {
			case e := <-done:
				t.Fatalf("success before durable response: %v", e)
			default:
			}
			cancel()
			close(backend.release)
			err = <-done
			if !after {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("precommit cancellation: %v", err)
				}
				if _, err = s.Submission(t.Context(), input.RequestID); !errors.Is(err, ErrQueueNotFound) {
					t.Fatalf("uncommitted admission present: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			terminal, err := s.WaitSubmission(t.Context(), input.RequestID)
			if err != nil || terminal.State != "completed" {
				t.Fatalf("postcommit cancellation lost input: %+v %v", terminal, err)
			}
		})
	}
}

func TestClientNamespacesAndAuthorization(t *testing.T) {
	f := faux.New(faux.Options{})
	cfg := StackConfig{Model: f.Model(), StreamFn: f.StreamSimple}
	denied := errors.New("denied")
	s, err := Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{}, Stack: &cfg, Authorize: func(ctx context.Context, a Access) error {
		if a.Action == "commit" {
			return denied
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	a := durable.WithClientNamespace(t.Context(), "principal-a")
	b := durable.WithClientNamespace(t.Context(), "principal-b")
	in := Input{RequestID: "same", Placement: PlacementSteer, Prompt: Prompt{Text: "same input"}}
	one, err := s.Submit(a, in)
	if err != nil {
		t.Fatal(err)
	}
	two, err := s.Submit(b, in)
	if err != nil || two.ID == one.ID {
		t.Fatalf("client namespaces collided: %+v %+v %v", one, two, err)
	}
	if _, err = s.Commit(t.Context(), durable.Commit{}); !errors.Is(err, denied) {
		t.Fatalf("authorization bypass: %v", err)
	}
}

func journalEntryLine(line string) string {
	var envelope durable.Envelope
	if json.Unmarshal([]byte(line), &envelope) == nil && envelope.Type == "commit" {
		for _, change := range envelope.Commit.Changes {
			if change.Kind == durable.EntryKind && change.Namespace == "_tempest/journal" {
				return string(change.Value)
			}
		}
	}
	return line
}
