package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/tempest/durable"
)

var (
	epoch   = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	account = model.Actor{Login: "tempest-bot", ID: 100, Type: "User"}
	parth   = model.Actor{Login: "parth", ID: 1111, Type: "User"}
	alice   = model.Actor{Login: "alice", ID: 1112, Type: "User"}
	mallory = model.Actor{Login: "mallory", ID: 3333, Type: "User"}
	rabbit  = model.Actor{Login: "coderabbitai[bot]", ID: 2222, Type: "Bot"}
	ci      = model.Actor{Login: "github-actions[bot]", ID: 4444, Type: "Bot"}
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

func (c *fakeClock) Advance(step time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(step)
	return c.now
}

func testConfig() model.Config {
	return model.Config{
		Version:    1,
		Repository: "acme/widgets",
		Account:    model.AccountConfig{Login: account.Login, ID: account.ID},
		Poll:       model.PollConfig{Retention: model.Duration(336 * time.Hour)},
		Admission: model.AdmissionConfig{
			Mode:   model.ModeObserve,
			Actors: []model.ActorID{{Login: parth.Login, ID: parth.ID}, {Login: alice.Login, ID: alice.ID}},
			Bots:   []model.ActorID{{Login: rabbit.Login, ID: rabbit.ID}},
		},
		Workers: model.WorkersConfig{Max: 2},
		Budgets: model.BudgetsConfig{
			Run:                model.RunBudget{MaxTurns: 60, MaxCostUSD: 2, MaxWallTime: model.Duration(45 * time.Minute), MaxToolCalls: 300},
			TicketUSD:          10,
			RepoUSD:            100,
			RepoPeriod:         model.Duration(720 * time.Hour),
			MinRunUSD:          0.2,
			MaxAutoWakesPerDay: 3,
		},
		Merge: "manual",
	}
}

func testRepo() model.RepoMeta {
	return model.RepoMeta{Repo: "acme/widgets", RepoID: 42, DefaultBranch: "main", Account: account, CreatedAt: epoch}
}

// harness is one control Store over a journal plus its clock.
type harness struct {
	t      *testing.T
	ctx    context.Context
	clock  *fakeClock
	config model.Config
	path   string
	store  *Store
}

func newHarness(t *testing.T) *harness {
	return newHarnessWith(t, testConfig())
}

func newHarnessWith(t *testing.T, config model.Config) *harness {
	t.Helper()
	h := &harness{t: t, ctx: context.Background(), clock: &fakeClock{now: epoch}, config: config, path: filepath.Join(t.TempDir(), "control.jsonl")}
	h.reopen()
	t.Cleanup(func() { h.close() })
	return h
}

func (h *harness) open(store durable.Store) (*Store, error) {
	return Open(h.ctx, Options{Store: store, Config: h.config, Repo: testRepo(), Clock: h.clock})
}

// reopen closes the current Store (if any) and opens the journal file again,
// as a restarted process would.
func (h *harness) reopen() {
	h.t.Helper()
	h.close()
	log, err := durable.OpenFile(h.ctx, h.path)
	if err != nil {
		h.t.Fatalf("open journal: %v", err)
	}
	store, err := h.open(log)
	if err != nil {
		h.t.Fatalf("open control: %v", err)
	}
	h.store = store
}

func (h *harness) close() {
	if h.store != nil {
		if err := h.store.Close(h.ctx); err != nil {
			h.t.Errorf("close: %v", err)
		}
		h.store = nil
	}
}

func (h *harness) at(offset time.Duration) time.Time {
	at := epoch.Add(offset)
	h.clock.Set(at)
	return at
}

func (h *harness) ingest(resource string, events ...model.Event) model.IngestResult {
	h.t.Helper()
	result, err := h.store.Ingest(h.ctx, model.IntakeBatch{Resource: resource, Events: events, ReadAt: h.clock.Now()})
	if err != nil {
		h.t.Fatalf("ingest %s: %v", resource, err)
	}
	return result
}

func (h *harness) ticket(id model.TicketID) model.Ticket {
	h.t.Helper()
	ticket, err := h.store.Ticket(h.ctx, id)
	if err != nil {
		h.t.Fatalf("ticket %s: %v", id, err)
	}
	return ticket
}

func (h *harness) detail(id model.TicketID) model.TicketDetail {
	h.t.Helper()
	detail, err := h.store.Operator().Ticket(h.ctx, id)
	if err != nil {
		h.t.Fatalf("detail %s: %v", id, err)
	}
	return detail
}

func (h *harness) dispatch(id model.DispatchID) model.DispatchIntent {
	h.t.Helper()
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	dispatch, ok := h.store.dispatches.get(string(id))
	if !ok {
		h.t.Fatalf("dispatch %s missing", id)
	}
	return *dispatch
}

func (h *harness) ledger(key model.LedgerKey) model.Ledger {
	h.t.Helper()
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	ledger, ok := h.store.ledgers.get(string(key))
	if !ok {
		h.t.Fatalf("ledger %s missing", key)
	}
	return clone(*ledger)
}

func (h *harness) admit() []model.AdmittedDispatch {
	h.t.Helper()
	admitted, err := h.store.AdmitNext(h.ctx, h.clock.Now())
	if err != nil {
		h.t.Fatalf("admit: %v", err)
	}
	return admitted
}

func sessionOf(id model.TicketID) string { return "sess-" + string(id) }

// start walks an admitted dispatch through the dispatcher's steps up to
// submitted, as G3 would.
func (h *harness) start(admitted model.AdmittedDispatch) {
	h.t.Helper()
	dispatch := admitted.Dispatch
	ticket := dispatch.Ticket
	steps := []func() error{
		func() error { return h.store.Preparing(h.ctx, dispatch.ID, h.clock.Now()) },
		func() error { return h.store.Prepared(h.ctx, ticket, "/wt/"+string(ticket), "base0") },
		func() error {
			return h.store.Bind(h.ctx, model.SessionBinding{SessionID: sessionOf(ticket), Ticket: ticket, Worktree: "/wt/" + string(ticket), BoundAt: h.clock.Now()})
		},
		func() error {
			return h.store.Submitted(h.ctx, dispatch.ID, model.SubmissionReceipt{RequestID: dispatch.RequestID, SubmissionID: "sub-" + string(dispatch.ID), RunID: "run-" + string(dispatch.ID), At: h.clock.Now()})
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			h.t.Fatalf("start %s: %v", dispatch.ID, err)
		}
	}
}

// runAll admits and starts everything admissible.
func (h *harness) runAll() []model.AdmittedDispatch {
	h.t.Helper()
	admitted := h.admit()
	for _, a := range admitted {
		h.start(a)
	}
	return admitted
}

func completed(microUSD int64) model.RunOutcome {
	return model.RunOutcome{StopReason: "completed", Usage: model.Spend{MicroUSD: microUSD}, UsageKnown: true}
}

func (h *harness) finish(id model.DispatchID, outcome model.RunOutcome) {
	h.t.Helper()
	if err := h.store.Finished(h.ctx, id, outcome, h.clock.Now()); err != nil {
		h.t.Fatalf("finish %s: %v", id, err)
	}
}

func (h *harness) report(id model.TicketID, call string, status model.ReportStatus) model.Ticket {
	h.t.Helper()
	ticket, err := h.store.Report(h.ctx, model.WorkerCall{SessionID: sessionOf(id), ToolCallID: call, At: h.clock.Now()}, model.ReportRequest{Status: status, Summary: string(status)})
	if err != nil {
		h.t.Fatalf("report %s: %v", id, err)
	}
	return ticket
}

// Event builders. Comment IDs version by the given time.

func comment(number int, isPR bool, commentID int64, actor model.Actor, body string, at time.Time) model.Event {
	return model.Event{ID: model.CommentID(commentID, at), Kind: model.EventComment, Thread: model.ThreadRef{Number: number, IsPR: isPR}, Actor: actor, At: at, ObservedAt: at, Body: body, BodyBytes: len(body), CommentID: commentID}
}

func mention(number int, isPR bool, commentID int64, actor model.Actor, body string, at time.Time) model.Event {
	event := comment(number, isPR, commentID, actor, "@tempest-bot "+body, at)
	event.Mention = true
	return event
}

func command(number int, commentID int64, actor model.Actor, cmd model.Command, at time.Time) model.Event {
	event := mention(number, false, commentID, actor, string(cmd), at)
	event.Command = cmd
	return event
}

func edited(base model.Event, at time.Time, body string) model.Event {
	event := base
	event.ID = model.CommentID(base.CommentID, at)
	event.Kind = model.EventCommentEdited
	event.Body, event.At = body, at
	return event
}

func issueOpened(number int, actor model.Actor, body string, mentioned bool, at time.Time) model.Event {
	return model.Event{ID: model.IssueOpenedID(number), Kind: model.EventIssueOpened, Thread: model.ThreadRef{Number: number}, Actor: actor, At: at, Title: fmt.Sprintf("Issue %d", number), Body: body, Mention: mentioned}
}

func prClosed(number int, merged bool, at time.Time) model.Event {
	if merged {
		return model.Event{ID: model.PRMergedID(number, "merge"+fmt.Sprint(number)), Kind: model.EventPRMerged, Thread: model.ThreadRef{Number: number, IsPR: true}, At: at, CommitSHA: "merge" + fmt.Sprint(number)}
	}
	return model.Event{ID: model.PRClosedID(number, at), Kind: model.EventPRClosed, Thread: model.ThreadRef{Number: number, IsPR: true}, At: at}
}

func prCI(number int, sha string, failing []string, at time.Time) model.Event {
	fingerprint := model.CIFingerprint(append([]string{"head=" + sha}, failing...))
	return model.Event{ID: model.PRCIID(number, sha, fingerprint), Kind: model.EventPRCI, Thread: model.ThreadRef{Number: number, IsPR: true}, Actor: ci, At: at, CommitSHA: sha, CI: &model.CISnapshot{SHA: sha, Fingerprint: fingerprint, Failing: failing, Passing: 3}}
}

// openPR enqueues, attempts and confirms an open_pr from the Ticket's
// worker, returning the bound PR number.
func (h *harness) openPR(id model.TicketID, call string, number int) {
	h.t.Helper()
	intent, err := h.store.Enqueue(h.ctx, model.WorkerCall{SessionID: sessionOf(id), ToolCallID: call, At: h.clock.Now()}, model.EffectRequest{Kind: model.EffectOpenPR, Payload: model.EffectPayload{Title: "Fix", Body: "body"}})
	if err != nil {
		h.t.Fatalf("enqueue open_pr: %v", err)
	}
	if _, err := h.store.Attempt(h.ctx, intent.ID, h.clock.Now()); err != nil {
		h.t.Fatalf("attempt: %v", err)
	}
	if _, err := h.store.Settle(h.ctx, intent.ID, model.EffectSettlement{State: model.EffectConfirmed, Result: &model.EffectReply{RemoteID: int64(number)}, SettledAt: h.clock.Now()}); err != nil {
		h.t.Fatalf("settle: %v", err)
	}
}

// errCrash is what the crash store returns at its chosen commit.
var errCrash = errors.New("injected crash")

// crashStore fails its failAt-th commit, either before anything is written
// or after the inner store made the commit durable (a lost acknowledgment).
type crashStore struct {
	durable.Store
	failAt  int
	durable bool
	commits int
}

func (c *crashStore) Commit(ctx context.Context, commit durable.Commit) (durable.Receipt, error) {
	c.commits++
	if c.commits != c.failAt {
		return c.Store.Commit(ctx, commit)
	}
	if c.durable {
		if _, err := c.Store.Commit(ctx, commit); err != nil {
			return durable.Receipt{}, err
		}
	}
	return durable.Receipt{}, errCrash
}

// journal dumps every current record as namespace/id -> its JSON value.
// Values are json.Marshal output of the same structs, so equal state has
// equal bytes.
func journal(t *testing.T, store durable.Store) map[string]string {
	t.Helper()
	out := make(map[string]string)
	for _, kind := range []durable.Kind{durable.DocumentKind, durable.EntryKind} {
		cursor := ""
		for {
			page, err := store.Read(context.Background(), durable.Query{SessionID: model.ControlSession, Kind: kind, Cursor: cursor, Limit: 1000})
			if err != nil {
				t.Fatalf("read journal: %v", err)
			}
			for _, record := range page.Records {
				out[record.Namespace+"/"+record.ID] = string(record.Value)
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
	}
	return out
}

func diffJournals(want, got map[string]string) string {
	var keys []string
	for key := range want {
		keys = append(keys, key)
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var diff []string
	for _, key := range keys {
		if want[key] != got[key] {
			diff = append(diff, fmt.Sprintf("%s:\n  want %s\n  got  %s", key, want[key], got[key]))
		}
	}
	if len(diff) == 0 {
		return ""
	}
	raw, _ := json.MarshalIndent(diff, "", " ")
	return string(raw)
}
