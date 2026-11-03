package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/githubsource/control"
	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/internal/prompt"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

var (
	epoch   = time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	account = model.Actor{Login: "tempest-bot", ID: 100, Type: "User"}
	parth   = model.Actor{Login: "parth", ID: 1111, Type: "User"}
)

// errCrash is what a crash point returns: the process "dies" there.
var errCrash = errors.New("injected crash")

// waitLimit bounds every eventual condition of these tests.
const waitLimit = 30 * time.Second

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(step time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(step)
	return c.now
}

// reply is one scripted provider response for a Ticket's worker. A gate
// holds the response until the test closes it.
type reply struct {
	gate    chan struct{}
	content []types.ContentBlock
}

// workerScript routes every provider request to the Ticket named in the
// worker's system prompt (rendered by the test extension from the
// WorkerTicket document) and answers from that Ticket's script.
type workerScript struct {
	provider *faux.Faux
	mu       sync.Mutex
	replies  map[model.TicketID][]reply
	calls    map[model.TicketID][]string
	active   int
	peak     int
}

func newWorkerScript() *workerScript {
	cost := types.ModelCost{Input: 10, Output: 10}
	return &workerScript{
		provider: faux.New(faux.Options{Models: []faux.ModelDef{{ID: "worker-model", Cost: cost}}}),
		replies:  make(map[model.TicketID][]reply),
		calls:    make(map[model.TicketID][]string),
	}
}

func (w *workerScript) script(ticket model.TicketID, replies ...reply) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.replies[ticket] = append(w.replies[ticket], replies...)
}

// requests returns the transcripts the Ticket's worker sent, one per call.
func (w *workerScript) requests(ticket model.TicketID) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.calls[ticket]...)
}

func (w *workerScript) peakConcurrency() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.peak
}

func (w *workerScript) stream(ctx context.Context, worker types.Model, request types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
	w.provider.AppendResponses(w.respond)
	return w.provider.StreamSimple(ctx, worker, request, options)
}

const ticketMarker = "github-ticket-id: "

// childMarker opens a delegated child's system prompt; children answer
// from the childScript bucket so they never consume their parent's script.
const (
	childMarker = "You are a delegated child."
	childScript = model.TicketID("child")
)

func (w *workerScript) respond(request types.Context, options *types.StreamOptions, state faux.State, worker types.Model) (types.AssistantMessage, error) {
	ticket := model.TicketID("")
	if at := strings.Index(request.SystemPrompt, ticketMarker); at >= 0 {
		ticket = model.TicketID(strings.Fields(request.SystemPrompt[at+len(ticketMarker):])[0])
	}
	if strings.Contains(request.SystemPrompt, childMarker) {
		ticket = childScript
	}
	// Only the messages since the worker's last answer: earlier ones repeat
	// in every later request.
	transcript, err := json.Marshal(request.Messages[newSince(request.Messages):])
	if err != nil {
		return types.AssistantMessage{}, err
	}
	w.mu.Lock()
	w.calls[ticket] = append(w.calls[ticket], string(transcript))
	next := reply{content: []types.ContentBlock{faux.Text("done")}}
	if queued := w.replies[ticket]; len(queued) > 0 {
		next, w.replies[ticket] = queued[0], queued[1:]
	}
	w.active++
	w.peak = max(w.peak, w.active)
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.active--
		w.mu.Unlock()
	}()
	if next.gate != nil {
		select {
		case <-next.gate:
		case <-state.Context.Done():
			// Stream the reply into the cancelled context, so the run
			// ends aborted as with a real provider.
			next.content = []types.ContentBlock{faux.Text("cancelled")}
		}
	}
	return faux.Respond(next.content...)(request, options, state, worker)
}

// newSince is the index after the last assistant message.
func newSince(messages []types.Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		switch messages[i].(type) {
		case types.AssistantMessage, *types.AssistantMessage:
			return i + 1
		}
	}
	return 0
}

func text(body string) reply { return reply{content: []types.ContentBlock{faux.Text(body)}} }

func gated(gate chan struct{}, content ...types.ContentBlock) reply {
	if len(content) == 0 {
		content = []types.ContentBlock{faux.Text("done")}
	}
	return reply{gate: gate, content: content}
}

func toolCall(name, id string, arguments any) types.ContentBlock {
	raw, err := json.Marshal(arguments)
	if err != nil {
		panic(err)
	}
	return faux.ToolCall(name, raw, id)
}

// testExtension is a minimal real github bundle: the WorkerTicket document,
// one section that names the Ticket from it, and a report tool calling the
// control gateway. It is not a stand-in for G4's bundle.
func testExtension(gateway func() model.WorkerGateway) extension.Definition {
	reportSchema := json.RawMessage(`{"type":"object","properties":{"status":{"type":"string"},"summary":{"type":"string"}},"required":["status","summary"]}`)
	return extension.Definition{
		ID:        model.ExtensionID,
		Revision:  "test",
		Documents: []durable.DocumentDefinition{model.WorkerDocumentDefinition()},
		Sections: []extension.Section{{Name: "github-ticket", Render: func(ctx context.Context, view extension.ReadView) (string, error) {
			page, err := view.Read(ctx, durable.Query{Kind: durable.DocumentKind, Namespace: model.WorkerNamespace, ID: model.WorkerDocument, Limit: 1})
			if err != nil || len(page.Records) == 0 {
				return "", err
			}
			var document model.WorkerTicket
			if err := json.Unmarshal(page.Records[0].Value, &document); err != nil {
				return "", err
			}
			if document.Ticket == "" {
				return "github ticket: not bound", nil
			}
			return ticketMarker + string(document.Ticket) + "\n", nil
		}}},
		Tools: []extension.Tool{{
			Name: model.ToolReport, Description: "Report the ticket's status.", InputSchema: reportSchema,
			Effect: extension.EffectWrite, Replay: extension.ReplaySafe,
			Execute: func(ctx context.Context, call extension.Call, invocation extension.Invocation) (extension.Result, error) {
				var request model.ReportRequest
				if err := json.Unmarshal(call.Arguments, &request); err != nil {
					return extension.Result{}, err
				}
				worker := model.WorkerCall{SessionID: invocation.Identity().SessionID, ToolCallID: call.ID, At: epoch}
				if _, err := gateway().Report(ctx, worker, request); err != nil {
					return extension.Result{Content: []extension.Content{{Type: "text", Text: err.Error()}}, IsError: true}, nil
				}
				return extension.Result{Content: []extension.Content{{Type: "text", Text: "recorded"}}}, nil
			},
		}},
	}
}

// hostStore is the worker sessions' transaction domain. crash makes it
// refuse every later write and drops the dead host's session leases, as a
// process death would; what was committed before stays.
type hostStore struct {
	log      *durable.Log
	faults   *storeFaults
	mu       sync.Mutex
	crashed  bool
	releases []func()
}

// storeFaults injects worker store faults that outlive a restart: read
// fails a matching read, committed observes every landed commit.
type storeFaults struct {
	read      atomic.Pointer[func(durable.Query) error]
	committed atomic.Pointer[func(durable.Commit)]
}

func (s *hostStore) Read(ctx context.Context, query durable.Query) (durable.Page, error) {
	if fault := s.faults.read.Load(); fault != nil {
		if err := (*fault)(query); err != nil {
			return durable.Page{}, err
		}
	}
	return s.log.Read(ctx, query)
}

func (s *hostStore) Commit(ctx context.Context, commit durable.Commit) (durable.Receipt, error) {
	s.mu.Lock()
	crashed := s.crashed
	s.mu.Unlock()
	if crashed {
		return durable.Receipt{}, errCrash
	}
	receipt, err := s.log.Commit(ctx, commit)
	if err == nil {
		if observe := s.faults.committed.Load(); observe != nil {
			(*observe)(commit)
		}
	}
	return receipt, err
}

func (s *hostStore) Close(context.Context) error { return nil }

func (s *hostStore) AcquireSession(ctx context.Context, id string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.crashed {
		return nil, errCrash
	}
	release, err := s.log.AcquireSession(ctx, id)
	if err != nil {
		return nil, err
	}
	s.releases = append(s.releases, release)
	return release, nil
}

func (s *hostStore) crash() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.crashed = true
	for _, release := range s.releases {
		release()
	}
}

// crashingStore fails every control call once the world crashed.
type crashingStore struct {
	inner   *control.Store
	crashed *atomic.Bool
}

func (s *crashingStore) alive() error {
	if s.crashed.Load() {
		return errCrash
	}
	return nil
}

func (s *crashingStore) AdmitNext(ctx context.Context, now time.Time) ([]model.AdmittedDispatch, error) {
	if err := s.alive(); err != nil {
		return nil, err
	}
	return s.inner.AdmitNext(ctx, now)
}

func (s *crashingStore) Active(ctx context.Context) ([]model.AdmittedDispatch, error) {
	if err := s.alive(); err != nil {
		return nil, err
	}
	return s.inner.Active(ctx)
}

func (s *crashingStore) Deliveries(ctx context.Context) ([]model.Delivery, error) {
	if err := s.alive(); err != nil {
		return nil, err
	}
	return s.inner.Deliveries(ctx)
}

func (s *crashingStore) Preparing(ctx context.Context, id model.DispatchID, now time.Time) error {
	return s.guard(func() error { return s.inner.Preparing(ctx, id, now) })
}

func (s *crashingStore) Prepared(ctx context.Context, id model.TicketID, worktree, baseSHA string) error {
	return s.guard(func() error { return s.inner.Prepared(ctx, id, worktree, baseSHA) })
}

func (s *crashingStore) Bind(ctx context.Context, binding model.SessionBinding) error {
	return s.guard(func() error { return s.inner.Bind(ctx, binding) })
}

func (s *crashingStore) Submitted(ctx context.Context, id model.DispatchID, receipt model.SubmissionReceipt) error {
	return s.guard(func() error { return s.inner.Submitted(ctx, id, receipt) })
}

func (s *crashingStore) Delivered(ctx context.Context, id model.TicketID, gen uint32, through uint64) error {
	return s.guard(func() error { return s.inner.Delivered(ctx, id, gen, through) })
}

func (s *crashingStore) Finished(ctx context.Context, id model.DispatchID, outcome model.RunOutcome, now time.Time) error {
	return s.guard(func() error { return s.inner.Finished(ctx, id, outcome, now) })
}

func (s *crashingStore) Abandon(ctx context.Context, id model.DispatchID, failure model.DispatchFailure) error {
	return s.guard(func() error { return s.inner.Abandon(ctx, id, failure) })
}

func (s *crashingStore) Inputs(ctx context.Context, id model.TicketID, after uint64, limit int) ([]model.InputView, error) {
	if err := s.alive(); err != nil {
		return nil, err
	}
	return s.inner.Inputs(ctx, id, after, limit)
}

func (s *crashingStore) guard(call func() error) error {
	if err := s.alive(); err != nil {
		return err
	}
	return call()
}

// world is a repository, its control journal and a worker host that the
// test can crash and restart at any dispatcher step.
type world struct {
	t                     *testing.T
	ctx                   context.Context
	root, remote, clone   string
	controlPath, sessions string
	config                model.Config
	limits                tempest.HostLimits
	enableTask            bool
	clock                 *fakeClock
	worker                *workerScript
	log                   *durable.Log
	faults                storeFaults
	registry              *extension.Registry
	crashed               atomic.Bool
	crashAt               atomic.Value
	onPoint               atomic.Pointer[func(point string)]
	nextComment           int64

	mu         sync.Mutex
	hostStore  *hostStore
	host       *tempest.Host
	control    *control.Store
	dispatcher *Dispatcher
	stop       context.CancelFunc
	done       chan error
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.test", "GIT_CONFIG_NOSYSTEM=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func testConfig(root string) model.Config {
	return model.Config{
		Version:    1,
		Repository: "acme/widgets",
		Account:    model.AccountConfig{Login: account.Login, ID: account.ID},
		Clone:      filepath.Join(root, "clone"),
		StateDir:   filepath.Join(root, "state"),
		Poll:       model.PollConfig{Retention: model.Duration(336 * time.Hour)},
		Admission:  model.AdmissionConfig{Mode: model.ModeObserve, Actors: []model.ActorID{{Login: parth.Login, ID: parth.ID}}},
		Workers:    model.WorkersConfig{Max: 2, PermissionMode: "bypass"},
		Budgets: model.BudgetsConfig{
			Run:                model.RunBudget{MaxTurns: 60, MaxCostUSD: 2, MaxWallTime: model.Duration(45 * time.Minute), MaxToolCalls: 300},
			TicketUSD:          10,
			RepoUSD:            100,
			RepoPeriod:         model.Duration(720 * time.Hour),
			MinRunUSD:          0.2,
			MaxAutoWakesPerDay: 12,
		},
		Merge: "manual",
	}
}

func newWorld(t *testing.T, configure func(*world)) *world {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, ctx: context.Background(), root: root, remote: filepath.Join(root, "remote.git"), controlPath: filepath.Join(root, "control.jsonl"), sessions: filepath.Join(root, "sessions"), clock: &fakeClock{now: epoch}, worker: newWorkerScript(), log: durable.NewMemory()}
	w.config = testConfig(root)
	w.clone = w.config.Clone
	if configure != nil {
		configure(w)
	}
	w.crashAt.Store("")
	gitRun(t, root, "init", "--bare", "-b", "main", w.remote)
	seed := filepath.Join(root, "seed")
	gitRun(t, root, "clone", w.remote, seed)
	gitRun(t, seed, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("widgets\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "-m", "base")
	gitRun(t, seed, "push", "origin", "main")
	gitRun(t, root, "clone", w.remote, w.clone)
	w.registry = extension.NewRegistry()
	if err := w.registry.Install(w.ctx, testExtension(func() model.WorkerGateway { return w.store() })); err != nil {
		t.Fatal(err)
	}
	w.boot()
	t.Cleanup(w.shutdown)
	return w
}

func (w *world) store() *control.Store {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.control
}

func (w *world) currentHost() *tempest.Host {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.host
}

// boot opens the worker host, the control journal and a dispatcher, and
// starts Run, as a process start would.
func (w *world) boot() {
	w.t.Helper()
	store := &hostStore{log: w.log, faults: &w.faults}
	host, err := tempest.OpenHost(w.ctx, tempest.HostOptions{
		SessionDir: w.sessions,
		Store:      store,
		Registry:   w.registry,
		Limits:     w.limits,
		Stack:      &tempest.StackConfig{Model: w.worker.provider.Model(), StreamFn: w.worker.stream, ContextFiles: []prompt.ContextFile{}, EnableTask: w.enableTask},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	journal, err := durable.OpenFile(w.ctx, w.controlPath)
	if err != nil {
		w.t.Fatal(err)
	}
	controlStore, err := control.Open(w.ctx, control.Options{Store: journal, Config: w.config, Repo: model.RepoMeta{Repo: w.config.Repository, RepoID: 42, DefaultBranch: "main", Account: account, CreatedAt: epoch}, Clock: w.clock})
	if err != nil {
		w.t.Fatal(err)
	}
	dispatcher, err := New(Options{Host: host, Store: &crashingStore{inner: controlStore, crashed: &w.crashed}, Config: w.config, Repo: model.RepoMeta{DefaultBranch: "main"}, Clock: w.clock, Interval: time.Hour})
	if err != nil {
		w.t.Fatal(err)
	}
	dispatcher.hook = w.hook
	ctx, stop := context.WithCancel(w.ctx)
	done := make(chan error, 1)
	w.mu.Lock()
	w.hostStore, w.host, w.control, w.dispatcher, w.stop, w.done = store, host, controlStore, dispatcher, stop, done
	w.mu.Unlock()
	go func() { done <- dispatcher.Run(ctx) }()
}

func (w *world) hook(point string) error {
	if onPoint := w.onPoint.Load(); onPoint != nil {
		(*onPoint)(point)
	}
	if want, _ := w.crashAt.Load().(string); want != "" && want == point && w.crashed.CompareAndSwap(false, true) {
		w.hostStore.crash()
		return errCrash
	}
	return nil
}

// crash kills the process at an arbitrary moment: nothing it does later
// becomes durable.
func (w *world) crash() {
	if w.crashed.CompareAndSwap(false, true) {
		w.hostStore.crash()
	}
}

// halt stops the dispatcher and closes the host and journal.
func (w *world) halt() {
	w.mu.Lock()
	stop, done, host, controlStore := w.stop, w.done, w.host, w.control
	w.mu.Unlock()
	stop()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		w.t.Errorf("dispatcher run: %v", err)
	}
	if err := controlStore.Close(w.ctx); err != nil && !w.crashed.Load() {
		w.t.Errorf("close control: %v", err)
	}
	if err := host.Close(w.ctx); err != nil && !w.crashed.Load() {
		w.t.Errorf("close host: %v", err)
	}
}

// restart halts the current process (crashed or not) and boots another.
func (w *world) restart() {
	w.t.Helper()
	w.crashAt.Store("")
	w.halt()
	w.crashed.Store(false)
	w.boot()
}

func (w *world) shutdown() {
	w.halt()
	_ = w.registry.Close(context.Background())
}

func (w *world) wake() { w.dispatcherNow().Wake() }

func (w *world) dispatcherNow() *Dispatcher {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dispatcher
}

// mentionEvent builds an authorized @mention comment on issue number.
func (w *world) mentionEvent(number int, body string) model.Event {
	event := w.commentEvent(number, "@"+account.Login+" "+body)
	event.Mention = true
	return event
}

// commentEvent builds an authorized comment without a mention.
func (w *world) commentEvent(number int, body string) model.Event {
	w.nextComment++
	at := w.clock.Advance(time.Second)
	return model.Event{ID: model.CommentID(w.nextComment, at), Kind: model.EventComment, Thread: model.ThreadRef{Number: number}, Actor: parth, At: at, ObservedAt: at, Body: body, BodyBytes: len(body), CommentID: w.nextComment, URL: fmt.Sprintf("https://github.com/acme/widgets/issues/%d#c%d", number, w.nextComment)}
}

// ticketOf is the Ticket a request event creates.
func ticketOf(event model.Event) model.TicketID { return model.NewTicketID(event.Thread, event.ID) }

func (w *world) ingest(event model.Event) model.TicketID {
	w.t.Helper()
	result, err := w.store().Ingest(w.ctx, model.IntakeBatch{Resource: fmt.Sprintf("thread:%d", event.Thread.Number), Events: []model.Event{event}, ReadAt: w.clock.Now()})
	if err != nil {
		w.t.Fatalf("ingest: %v", err)
	}
	w.wake()
	for _, decision := range result.Decisions {
		if len(decision.Tickets) > 0 {
			return decision.Tickets[0]
		}
	}
	w.t.Fatalf("event %s fed no ticket: %+v", event.ID, result.Decisions)
	return ""
}

func (w *world) detail(id model.TicketID) model.TicketDetail {
	w.t.Helper()
	detail, err := w.store().Operator().Ticket(w.ctx, id)
	if err != nil {
		w.t.Fatalf("ticket %s: %v", id, err)
	}
	return detail
}

func (w *world) dispatchRecord(id model.DispatchID) model.DispatchIntent {
	w.t.Helper()
	ticket, _, _ := strings.Cut(string(id), ":")
	for _, dispatch := range w.detail(model.TicketID(ticket)).Dispatches {
		if dispatch.ID == id {
			return dispatch
		}
	}
	w.t.Fatalf("dispatch %s missing", id)
	return model.DispatchIntent{}
}

// eventually polls cond until it holds or the wait limit passes.
func (w *world) eventually(what string, cond func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !cond() {
		if time.Now().After(deadline) {
			w.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// settled waits until the dispatch reached a terminal state.
func (w *world) settled(id model.DispatchID) model.DispatchIntent {
	w.t.Helper()
	var dispatch model.DispatchIntent
	w.eventually("dispatch "+string(id)+" to settle", func() bool {
		dispatch = w.dispatchRecord(id)
		return dispatch.State == model.DispatchFinished || dispatch.State == model.DispatchCancelled
	})
	return dispatch
}

func (w *world) worktree(id model.TicketID) string {
	return filepath.Join(w.config.StateDir, "worktrees", string(id))
}

// sessionsIn lists the worker host's sessions whose cwd is dir.
func (w *world) sessionsIn(dir string) []tempest.SessionInfo {
	w.t.Helper()
	page, err := w.currentHost().List(w.ctx, tempest.ListOptions{Cwd: dir})
	if err != nil {
		w.t.Fatal(err)
	}
	return page.Sessions
}

func (w *world) session(id string) *tempest.Session {
	w.t.Helper()
	session, err := w.currentHost().Open(w.ctx, id)
	if err != nil {
		w.t.Fatal(err)
	}
	return session
}

func containsCount(texts []string, needle string) int {
	count := 0
	for _, text := range texts {
		if strings.Contains(text, needle) {
			count++
		}
	}
	return count
}
