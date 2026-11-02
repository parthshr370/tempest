package outbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/control"
	"go.harness.dev/harness/internal/githubsource/gh"
	"go.harness.dev/harness/internal/githubsource/ghfake"
	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/tempest/durable"
)

func TestMain(m *testing.M) {
	ghfake.MainIfFake()
	os.Exit(m.Run())
}

const repoName = "acme/widgets"

var (
	start   = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	botUser = ghfake.User{Login: "tempest-bot", ID: 100, Type: ghfake.TypeUser}
	parth   = ghfake.User{Login: "parth", ID: 1111, Type: ghfake.TypeUser}
	alice   = ghfake.User{Login: "alice", ID: 1112, Type: ghfake.TypeUser}
)

func actor(user ghfake.User) model.Actor {
	return model.Actor{Login: user.Login, ID: user.ID, Type: user.Type}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(step time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(step)
}

var errCrash = errors.New("injected crash")

// crashStore makes a chosen commit durable and then reports failure, as a
// process that died after the write but before the acknowledgment.
type crashStore struct {
	durable.Store
	mu   sync.Mutex
	fail func(commitID string) bool
}

func (c *crashStore) Commit(ctx context.Context, commit durable.Commit) (durable.Receipt, error) {
	receipt, err := c.Store.Commit(ctx, commit)
	c.mu.Lock()
	fail := c.fail
	c.mu.Unlock()
	if err == nil && fail != nil && fail(commit.ID) {
		return durable.Receipt{}, errCrash
	}
	return receipt, err
}

func (c *crashStore) failOn(fail func(commitID string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail = fail
}

// world is one repository: the fake GitHub with a bare repository as its
// git storage, a local clone, the control journal and the outbox. restart
// rebuilds the control store from its file and a new outbox, as a process
// restart would.
type world struct {
	t       *testing.T
	ctx     context.Context
	clock   *fakeClock
	server  *ghfake.Server
	repo    *ghfake.Repo
	writes  *ghfake.Writes
	dir     string
	bare    string
	clone   string
	gitEnv  []string
	config  model.Config
	journal string
	crash   *crashStore
	store   *control.Store
	outbox  *Outbox
}

func newWorld(t *testing.T) *world {
	t.Helper()
	dir := t.TempDir()
	w := &world{t: t, ctx: context.Background(), clock: &fakeClock{now: start}, dir: dir, journal: filepath.Join(dir, "control.jsonl")}
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	w.gitEnv = []string{"HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=worker", "GIT_AUTHOR_EMAIL=worker@example.com", "GIT_COMMITTER_NAME=worker", "GIT_COMMITTER_EMAIL=worker@example.com"}
	w.bare = filepath.Join(dir, "remote.git")
	w.clone = filepath.Join(dir, "clone")
	w.git(dir, "init", "--quiet", "--bare", "-b", "main", w.bare)
	w.git(dir, "init", "--quiet", "-b", "main", w.clone)
	w.commit(w.clone, "README.md", "widgets\n")
	w.git(w.clone, "remote", "add", "origin", w.bare)
	w.git(w.clone, "push", "--quiet", "origin", "main")

	w.server = ghfake.NewServer(t)
	w.repo = ghfake.NewRepo(w.server, repoName, botUser, start)
	w.writes = w.repo.ServeWrites(w.server, w.bare)
	w.config = model.Config{
		Version:    1,
		Repository: repoName,
		Account:    model.AccountConfig{Login: botUser.Login, ID: botUser.ID, GHPath: ghfake.Executable(), GHConfigDir: filepath.Join(dir, "gh")},
		Clone:      w.clone,
		StateDir:   dir,
		Admission:  model.AdmissionConfig{Mode: model.ModeObserve, Actors: []model.ActorID{{Login: parth.Login, ID: parth.ID}}},
		Workers:    model.WorkersConfig{Max: 4},
		Budgets: model.BudgetsConfig{
			Run:       model.RunBudget{MaxTurns: 10, MaxCostUSD: 1, MaxWallTime: model.Duration(time.Hour)},
			TicketUSD: 10, RepoUSD: 100, RepoPeriod: model.Duration(720 * time.Hour), MinRunUSD: 0.1, MaxAutoWakesPerDay: 5,
		},
		Merge: "manual",
	}
	w.open()
	t.Cleanup(w.close)
	return w
}

func (w *world) git(dir string, args ...string) string {
	w.t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	command.Env = append(os.Environ(), w.gitEnv...)
	out, err := command.CombinedOutput()
	if err != nil {
		w.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes a file in a checkout, commits it and returns the SHA.
func (w *world) commit(dir, name, content string) string {
	w.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		w.t.Fatal(err)
	}
	w.git(dir, "add", name)
	w.git(dir, "commit", "--quiet", "-m", "change "+name)
	return w.git(dir, "rev-parse", "HEAD")
}

// remoteHead is the bare repository's branch head, empty when absent.
func (w *world) remoteHead(branch string) string {
	w.t.Helper()
	command := exec.Command("git", "--git-dir", w.bare, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	out, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (w *world) open() {
	w.t.Helper()
	log, err := durable.OpenFile(w.ctx, w.journal)
	if err != nil {
		w.t.Fatalf("open journal: %v", err)
	}
	w.crash = &crashStore{Store: log}
	repo := model.RepoMeta{Repo: repoName, RepoID: 1, DefaultBranch: "main", Account: actor(botUser), CreatedAt: start}
	store, err := control.Open(w.ctx, control.Options{Store: w.crash, Config: w.config, Repo: repo, Clock: w.clock})
	if err != nil {
		w.t.Fatalf("open control: %v", err)
	}
	w.store = store
	client := gh.New(gh.Options{Path: ghfake.Executable(), ConfigDir: w.config.Account.GHConfigDir, Env: w.server.Env(), Clock: w.repo})
	outbox, err := New(Options{Client: client, Store: store, Config: w.config, Clock: w.clock, GitEnv: w.gitEnv, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		w.t.Fatalf("new outbox: %v", err)
	}
	w.outbox = outbox
}

func (w *world) close() {
	if w.store != nil {
		if err := w.store.Close(w.ctx); err != nil {
			w.t.Errorf("close control: %v", err)
		}
		w.store, w.outbox = nil, nil
	}
}

// restart drops the outbox and the control store and opens both again.
func (w *world) restart() {
	w.t.Helper()
	w.close()
	w.open()
}

// publish runs one outbox pass.
func (w *world) publish() error {
	return w.outbox.publish(w.ctx)
}

// drain runs passes until nothing is due, failing on a pass error.
func (w *world) drain() {
	w.t.Helper()
	for range 10 {
		if err := w.publish(); err != nil {
			w.t.Fatalf("publish: %v", err)
		}
		due, err := w.store.Due(w.ctx, w.clock.Now())
		if err != nil {
			w.t.Fatal(err)
		}
		if len(due) == 0 {
			return
		}
	}
	w.t.Fatal("outbox did not drain")
}

func session(id model.TicketID) string { return "worker-" + string(id) }

// launch ingests an authorized request on thread and walks its dispatch to
// submitted with a worktree on the Ticket's branch, as the dispatcher would.
func (w *world) launch(thread model.ThreadRef, extra ...model.Event) model.TicketID {
	w.t.Helper()
	kind, id := model.EventIssueOpened, model.IssueOpenedID(thread.Number)
	if thread.IsPR {
		kind, id = model.EventComment, model.CommentID(int64(9000+thread.Number), w.clock.Now())
	}
	request := model.Event{ID: id, Kind: kind, Thread: thread, Actor: actor(parth), At: w.clock.Now(), ObservedAt: w.clock.Now(), Title: "Fix the widget", Body: "@tempest-bot please fix", Mention: true}
	if _, err := w.store.Ingest(w.ctx, model.IntakeBatch{Resource: fmt.Sprintf("thread:%d", thread.Number), Events: append([]model.Event{request}, extra...), ReadAt: w.clock.Now()}); err != nil {
		w.t.Fatalf("ingest: %v", err)
	}
	ticket := model.NewTicketID(thread, id)
	admitted, err := w.store.AdmitNext(w.ctx, w.clock.Now())
	if err != nil || len(admitted) != 1 || admitted[0].Ticket.ID != ticket {
		w.t.Fatalf("admit %s: %+v %v", ticket, admitted, err)
	}
	dispatch := admitted[0].Dispatch
	worktree := filepath.Join(w.dir, "worktrees", string(ticket))
	w.git(w.clone, "worktree", "add", "--quiet", "-b", model.TicketBranch(ticket), worktree, "main")
	steps := []func() error{
		func() error { return w.store.Preparing(w.ctx, dispatch.ID, w.clock.Now()) },
		func() error { return w.store.Prepared(w.ctx, ticket, worktree, w.git(w.clone, "rev-parse", "main")) },
		func() error {
			return w.store.Bind(w.ctx, model.SessionBinding{SessionID: session(ticket), Ticket: ticket, Worktree: worktree, BoundAt: w.clock.Now()})
		},
		func() error {
			return w.store.Submitted(w.ctx, dispatch.ID, model.SubmissionReceipt{RequestID: dispatch.RequestID, SubmissionID: "sub", RunID: "run", ThroughInput: dispatch.ThroughInput, At: w.clock.Now()})
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			w.t.Fatalf("launch %s: %v", ticket, err)
		}
	}
	return ticket
}

func (w *world) worktree(id model.TicketID) string {
	return filepath.Join(w.dir, "worktrees", string(id))
}

func (w *world) enqueue(ticket model.TicketID, callID string, kind model.EffectKind, payload model.EffectPayload) model.OutboxIntent {
	w.t.Helper()
	intent, err := w.store.Enqueue(w.ctx, model.WorkerCall{SessionID: session(ticket), ToolCallID: callID}, model.EffectRequest{Kind: kind, Payload: payload})
	if err != nil {
		w.t.Fatalf("enqueue %s: %v", kind, err)
	}
	return intent
}

func (w *world) intent(ticket model.TicketID, id model.OutboxID) model.OutboxIntent {
	w.t.Helper()
	intent, err := w.store.Effect(w.ctx, session(ticket), id)
	if err != nil {
		w.t.Fatalf("intent %s: %v", id, err)
	}
	return intent
}

// requests counts the fake's requests with method and a path suffix.
func (w *world) requests(method, pathSuffix string) []ghfake.RecordedRequest {
	var out []ghfake.RecordedRequest
	for _, request := range w.server.Requests() {
		if request.Method == method && strings.HasSuffix(request.Path, pathSuffix) {
			out = append(out, request)
		}
	}
	return out
}

func (w *world) marked(number int, id model.OutboxID) []ghfake.Comment {
	var out []ghfake.Comment
	for _, comment := range w.repo.Comments(number) {
		if strings.Contains(comment.Body, model.OutboxMarker(id)) {
			out = append(out, comment)
		}
	}
	return out
}
