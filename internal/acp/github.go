package acp

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"

	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/internal/rpc"
)

// GitHub is the operator surface of a running GitHub ticket runner
// (serve -github). serve passes it to every ACP connection with WithGitHub.
type GitHub interface {
	// Control is the control journal's operator view.
	Control() model.Control
	// Repo is the repository, owner/repo.
	Repo() string
	// Health is the poll loop's in-memory health.
	Health() model.SourceHealth
	// PollNow runs one poll round, serialized with the poll loop.
	PollNow(ctx context.Context) (model.PollReport, error)
	// Subscribe calls notify after every durable control commit, in commit
	// order, with the Tickets it changed. notify must not block or call
	// back; cancel stops it.
	Subscribe(notify func(seq uint64, tickets []model.TicketID)) (cancel func())
}

// WithGitHub serves the _tempest/github/* methods over github on this
// connection and advertises them in initialize. Without it (or with nil)
// the methods are unknown.
func (c *Client) WithGitHub(github GitHub) *Client {
	c.github = github
	return c
}

// GitHub methods and notifications.
const (
	githubPrefix       = "_tempest/github/"
	githubUpdate       = "_tempest/github/update"
	githubGap          = "_tempest/github/gap"
	maxGitHubPage      = 100
	defaultGitHubWatch = 256
	maxGitHubWatch     = 65536
)

var githubMethods = []string{
	"_tempest/github/status", "_tempest/github/tickets", "_tempest/github/ticket", "_tempest/github/events",
	"_tempest/github/stop", "_tempest/github/retry", "_tempest/github/pause", "_tempest/github/unpause",
	"_tempest/github/grant", "_tempest/github/resolve_effect", "_tempest/github/poll_now",
	"_tempest/github/watch", "_tempest/github/unwatch",
}

// githubCapability is agentCapabilities._meta.tempest.github.
func githubCapability(github GitHub) map[string]any {
	return map[string]any{"version": 1, "repo": github.Repo(), "methods": githubMethods, "notifications": []string{githubUpdate, githubGap}}
}

// githubStatus is the status result: the control journal's summary and the
// poll loop's health.
type githubStatus struct {
	model.StatusView
	Health model.SourceHealth `json:"health"`
}

// githubSnapshot is the watch result. Updates with a seq above Seq follow.
type githubSnapshot struct {
	Status    githubStatus     `json:"status"`
	Attention model.TicketPage `json:"attention"`
	Seq       uint64           `json:"seq"`
}

// githubFailure maps the runner's errors to their wire codes.
func githubFailure(err error) *rpc.Error {
	switch {
	case errors.Is(err, model.ErrNotFound):
		return rpc.NewError(-32030, "github_not_found", err.Error())
	case errors.Is(err, model.ErrStopped):
		return rpc.NewError(-32031, "github_stopped", err.Error())
	case errors.Is(err, model.ErrHeld):
		return rpc.NewError(-32032, "github_held", err.Error())
	case errors.Is(err, model.ErrPaused):
		return rpc.NewError(-32033, "github_paused", err.Error())
	case errors.Is(err, model.ErrBudget):
		return rpc.NewError(-32034, "github_budget", err.Error())
	case errors.Is(err, model.ErrEnded):
		return rpc.NewError(-32035, "github_ended", err.Error())
	}
	return failure(err)
}

func invalid(message string) *rpc.Error { return rpc.NewError(-32602, "invalid_params", message) }

// githubParams is the union of the github methods' parameters.
type githubParams struct {
	RequestID string             `json:"requestId"`
	TicketID  model.TicketID     `json:"ticketId"`
	IntentID  model.OutboxID     `json:"intentId"`
	Reason    string             `json:"reason"`
	Filter    model.TicketFilter `json:"filter"`
	Cursor    string             `json:"cursor"`
	Limit     int                `json:"limit"`
	Thread    int                `json:"thread"`
	State     string             `json:"state"`
	USD       *float64           `json:"usd"`
	Tokens    int64              `json:"tokens"`
	Buffer    int                `json:"buffer"`
}

// operator builds an audited operator request; requestId is the client's
// idempotency key.
func (p githubParams) operator(target string) (model.OperatorRequest, *rpc.Error) {
	if p.RequestID == "" {
		return model.OperatorRequest{}, invalid("requestId is required")
	}
	return model.OperatorRequest{ID: p.RequestID, Target: target, Reason: p.Reason}, nil
}

// handleGitHub serves _tempest/github/*.
func (c *Client) handleGitHub(ctx context.Context, method string, raw json.RawMessage) (any, *rpc.Error) {
	if c.github == nil {
		return nil, rpc.NewError(-32601, "method_not_found", "unknown method: "+method+" (serve runs without -github)")
	}
	var p githubParams
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	if p.Limit < 0 || p.Limit > maxGitHubPage {
		return nil, invalid("limit must be between 0 and 100")
	}
	control := c.github.Control()
	done := func(err error) (any, *rpc.Error) {
		if err != nil {
			return nil, githubFailure(err)
		}
		return struct{}{}, nil
	}
	switch strings.TrimPrefix(method, githubPrefix) {
	case "status":
		status, err := control.Status(ctx)
		if err != nil {
			return nil, githubFailure(err)
		}
		return githubStatus{StatusView: status, Health: c.github.Health()}, nil
	case "tickets":
		switch p.Filter {
		case "", model.FilterAttention, model.FilterOpen, model.FilterEnded, model.FilterAll:
		default:
			return nil, invalid("filter must be attention, open, ended or all")
		}
		page, err := control.Tickets(ctx, model.TicketQuery{Filter: p.Filter, Cursor: p.Cursor, Limit: p.Limit})
		if err != nil {
			return nil, githubFailure(err)
		}
		return page, nil
	case "ticket":
		if p.TicketID == "" {
			return nil, invalid("ticketId is required")
		}
		detail, err := control.Ticket(ctx, p.TicketID)
		if err != nil {
			return nil, githubFailure(err)
		}
		return detail, nil
	case "events":
		if p.Thread < 0 || p.Thread != 0 && p.TicketID != "" {
			return nil, invalid("give at most one of thread and ticketId")
		}
		page, err := control.Events(ctx, model.EventQuery{Thread: p.Thread, Ticket: p.TicketID, Cursor: p.Cursor, Limit: p.Limit})
		if err != nil {
			return nil, githubFailure(err)
		}
		return page, nil
	case "stop", "retry":
		if p.TicketID == "" {
			return nil, invalid("ticketId is required")
		}
		request, e := p.operator(string(p.TicketID))
		if e != nil {
			return nil, e
		}
		if method == githubPrefix+"stop" {
			return done(control.Stop(ctx, request))
		}
		return done(control.Retry(ctx, request))
	case "pause", "unpause":
		request, e := p.operator("")
		if e != nil {
			return nil, e
		}
		return done(control.SetPaused(ctx, request, method == githubPrefix+"pause"))
	case "grant":
		request, e := p.operator(string(p.TicketID))
		if e != nil {
			return nil, e
		}
		amount, e := grantAmount(p.USD, p.Tokens)
		if e != nil {
			return nil, e
		}
		return done(control.Grant(ctx, request, amount))
	case "resolve_effect":
		if p.IntentID == "" {
			return nil, invalid("intentId is required")
		}
		var state model.EffectState
		switch p.State {
		case "confirmed":
			state = model.EffectConfirmed
		case "failed":
			state = model.EffectFailed
		case "resend":
			state = model.EffectPending
		default:
			return nil, invalid(`state must be "confirmed", "failed" or "resend"`)
		}
		request, e := p.operator(string(p.IntentID))
		if e != nil {
			return nil, e
		}
		return done(control.ResolveEffect(ctx, request, state))
	case "poll_now":
		report, err := c.github.PollNow(ctx)
		if err != nil {
			return nil, githubFailure(err)
		}
		return report, nil
	case "watch":
		buffer := p.Buffer
		if buffer == 0 {
			buffer = defaultGitHubWatch
		}
		if buffer < 0 || buffer > maxGitHubWatch {
			return nil, invalid("buffer must be between 1 and 65536")
		}
		return c.watchGitHub(ctx, buffer)
	case "unwatch":
		c.watchMu.Lock()
		defer c.watchMu.Unlock()
		c.stopGitHubWatch()
		return struct{}{}, nil
	}
	return nil, rpc.NewError(-32601, "method_not_found", "unknown method: "+method)
}

// grantAmount converts a grant's dollars to micro-USD. At least one of usd
// and tokens must add something; neither may be negative.
func grantAmount(usd *float64, tokens int64) (model.Spend, *rpc.Error) {
	var amount model.Spend
	if usd != nil {
		dollars := *usd
		if math.IsNaN(dollars) || math.IsInf(dollars, 0) || dollars < 0 || dollars > 1e9 {
			return model.Spend{}, invalid("usd must be a finite amount between 0 and 1e9")
		}
		amount.MicroUSD = int64(math.Round(dollars * 1e6))
	}
	if tokens < 0 {
		return model.Spend{}, invalid("tokens must not be negative")
	}
	amount.TotalTokens = tokens
	if amount.MicroUSD == 0 && amount.TotalTokens == 0 {
		return model.Spend{}, invalid("a grant must add usd or tokens")
	}
	return amount, nil
}

// githubWatcher forwards control commits to one connection. A watcher that
// falls buffer commits behind gets a gap and is dropped; the client watches
// again for a fresh snapshot.
type githubWatcher struct {
	updates chan githubChange
	// gap is closed when an update did not fit the buffer.
	gap chan struct{}

	mu       sync.Mutex
	overflow bool
	stopped  bool

	stop func()
}

type githubChange struct {
	seq     uint64
	tickets []model.TicketID
}

// notify runs under the control store's mutex: it never blocks.
func (w *githubWatcher) notify(seq uint64, tickets []model.TicketID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.overflow || w.stopped {
		return
	}
	select {
	case w.updates <- githubChange{seq: seq, tickets: tickets}:
	default:
		w.overflow = true
		close(w.gap)
	}
}

// watchGitHub replaces this connection's github watch. It subscribes
// before reading the snapshot, so no commit after the snapshot is missed;
// commits at or below the snapshot's seq are skipped.
func (c *Client) watchGitHub(ctx context.Context, buffer int) (any, *rpc.Error) {
	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	c.stopGitHubWatch()
	w := &githubWatcher{updates: make(chan githubChange, buffer), gap: make(chan struct{})}
	watchCtx, cancel := context.WithCancel(ctx)
	unsubscribe := c.github.Subscribe(w.notify)
	var once sync.Once
	w.stop = func() {
		once.Do(func() {
			unsubscribe()
			cancel()
			w.mu.Lock()
			w.stopped = true
			w.mu.Unlock()
		})
	}
	control := c.github.Control()
	status, err := control.Status(ctx)
	if err != nil {
		w.stop()
		return nil, githubFailure(err)
	}
	attention, err := control.Tickets(ctx, model.TicketQuery{Filter: model.FilterAttention, Limit: maxGitHubPage})
	if err != nil {
		w.stop()
		return nil, githubFailure(err)
	}
	snapshot := githubSnapshot{Status: githubStatus{StatusView: status, Health: c.github.Health()}, Attention: attention, Seq: status.Seq}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		w.stop()
		return nil, rpc.NewError(-32603, "internal_error", "connection closed")
	}
	c.githubWatch = w
	c.mu.Unlock()
	after := func() { go c.forwardGitHub(watchCtx, w, snapshot.Seq) }
	return rpc.Response{Value: snapshot, After: after}, nil
}

// forwardGitHub sends the watcher's updates until it stops, the connection
// fails or it falls behind, which sends a gap naming the last seq sent.
func (c *Client) forwardGitHub(ctx context.Context, w *githubWatcher, last uint64) {
	defer w.stop()
	defer func() {
		c.mu.Lock()
		if c.githubWatch == w {
			c.githubWatch = nil
		}
		c.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.gap:
			_ = c.conn.Notify(ctx, githubGap, map[string]any{"lastSeq": last})
			return
		case change := <-w.updates:
			if change.seq <= last {
				continue
			}
			tickets := change.tickets
			if tickets == nil {
				tickets = []model.TicketID{}
			}
			if err := c.conn.Notify(ctx, githubUpdate, map[string]any{"seq": change.seq, "tickets": tickets, "health": c.github.Health()}); err != nil {
				return
			}
			last = change.seq
		}
	}
}

// stopGitHubWatch ends this connection's github watch. Callers hold
// watchMu.
func (c *Client) stopGitHubWatch() {
	c.mu.Lock()
	w := c.githubWatch
	c.githubWatch = nil
	c.mu.Unlock()
	if w != nil {
		w.stop()
	}
}
