package tempest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest/durable"
)

func TestHostCatalogPublicationAndStablePaging(t *testing.T) {
	provider := faux.New(faux.Options{})
	options := HostOptions{SessionDir: t.TempDir(), Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}}
	first, err := OpenHost(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close(context.Background())
	second, err := OpenHost(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close(context.Background())
	cwd := t.TempDir()
	var wait sync.WaitGroup
	for _, host := range []*Host{first, second} {
		wait.Go(func() {
			if _, err := host.Create(t.Context(), AgentOptions{Cwd: cwd}); err != nil {
				t.Error(err)
			}
		})
	}
	wait.Wait()
	page, err := first.List(t.Context(), ListOptions{Cwd: cwd, Limit: 1})
	if err != nil || len(page.Sessions) != 1 || page.NextCursor == "" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if _, err := second.Create(t.Context(), AgentOptions{Cwd: cwd}); err != nil {
		t.Fatal(err)
	}
	next, err := first.List(t.Context(), ListOptions{Cwd: cwd, Limit: 1, Cursor: page.NextCursor})
	if err != nil || len(next.Sessions) != 1 || next.NextCursor != "" || next.Sessions[0].SessionID == page.Sessions[0].SessionID {
		t.Fatalf("next=%+v err=%v", next, err)
	}
	if _, err := first.Create(t.Context(), AgentOptions{Cwd: t.TempDir(), New: &NewSession{MCPServers: []MCPServerConfig{{Name: "bad", Command: "/does-not-exist"}}}}); err == nil {
		t.Fatal("failed startup accepted")
	}
	all, err := first.List(t.Context(), ListOptions{Limit: 100})
	if err != nil || len(all.Sessions) != 3 {
		t.Fatalf("partial creation leaked: %+v err=%v", all, err)
	}
}

func TestHostLimitsFailFastAndRelease(t *testing.T) {
	host, err := OpenHost(t.Context(), HostOptions{SessionDir: t.TempDir(), Limits: HostLimits{MaxConcurrentRuns: 1, MaxConcurrentProcesses: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	for _, admit := range []func(context.Context) (func(), error){host.admitRun, host.admitProcess} {
		release, err := admit(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admit(t.Context()); !errors.Is(err, ErrHostLimit) {
			t.Fatalf("capacity=%v", err)
		}
		release()
		release()
		release, err = admit(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
}

type failingHostStore struct {
	durable.Store
	closed  bool
	failure error
}

func (store *failingHostStore) Read(context.Context, durable.Query) (durable.Page, error) {
	return durable.Page{}, store.failure
}
func (store *failingHostStore) Close(context.Context) error { store.closed = true; return nil }
func TestHostFailedOpenRetainsCallerStore(t *testing.T) {
	failure := errors.New("backend unavailable")
	store := &failingHostStore{failure: failure}
	if _, err := OpenHost(t.Context(), HostOptions{SessionDir: t.TempDir(), Store: store}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if store.closed {
		t.Fatal("failed constructor took caller store ownership")
	}
}

func TestHostSlowCreateDoesNotBlockList(t *testing.T) {
	provider := faux.New(faux.Options{})
	host, err := OpenHost(t.Context(), HostOptions{SessionDir: t.TempDir(), Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		http.Error(w, "stopped", http.StatusServiceUnavailable)
	}))
	defer endpoint.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := host.Create(ctx, AgentOptions{Cwd: t.TempDir(), New: &NewSession{MCPServers: []MCPServerConfig{{Name: "slow", Transport: "http", URL: endpoint.URL}}}})
		done <- err
	}()
	defer func() {
		cancel()
		close(release)
		<-done
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("MCP initialization did not start")
	}
	listCtx, stop := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer stop()
	if _, err := host.List(listCtx, ListOptions{}); err != nil {
		t.Fatalf("slow initialization blocked independent List: %v", err)
	}
}

func TestHostReopensDirectlyClosedSessionAndListsArchivedRun(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.AppendResponses(faux.Respond(faux.Text("saved")))
	host, err := OpenHost(t.Context(), HostOptions{SessionDir: t.TempDir(), Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	first, err := host.Create(t.Context(), AgentOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Prompt(t.Context(), Prompt{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := host.Open(t.Context(), first.ID())
	if err != nil || reopened == first {
		t.Fatalf("host reused closed writer: same=%t err=%v", reopened == first, err)
	}
	if err := host.CloseSession(t.Context(), reopened.ID()); err != nil {
		t.Fatal(err)
	}
	second, err := host.Create(t.Context(), AgentOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	list, err := host.List(t.Context(), ListOptions{})
	if err != nil || len(list.Sessions) != 2 || list.Sessions[0].SessionID != second.ID() {
		t.Fatalf("newest-first list=%+v err=%v", list, err)
	}
	if list.Sessions[1].LastRun == nil || !list.Sessions[1].LastRun.Finished || list.Sessions[1].LastRun.StopReason != "completed" {
		t.Fatalf("archived run lost: %+v", list.Sessions[1])
	}
	if err := os.Remove(first.core.store.Path()); err != nil {
		t.Fatal(err)
	}
	list, err = host.List(t.Context(), ListOptions{})
	if err != nil || len(list.Sessions) != 1 || list.Sessions[0].SessionID != second.ID() {
		t.Fatalf("missing archive stopped listing: %+v err=%v", list, err)
	}
}

func TestHostRunLimitRejectsRealChildWithoutDeadlock(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.AppendResponses(
		faux.Respond(faux.ToolCall("task", json.RawMessage(`{"prompt":"child work","subagent_type":"general"}`), "child")),
		faux.Respond(faux.Text("parent completed")),
		faux.Respond(faux.Text("parent settled")),
	)
	host, err := OpenHost(t.Context(), HostOptions{
		SessionDir: t.TempDir(),
		Stack:      &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple, EnableTask: true},
		Limits:     HostLimits{MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	owner, err := host.Create(t.Context(), AgentOptions{Cwd: t.TempDir(), New: &NewSession{PermissionMode: "bypass"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := owner.Prompt(ctx, Prompt{Text: "delegate"})
	if err != nil || result.StopReason != "completed" {
		t.Fatalf("parent blocked on child limit: %+v %v", result, err)
	}
	page, err := owner.Jobs(t.Context(), "", 10)
	if err != nil || len(page.Jobs) != 1 {
		t.Fatalf("child jobs=%+v err=%v", page, err)
	}
	jobs, err := owner.WaitJobs(t.Context(), []string{page.Jobs[0].ID}, time.Second)
	if err != nil || len(jobs) != 1 || jobs[0].State != "failed" || !strings.Contains(jobs[0].ResultText, ErrHostLimit.Error()) {
		t.Fatalf("child limit outcome=%+v err=%v", jobs, err)
	}
	release, err := host.admitRun(t.Context())
	if err != nil {
		t.Fatalf("parent did not release permit: %v", err)
	}
	release()
}

func TestHostResumePendingAttemptsEveryOpenSession(t *testing.T) {
	provider := faux.New(faux.Options{})
	attempted := make(map[string]bool)
	host, err := OpenHost(t.Context(), HostOptions{
		SessionDir: t.TempDir(),
		Stack:      &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple},
		Authorize: func(_ context.Context, access Access) error {
			if access.Action == "resume_pending" {
				attempted[access.SessionID] = true
				return ErrRunActive
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	for range 2 {
		if _, err := host.Create(t.Context(), AgentOptions{Cwd: t.TempDir()}); err != nil {
			t.Fatal(err)
		}
	}
	err = host.ResumePending(t.Context())
	if !errors.Is(err, ErrRunActive) || len(attempted) != 2 {
		t.Fatalf("host stopped early: attempted=%v error=%v", attempted, err)
	}
}

func TestHostMemoryCatalogDoesNotRequireJournalFiles(t *testing.T) {
	provider := faux.New(faux.Options{})
	host, err := OpenHost(t.Context(), HostOptions{SessionDir: t.TempDir(), Store: durable.NewMemory(), Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	session, err := host.Create(t.Context(), AgentOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.CloseSession(t.Context(), session.ID()); err != nil {
		t.Fatal(err)
	}
	page, err := host.List(t.Context(), ListOptions{})
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].SessionID != session.ID() {
		t.Fatalf("memory catalog treated its session as a missing journal: %+v %v", page, err)
	}
	if _, err := host.Open(t.Context(), session.ID()); err != nil {
		t.Fatalf("memory session failed reopen: %v", err)
	}
}
