// Package source is the GitHub side of the ticket runner: it polls the
// configured repository through a model.GHClient and hands normalized,
// deterministic events to control through model.Intake.
//
// The source owns no durable state. Every read position lives in the
// control journal and moves only in the Ingest commit that stores what was
// read up to it, so a crash or a failed page rereads instead of skipping.
// Notifications never supply a lower bound for reading a thread: they only
// flag the thread, which is then read from its own committed positions
// (from its creation when it was never read).
package source

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// Defaults for zero configuration values, as documented on model.Config.
const (
	defaultInterval   = 60 * time.Second
	defaultLookback   = time.Hour
	defaultMaxPages   = 10
	defaultMaxBody    = 64 << 10
	listPageSize      = 100
	notificationsPage = 50
	// unhealthyAfter consecutive failed rounds turn health unhealthy.
	unhealthyAfter = 3
	// lowBudgetDivisor: below Limit/lowBudgetDivisor remaining requests a
	// round reads only flagged threads.
	lowBudgetDivisor = 10
	// rereadPageFactor bounds pages that only reread already-committed
	// items (the lookback overlap) to this multiple of MaxPages, so the
	// overlap cannot starve progress yet a round stays bounded.
	rereadPageFactor = 4
	// rateLimitRetry is the wait after a 429 that names no Retry-After.
	rateLimitRetry = time.Minute
)

// Options configures a Source.
type Options struct {
	Client model.GHClient
	Intake model.Intake
	Clock  model.Clock
	// Config supplies poll, observe and limit settings; zero values take
	// the documented defaults.
	Config model.Config
	// Repo names the repository ("owner/repo" in Repo.Repo) and its default
	// branch for default-branch CI.
	Repo model.RepoMeta
	// Account is the dedicated user: its own activity is never stored and
	// mentions of its login address the runner.
	Account model.Actor
}

// Source implements model.Source.
type Source struct {
	client        model.GHClient
	intake        model.Intake
	clock         model.Clock
	repo          string
	defaultBranch string
	account       model.Actor
	interval      time.Duration
	lookback      time.Duration
	maxPages      int
	observe       model.ObserveConfig
	text          text

	// polling serializes rounds: the poll loop and poll_now share a Source.
	polling sync.Mutex
	// mu guards health and blockedUntil.
	mu           sync.Mutex
	health       model.SourceHealth
	blockedUntil time.Time
}

var _ model.Source = (*Source)(nil)

// New validates options and returns a Source.
func New(options Options) (*Source, error) {
	switch {
	case options.Client == nil:
		return nil, errors.New("source: no GitHub client")
	case options.Intake == nil:
		return nil, errors.New("source: no intake")
	case options.Clock == nil:
		return nil, errors.New("source: no clock")
	case options.Repo.Repo == "":
		return nil, errors.New("source: no repository")
	case options.Account.Login == "":
		return nil, errors.New("source: no account login")
	}
	poll := options.Config.Poll
	maxBody := cmp.Or(options.Config.Limits.MaxEventBodyBytes, defaultMaxBody)
	return &Source{
		client:        options.Client,
		intake:        options.Intake,
		clock:         options.Clock,
		repo:          options.Repo.Repo,
		defaultBranch: options.Repo.DefaultBranch,
		account:       options.Account,
		interval:      cmp.Or(time.Duration(poll.Interval), defaultInterval),
		lookback:      cmp.Or(time.Duration(poll.Lookback), defaultLookback),
		maxPages:      cmp.Or(poll.MaxPages, defaultMaxPages),
		observe:       options.Config.Observe,
		text:          newText(options.Account.Login, maxBody),
		health:        model.SourceHealth{Healthy: true},
	}, nil
}

// Health returns the in-memory view of recent rounds.
func (s *Source) Health() model.SourceHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	health := s.health
	health.Errors = append([]model.ResourceError(nil), s.health.Errors...)
	return health
}

// round is the state of one Poll.
type round struct {
	started time.Time
	report  model.PollReport
	// rate is the newest budget any reply reported.
	rate model.RateLimit
	// limitedUntil is set when a rate limit stopped the round.
	limitedUntil time.Time
	// pollInterval is GitHub's X-Poll-Interval for notifications.
	pollInterval time.Duration
}

func (rd *round) stopped() bool { return !rd.limitedUntil.IsZero() }

// lowBudget reports a remaining budget under a tenth of the limit.
func (rd *round) lowBudget() bool {
	return rd.rate.Limit > 0 && rd.rate.Remaining*lowBudgetDivisor < rd.rate.Limit
}

// errRoundStopped ends the remaining reads of a rate-limited round.
var errRoundStopped = errors.New("round stopped by a rate limit")

// Poll runs one bounded round: notifications, enabled observe streams,
// then watched threads with their PRs. Each resource fails on its own and
// a failure commits nothing past what was read. The error is non-nil only
// when ctx ended.
func (s *Source) Poll(ctx context.Context) (model.PollReport, error) {
	s.polling.Lock()
	defer s.polling.Unlock()
	rd := &round{started: s.clock.Now()}
	rd.report.StartedAt = rd.started
	s.mu.Lock()
	blocked := s.blockedUntil
	rd.rate = s.health.RateLimit
	s.mu.Unlock()
	if rd.started.Before(blocked) {
		// Still inside a rate-limit wait: no request is sent.
		rd.report.FinishedAt = rd.started
		rd.report.NextPollAt = blocked
		return rd.report, nil
	}

	steps := []struct {
		resource string
		run      func(context.Context, *round) error
		always   bool
	}{
		{"notifications", s.pollNotifications, true},
		{string(model.CursorIssues), s.pollIssuesStream, false},
		{string(model.CursorComments), s.pollCommentsStream, false},
		{string(model.CursorReviewComments), s.pollReviewCommentsStream, false},
		{string(model.CursorBranchCI), s.pollBranchCI, false},
		{"watched", s.pollWatched, true},
	}
	for _, step := range steps {
		if rd.stopped() {
			break
		}
		if !step.always && rd.lowBudget() {
			continue
		}
		if err := step.run(ctx, rd); err != nil {
			if ctx.Err() != nil {
				return s.finish(rd), ctx.Err()
			}
			s.fail(rd, step.resource, err)
		}
	}
	return s.finish(rd), nil
}

// finish computes the next poll time and folds the round into health.
func (s *Source) finish(rd *round) model.PollReport {
	rd.report.FinishedAt = s.clock.Now()
	next := rd.started.Add(max(s.interval, rd.pollInterval))
	if rd.limitedUntil.After(next) {
		next = rd.limitedUntil
	}
	rd.report.NextPollAt = next
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blockedUntil = rd.limitedUntil
	health := &s.health
	health.LastRoundAt = rd.report.FinishedAt
	health.NextPollAt = next
	health.RateLimit = rd.rate
	health.Errors = append([]model.ResourceError(nil), rd.report.Errors...)
	if len(rd.report.Errors) == 0 {
		health.ConsecutiveFailures = 0
		health.LastSuccessAt = rd.report.FinishedAt
	} else {
		health.ConsecutiveFailures++
	}
	health.Healthy = health.ConsecutiveFailures < unhealthyAfter
	return rd.report
}

// fail records a resource error. A stopped round's skipped reads are not
// separate failures: the rate-limit error that stopped it already counts.
func (s *Source) fail(rd *round, resource string, err error) {
	if errors.Is(err, errRoundStopped) {
		return
	}
	failure := model.ResourceError{Resource: resource, Message: err.Error(), At: s.clock.Now()}
	var ghErr *model.GHError
	if errors.As(err, &ghErr) {
		failure.Status = ghErr.Status
	}
	rd.report.Errors = append(rd.report.Errors, failure)
}

// get sends one request, tracking the rate budget and stopping the round
// on a rate limit.
func (s *Source) get(ctx context.Context, rd *round, request model.GHRequest) (model.GHResponse, error) {
	if rd.stopped() {
		return model.GHResponse{}, errRoundStopped
	}
	request.Method = http.MethodGet
	response, err := s.client.Do(ctx, request)
	if response.RateLimit.Limit > 0 {
		rd.rate = response.RateLimit
	}
	var ghErr *model.GHError
	if errors.As(err, &ghErr) && (ghErr.Status == http.StatusTooManyRequests || (ghErr.Status == http.StatusForbidden && ghErr.RetryAfter > 0)) {
		wait := ghErr.RetryAfter
		if wait <= 0 {
			wait = rateLimitRetry
		}
		rd.limitedUntil = s.clock.Now().Add(wait)
	}
	if err != nil {
		return response, fmt.Errorf("GET %s: %w", request.Path, err)
	}
	return response, nil
}

// pageVisitor handles one page of a walk. final is true on the last page.
// fresh reports whether the page held an item past the committed position;
// pages that only reread committed items do not use up MaxPages.
type pageVisitor func(response model.GHResponse, final bool) (fresh bool, err error)

// walk reads pages from first by Link rel="next" until the listing ends
// (complete) or MaxPages fresh pages were read.
func (s *Source) walk(ctx context.Context, rd *round, first model.GHRequest, visit pageVisitor) (bool, error) {
	request := first
	fresh, total := 0, 0
	for {
		response, err := s.get(ctx, rd, request)
		if err != nil {
			return false, err
		}
		final := response.NextPath == ""
		pageFresh, err := visit(response, final)
		if err != nil {
			return false, err
		}
		if final {
			return true, nil
		}
		total++
		if pageFresh {
			fresh++
		}
		if fresh >= s.maxPages || total >= s.maxPages*rereadPageFactor {
			return false, nil
		}
		request = model.GHRequest{Path: response.NextPath}
	}
}

// ingest commits one batch.
func (s *Source) ingest(ctx context.Context, rd *round, batch model.IntakeBatch) error {
	if batch.ReadAt.IsZero() {
		batch.ReadAt = s.clock.Now()
	}
	result, err := s.intake.Ingest(ctx, batch)
	if err != nil {
		return fmt.Errorf("ingest %s: %w", batch.Resource, err)
	}
	rd.report.Batches++
	rd.report.NewEvents += result.NewEvents
	return nil
}

// decode reads a reply body; a 304 yields the zero value.
func decode[T any](response model.GHResponse, what string) (T, error) {
	if response.Status == http.StatusNotModified {
		var zero T
		return zero, nil
	}
	return decodeBytes[T](response.Body, what)
}

func decodeBytes[T any](raw []byte, what string) (T, error) {
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, fmt.Errorf("decode %s: %w", what, err)
	}
	return value, nil
}

// sameCursor compares cursors by instant, not by time representation.
func sameCursor(a, b model.Cursor) bool {
	return a.Key == b.Key && a.Since.Equal(b.Since) && a.LastModified == b.LastModified && a.ETag == b.ETag &&
		a.EnabledAt.Equal(b.EnabledAt) && a.BranchSHA == b.BranchSHA && a.Fingerprint == b.Fingerprint
}

// listQuery is the query of an oldest-first listing from since.
func listQuery(since time.Time, sorted bool) url.Values {
	query := url.Values{"per_page": {strconv.Itoa(listPageSize)}}
	if sorted {
		query.Set("sort", "updated")
		query.Set("direction", "asc")
	}
	if !since.IsZero() {
		query.Set("since", since.UTC().Format(time.RFC3339))
	}
	return query
}

// rereadFrom is where a stream positioned at since is reread from: the
// lookback before it, or nothing (from the start) when never read.
func (s *Source) rereadFrom(since time.Time) time.Time {
	if since.IsZero() {
		return time.Time{}
	}
	return since.Add(-s.lookback)
}

func (s *Source) repoPath(format string, args ...any) string {
	return "repos/" + s.repo + "/" + fmt.Sprintf(format, args...)
}
