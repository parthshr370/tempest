package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/githubsource/control"
	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest/durable"
)

var (
	githubEpoch   = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	githubAccount = model.Actor{Login: "tempest-bot", ID: 100, Type: "User"}
	githubParth   = model.Actor{Login: "parth", ID: 1111, Type: "User"}
)

type githubClock struct{ now time.Time }

func (c githubClock) Now() time.Time { return c.now }

// fakeGitHub is a runner over a real control journal with a stub poll loop.
type fakeGitHub struct {
	store *control.Store
	mu    sync.Mutex
	polls int
}

func (f *fakeGitHub) Control() model.Control { return f.store.Operator() }
func (f *fakeGitHub) Repo() string           { return "acme/widgets" }
func (f *fakeGitHub) Health() model.SourceHealth {
	return model.SourceHealth{Healthy: true, LastRoundAt: githubEpoch}
}

func (f *fakeGitHub) PollNow(context.Context) (model.PollReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	return model.PollReport{StartedAt: githubEpoch, NewEvents: f.polls}, nil
}

func (f *fakeGitHub) Subscribe(notify func(uint64, []model.TicketID)) func() {
	return f.store.Subscribe(notify)
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	ctx := context.Background()
	log, err := durable.OpenFile(ctx, filepath.Join(t.TempDir(), "control.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	config := model.Config{
		Version:    1,
		Repository: "acme/widgets",
		Account:    model.AccountConfig{Login: githubAccount.Login, ID: githubAccount.ID},
		Admission:  model.AdmissionConfig{Mode: model.ModeObserve, Actors: []model.ActorID{{Login: githubParth.Login, ID: githubParth.ID}}},
		Workers:    model.WorkersConfig{Max: 2},
		Budgets: model.BudgetsConfig{
			Run:       model.RunBudget{MaxTurns: 10, MaxCostUSD: 1, MaxWallTime: model.Duration(time.Hour)},
			TicketUSD: 10, RepoUSD: 100, RepoPeriod: model.Duration(720 * time.Hour), MinRunUSD: 0.1, MaxAutoWakesPerDay: 5,
		},
		Merge: "manual",
	}
	repo := model.RepoMeta{Repo: "acme/widgets", RepoID: 42, DefaultBranch: "main", Account: githubAccount, CreatedAt: githubEpoch}
	store, err := control.Open(ctx, control.Options{Store: log, Config: config, Repo: repo, Clock: githubClock{githubEpoch}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close(ctx) })
	return &fakeGitHub{store: store}
}

// request creates a Ticket on issue number from an authorized mention.
func (f *fakeGitHub) request(t *testing.T, number int) model.TicketID {
	t.Helper()
	event := model.Event{ID: model.CommentID(int64(number), githubEpoch), Kind: model.EventComment, Thread: model.ThreadRef{Number: number}, Actor: githubParth, At: githubEpoch, ObservedAt: githubEpoch, Body: "@tempest-bot fix", Mention: true, CommentID: int64(number)}
	result, err := f.store.Ingest(context.Background(), model.IntakeBatch{Resource: fmt.Sprintf("thread:%d", number), Events: []model.Event{event}, ReadAt: githubEpoch})
	if err != nil || len(result.Decisions) != 1 || len(result.Decisions[0].Tickets) != 1 {
		t.Fatalf("ingest: %+v %v", result, err)
	}
	return result.Decisions[0].Tickets[0]
}

// unknownComment binds the Ticket's worker and leaves one comment unknown.
func (f *fakeGitHub) unknownComment(t *testing.T, id model.TicketID) model.OutboxID {
	t.Helper()
	ctx := context.Background()
	admitted, err := f.store.AdmitNext(ctx, githubEpoch)
	if err != nil || len(admitted) != 1 {
		t.Fatalf("admit: %+v %v", admitted, err)
	}
	session := "sess-" + string(id)
	steps := []func() error{
		func() error { return f.store.Preparing(ctx, admitted[0].Dispatch.ID, githubEpoch) },
		func() error { return f.store.Prepared(ctx, id, "/wt", "base") },
		func() error {
			return f.store.Bind(ctx, model.SessionBinding{SessionID: session, Ticket: id, BoundAt: githubEpoch})
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	intent, err := f.store.Enqueue(ctx, model.WorkerCall{SessionID: session, ToolCallID: "c1"}, model.EffectRequest{Kind: model.EffectComment, Payload: model.EffectPayload{Body: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Attempt(ctx, intent.ID, githubEpoch); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Settle(ctx, intent.ID, model.EffectSettlement{State: model.EffectUnknown, Error: "not found"}); err != nil {
		t.Fatal(err)
	}
	return intent.ID
}

// peer is the ACP client end of a connection served by a Client.
type peer struct {
	conn    *rpc.Conn
	notices chan notification
}

type notification struct {
	method string
	params json.RawMessage
}

func connect(t *testing.T, github GitHub) *peer {
	t.Helper()
	a, b := net.Pipe()
	server := rpc.NewStream(a)
	client := NewClient(server, nil, nil)
	if github != nil {
		client = client.WithGitHub(github)
	}
	go client.Serve()
	p := &peer{conn: rpc.NewStream(b), notices: make(chan notification, 1024)}
	go p.conn.ServeOrdered(func(_ context.Context, method string, params json.RawMessage) (any, *rpc.Error) {
		p.notices <- notification{method, append(json.RawMessage(nil), params...)}
		return nil, nil
	})
	t.Cleanup(func() { p.conn.Close(); server.Close() })
	var init map[string]any
	p.call(t, "initialize", map[string]any{"protocolVersion": 1}, &init)
	return p
}

func (p *peer) call(t *testing.T, method string, params, result any) {
	t.Helper()
	if err := p.conn.Call(t.Context(), method, params, result); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

// fail calls a method that must fail with code.
func (p *peer) fail(t *testing.T, method string, params any, code int) {
	t.Helper()
	err := p.conn.Call(t.Context(), method, params, nil)
	var wire *rpc.Error
	if !errors.As(err, &wire) || wire.Code != code {
		t.Fatalf("%s %v: error %v, want code %d", method, params, err, code)
	}
}

func (p *peer) next(t *testing.T) notification {
	t.Helper()
	select {
	case n := <-p.notices:
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("no notification")
	}
	panic("unreachable")
}

func (p *peer) quiet(t *testing.T) {
	t.Helper()
	select {
	case n := <-p.notices:
		t.Fatalf("unexpected notification %s %s", n.method, n.params)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestGitHubMethodsNeedTheRunner(t *testing.T) {
	p := connect(t, nil)
	var init struct {
		AgentCapabilities struct {
			Meta struct {
				Tempest map[string]json.RawMessage `json:"tempest"`
			} `json:"_meta"`
		} `json:"agentCapabilities"`
	}
	p.call(t, "initialize", map[string]any{"protocolVersion": 1}, &init)
	if _, ok := init.AgentCapabilities.Meta.Tempest["github"]; ok {
		t.Fatal("github advertised without a runner")
	}
	p.fail(t, "_tempest/github/status", map[string]any{}, -32601)
}

func TestGitHubQueries(t *testing.T) {
	github := newFakeGitHub(t)
	p := connect(t, github)
	var init struct {
		AgentCapabilities struct {
			Meta struct {
				Tempest struct {
					GitHub struct {
						Version       int      `json:"version"`
						Repo          string   `json:"repo"`
						Methods       []string `json:"methods"`
						Notifications []string `json:"notifications"`
					} `json:"github"`
				} `json:"tempest"`
			} `json:"_meta"`
		} `json:"agentCapabilities"`
	}
	p.call(t, "initialize", map[string]any{"protocolVersion": 1}, &init)
	capability := init.AgentCapabilities.Meta.Tempest.GitHub
	if capability.Version != 1 || capability.Repo != "acme/widgets" || !slices.Contains(capability.Methods, "_tempest/github/watch") || !slices.Equal(capability.Notifications, []string{githubUpdate, githubGap}) {
		t.Fatalf("capability: %+v", capability)
	}
	id := github.request(t, 7)

	var status struct {
		Repo        string             `json:"repo"`
		OpenTickets int                `json:"openTickets"`
		Seq         uint64             `json:"seq"`
		Health      model.SourceHealth `json:"health"`
	}
	p.call(t, "_tempest/github/status", map[string]any{}, &status)
	if status.Repo != "acme/widgets" || status.OpenTickets != 1 || status.Seq == 0 || !status.Health.Healthy {
		t.Fatalf("status: %+v", status)
	}
	var page model.TicketPage
	p.call(t, "_tempest/github/tickets", map[string]any{"filter": "open", "limit": 10}, &page)
	if len(page.Tickets) != 1 || page.Tickets[0].ID != id {
		t.Fatalf("tickets: %+v", page)
	}
	var detail model.TicketDetail
	p.call(t, "_tempest/github/ticket", map[string]any{"ticketId": id}, &detail)
	if detail.Ticket.ID != id || detail.Mandate.Actor.ID != githubParth.ID {
		t.Fatalf("ticket: %+v", detail)
	}
	var events model.EventPage
	p.call(t, "_tempest/github/events", map[string]any{"ticketId": id}, &events)
	if len(events.Events) != 1 || events.Events[0].Decision == nil {
		t.Fatalf("events: %+v", events)
	}
	p.call(t, "_tempest/github/events", map[string]any{"thread": 7}, &events)
	if len(events.Events) != 1 {
		t.Fatalf("thread events: %+v", events)
	}
	var report model.PollReport
	p.call(t, "_tempest/github/poll_now", map[string]any{}, &report)
	if report.NewEvents != 1 {
		t.Fatalf("poll: %+v", report)
	}

	p.fail(t, "_tempest/github/tickets", map[string]any{"filter": "mine"}, -32602)
	p.fail(t, "_tempest/github/tickets", map[string]any{"limit": 101}, -32602)
	p.fail(t, "_tempest/github/ticket", map[string]any{}, -32602)
	p.fail(t, "_tempest/github/ticket", map[string]any{"ticketId": "i9-000000"}, -32030)
	p.fail(t, "_tempest/github/events", map[string]any{"thread": 7, "ticketId": id}, -32602)
	p.fail(t, "_tempest/github/nope", map[string]any{}, -32601)
}

func TestGitHubOperatorControls(t *testing.T) {
	github := newFakeGitHub(t)
	p := connect(t, github)
	id := github.request(t, 7)
	intent := github.unknownComment(t, id)
	var empty struct{}

	p.fail(t, "_tempest/github/stop", map[string]any{"ticketId": id}, -32602)
	p.fail(t, "_tempest/github/grant", map[string]any{"requestId": "g0"}, -32602)
	p.fail(t, "_tempest/github/grant", map[string]any{"requestId": "g0", "usd": -1}, -32602)
	p.fail(t, "_tempest/github/grant", map[string]any{"requestId": "g0", "ticketId": "i9-000000", "usd": 1}, -32030)
	p.call(t, "_tempest/github/grant", map[string]any{"requestId": "g1", "ticketId": id, "usd": 1.5, "tokens": 1000}, &empty)
	if ledger := github.ticket(t, id).Ledger; ledger.Granted.MicroUSD != 1_500_000 || ledger.Granted.TotalTokens != 1000 {
		t.Fatalf("grant: %+v", ledger)
	}

	p.fail(t, "_tempest/github/resolve_effect", map[string]any{"requestId": "r0", "intentId": intent, "state": "pending"}, -32602)
	p.fail(t, "_tempest/github/resolve_effect", map[string]any{"requestId": "r0", "intentId": "missing", "state": "failed"}, -32030)
	p.call(t, "_tempest/github/resolve_effect", map[string]any{"requestId": "r1", "intentId": intent, "state": "resend", "reason": "not on GitHub"}, &empty)
	if outbox := github.ticket(t, id).Outbox; len(outbox) != 1 || outbox[0].State != model.EffectPending {
		t.Fatalf("resend: %+v", outbox)
	}
	// The same request ID is idempotent; resolving a pending intent is a
	// state conflict.
	p.call(t, "_tempest/github/resolve_effect", map[string]any{"requestId": "r1", "intentId": intent, "state": "resend"}, &empty)
	p.fail(t, "_tempest/github/resolve_effect", map[string]any{"requestId": "r2", "intentId": intent, "state": "confirmed"}, -32024)

	p.call(t, "_tempest/github/pause", map[string]any{"requestId": "p1", "reason": "deploy"}, &empty)
	var status model.StatusView
	p.call(t, "_tempest/github/status", map[string]any{}, &status)
	if !status.Pause.Paused || status.Pause.Reason != "deploy" {
		t.Fatalf("pause: %+v", status.Pause)
	}
	p.call(t, "_tempest/github/unpause", map[string]any{"requestId": "p2"}, &empty)

	p.call(t, "_tempest/github/stop", map[string]any{"requestId": "s1", "ticketId": id, "reason": "enough"}, &empty)
	p.call(t, "_tempest/github/stop", map[string]any{"requestId": "s1", "ticketId": id}, &empty)
	if ticket := github.ticket(t, id).Ticket; !ticket.Stopped {
		t.Fatalf("stop: %+v", ticket)
	}
	p.fail(t, "_tempest/github/retry", map[string]any{"requestId": "t1", "ticketId": id}, -32031)
	p.fail(t, "_tempest/github/stop", map[string]any{"requestId": "s2", "ticketId": "i9-000000"}, -32030)
}

func (f *fakeGitHub) ticket(t *testing.T, id model.TicketID) model.TicketDetail {
	t.Helper()
	detail, err := f.store.Operator().Ticket(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return detail
}

type githubUpdateParams struct {
	Seq     uint64             `json:"seq"`
	Tickets []model.TicketID   `json:"tickets"`
	Health  model.SourceHealth `json:"health"`
}

// §13 reconnectable operator state: a watch snapshot, then one update per
// commit after it; unwatch and disconnect stop updates; a new connection's
// watch is consistent with status.
func TestGitHubWatch(t *testing.T) {
	github := newFakeGitHub(t)
	first := github.request(t, 7)
	p := connect(t, github)
	var snapshot githubSnapshot
	p.call(t, "_tempest/github/watch", map[string]any{}, &snapshot)
	if snapshot.Seq == 0 || snapshot.Seq != snapshot.Status.Seq || snapshot.Status.OpenTickets != 1 || !snapshot.Status.Health.Healthy {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	second := github.request(t, 8)
	n := p.next(t)
	var update githubUpdateParams
	if err := json.Unmarshal(n.params, &update); err != nil || n.method != githubUpdate || update.Seq <= snapshot.Seq || !slices.Equal(update.Tickets, []model.TicketID{second}) || !update.Health.Healthy {
		t.Fatalf("update: %s %s", n.method, n.params)
	}
	if err := github.store.Operator().SetPaused(context.Background(), model.OperatorRequest{ID: "p1"}, true); err != nil {
		t.Fatal(err)
	}
	n = p.next(t)
	var pause githubUpdateParams
	if err := json.Unmarshal(n.params, &pause); err != nil || pause.Seq <= update.Seq || pause.Tickets == nil || len(pause.Tickets) != 0 {
		t.Fatalf("pause update: %s", n.params)
	}
	var empty struct{}
	p.call(t, "_tempest/github/unwatch", map[string]any{}, &empty)
	github.request(t, 9)
	p.quiet(t)

	// Reconnect: a fresh snapshot matches status and includes everything.
	p.conn.Close()
	again := connect(t, github)
	again.call(t, "_tempest/github/watch", map[string]any{}, &snapshot)
	var status model.StatusView
	again.call(t, "_tempest/github/status", map[string]any{}, &status)
	if snapshot.Seq != status.Seq || snapshot.Status.OpenTickets != 3 {
		t.Fatalf("re-watch snapshot %+v, status %+v", snapshot, status)
	}
	if _, err := github.store.Operator().Ticket(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	// A second watch on the same connection replaces the first: one
	// update per commit, not two.
	again.call(t, "_tempest/github/watch", map[string]any{}, &snapshot)
	if err := github.store.Operator().SetPaused(context.Background(), model.OperatorRequest{ID: "p2"}, false); err != nil {
		t.Fatal(err)
	}
	if n := again.next(t); n.method != githubUpdate {
		t.Fatalf("got %s", n.method)
	}
	again.quiet(t)
}

// A watcher that falls behind gets one gap naming the last seq it was sent
// and nothing after it; the commits themselves never wait for it.
func TestGitHubWatchGap(t *testing.T) {
	github := newFakeGitHub(t)
	a, b := net.Pipe()
	server := rpc.NewStream(a)
	defer server.Close()
	defer b.Close()
	client := NewClient(server, nil, nil).WithGitHub(github)
	if _, e := client.Handle(t.Context(), "initialize", json.RawMessage(`{"protocolVersion":1}`)); e != nil {
		t.Fatal(e)
	}
	value, e := client.Handle(t.Context(), "_tempest/github/watch", json.RawMessage(`{"buffer":1}`))
	if e != nil {
		t.Fatal(e)
	}
	response := value.(rpc.Response)
	snapshot := response.Value.(githubSnapshot)
	response.After()
	// Nobody reads the connection: its queue fills, then the watcher's.
	for i := range 64 {
		if err := github.store.Operator().SetPaused(context.Background(), model.OperatorRequest{ID: fmt.Sprintf("p%d", i)}, i%2 == 0); err != nil {
			t.Fatal(err)
		}
	}
	reader := bufio.NewReader(b)
	last := snapshot.Seq
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var frame struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Method == githubUpdate {
			var update githubUpdateParams
			json.Unmarshal(frame.Params, &update)
			if update.Seq != last+1 {
				t.Fatalf("update seq %d after %d", update.Seq, last)
			}
			last = update.Seq
			continue
		}
		var gap struct {
			LastSeq uint64 `json:"lastSeq"`
		}
		if err := json.Unmarshal(frame.Params, &gap); err != nil || frame.Method != githubGap || gap.LastSeq != last {
			t.Fatalf("frame %s %s after seq %d", frame.Method, frame.Params, last)
		}
		break
	}
	// The watch ended: later commits send nothing.
	if err := github.store.Operator().SetPaused(context.Background(), model.OperatorRequest{ID: "after"}, false); err != nil {
		t.Fatal(err)
	}
	b.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if line, err := reader.ReadBytes('\n'); err == nil {
		t.Fatalf("frame after the gap: %s", line)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.githubWatch != nil {
		t.Fatal("watch still registered after the gap")
	}
}
