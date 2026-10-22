package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	model "github.com/a2aproject/a2a-go/v2/a2a"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest"
)

type durableBackend struct {
	detail      tempest.RunDetail
	lookupError error
	lookups     int
}

func (*durableBackend) New(context.Context, string, *tempest.NewSession) (*tempest.Session, error) {
	return nil, errors.New("unexpected session open")
}
func (*durableBackend) Resume(context.Context, string, string, []tempest.MCPServerConfig) (*tempest.Session, error) {
	return nil, errors.New("unexpected session resume")
}
func (*durableBackend) Acquire(string) (*tempest.Session, func(), error) {
	return nil, nil, errors.New("no live writer")
}
func (*durableBackend) RunContext() context.Context  { return context.Background() }
func (*durableBackend) Defaults() tempest.NewSession { return tempest.NewSession{} }
func (backend *durableBackend) LookupRun(context.Context, string) (tempest.RunDetail, error) {
	backend.lookups++
	return backend.detail, backend.lookupError
}

func TestDurableGetTaskMapsStatesAndHistoryWithoutRunHandle(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	backend := &durableBackend{detail: tempest.RunDetail{SessionID: "parent", RunID: "old_run", LogicalRunID: "old_run", Finished: true, StartedAt: started, FinishedAt: started.Add(time.Second), StopReason: "completed", FinalText: "exact old answer", History: []journal.RunMessage{{Role: "user", Text: "old question"}, {Role: "assistant", Text: "exact old answer"}}}}
	server := &Server{backend: backend}
	zero := 0
	projected, wireError := server.lookupDurable(t.Context(), "old_run", &zero)
	if wireError != nil || projected.ID != "old_run" || projected.ContextID != "parent" || projected.Status.State != model.TaskStateCompleted || len(projected.History) != 0 || projected.Artifacts[0].Parts[0].Text() != "exact old answer" {
		t.Fatalf("projection=%+v error=%+v", projected, wireError)
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"historyReconstructed":true`) {
		t.Fatal(string(encoded))
	}
	backend.detail.Finished = false
	backend.detail.Interrupted = true
	backend.detail.Recoverable = true
	backend.detail.StopReason = ""
	projected, wireError = server.lookupDurable(t.Context(), "old_run", nil)
	if wireError != nil || projected.Status.State != model.TaskStateFailed {
		t.Fatalf("interrupted=%+v %+v", projected, wireError)
	}
	encoded, err = json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"recoverable":true`) || !strings.Contains(string(encoded), "Host process ended before this run finished") {
		t.Fatal(string(encoded))
	}
}
func TestDurableLookupLimitsAreNotFalseNotFound(t *testing.T) {
	for _, test := range []struct {
		err    error
		code   int
		reason string
	}{
		{journal.ErrRunNotFound, -32001, "TASK_NOT_FOUND"}, {journal.ErrLookupLimit, -32004, "lookup_limit"},
		{journal.ErrRunBusy, -32004, "task_owner_elsewhere"}, {journal.ErrRunAmbiguous, -32006, "ambiguous_run"},
		{errors.New("corrupt journal secret"), -32006, "journal_inspection_failed"},
	} {
		backend := &durableBackend{lookupError: test.err}
		_, wireError := (&Server{backend: backend}).lookupDurable(t.Context(), "old_run", nil)
		raw, err := json.Marshal(wireError)
		if err != nil {
			t.Fatal(err)
		}
		if wireError.Code != test.code || !strings.Contains(string(raw), test.reason) || strings.Contains(string(raw), "secret") || backend.lookups != 1 {
			t.Fatalf("lookup error=%s calls=%d", raw, backend.lookups)
		}
	}
}
func TestJobProgressDoesNotEndRootOrExposePrivateResults(t *testing.T) {
	at := time.Now().UTC()
	projected := projection("root", model.TaskStateWorking, at)
	projected.projectJobs(tempest.Snapshot{Run: tempest.RunState{RunID: "root"}, Jobs: []tempest.Job{{ID: "job_one", Kind: "task", State: "completed", ResultText: "PRIVATE CHILD RESULT", Cwd: "/private/worktree", Value: json.RawMessage(`{"private":true}`)}}})
	raw, err := json.Marshal(projected.snapshot(nil, true))
	if err != nil {
		t.Fatal(err)
	}
	if projected.value.Status.State.Terminal() || !strings.Contains(string(raw), `"jobs"`) || strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), "/private") || len(projected.value.Artifacts) != 0 {
		t.Fatal(string(raw))
	}
}
