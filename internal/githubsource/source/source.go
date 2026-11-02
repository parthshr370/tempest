// Package source is the GitHub side of the ticket runner: it polls the
// configured repository through a model.GHClient and hands normalized,
// deterministic events to control through model.Intake.
//
// The source owns no durable state. Every read position lives in the
// control journal and moves only in the Ingest commit that stores what was
// read up to it, so a crash or a failed page rereads instead of skipping.
// Notifications never supply a lower bound for reading a thread: they only
// flag the thread, which is then read from its own committed positions
// (from its creation when it was never read). Every position is on
// GitHub's clock, from reply Date headers and item timestamps; the local
// clock only schedules rounds, so local skew cannot exclude activity.
package source

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	// pollSharePercent is the part of the account's primary rate limit that
	// polling spends per rate window. The rest stays for the outbox, the
	// worker tools and anything else signed in as the account.
	pollSharePercent = 40
	// assumedRateLimit stands in for the primary limit until a reply
	// reports one: GitHub's authenticated REST limit per window.
	assumedRateLimit = 5000
	// rateWindow is the length of GitHub's primary rate-limit window.
	rateWindow = time.Hour
	// rereadPageFactor bounds the observe-stream pages a round rereads from
	// the cursor towards the frontier to this multiple of MaxPages; a
	// longer reread starts at the cursor again next round.
	rereadPageFactor = 4
	// rateLimitRetry is the wait after a 429 that names no Retry-After.
	rateLimitRetry = time.Minute
	// replyCacheRounds is how many rounds a reply kept for revalidation
	// survives unused.
	replyCacheRounds = 3
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
	// It guards the state below, how far this process got, which lives in
	// memory only, since losing it only costs a reread: each observe
	// stream's frontier, the round each watched thread was last read in, a
	// stalled notification second and the replies kept for revalidation.
	polling   sync.Mutex
	frontiers map[model.CursorKey]position
	rounds    int
	readRound map[int]int
	stall     notificationStall
	replies   map[string]storedReply
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
		frontiers:     map[model.CursorKey]position{},
		readRound:     map[int]int{},
		replies:       map[string]storedReply{},
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
	// window is what polling may still spend in the rate window when the
	// round starts and allowance the round's share of it; spent counts the
	// requests GitHub charged, every reply but a 304.
	window, allowance, spent int
	// limitedUntil is set when a rate limit stopped the round.
	limitedUntil time.Time
	// pollInterval is GitHub's X-Poll-Interval for notifications.
	pollInterval time.Duration
}

func (rd *round) stopped() bool { return !rd.limitedUntil.IsZero() }

// spare is what is left of the round's allowance.
func (rd *round) spare() int { return rd.allowance - rd.spent }

// windowLeft is what is left of the rate window's share.
func (rd *round) windowLeft() int { return max(0, rd.window-rd.spent) }

// budget sets the round's allowance from the newest rate limit GitHub
// reported, the last round's at the start and then each reply's: polling's
// share of the window's limit less everything used in that window before
// the round (by this source or anything else on the account), spread
// evenly over the rounds left until the window resets. Notifications and
// the first flagged thread read even when it is spent, so flags always
// progress; the rest read only while it lasts, observe streams last. The
// local clock only schedules here: a skewed clock misjudges how many
// rounds remain, never what was read.
func (s *Source) budget(rd *round) {
	limit, reset := rd.rate.Limit, rd.rate.Reset
	// The newest reply already counts what the round spent.
	before := min(limit, rd.rate.Remaining+rd.spent)
	if limit <= 0 {
		limit, before = assumedRateLimit, assumedRateLimit
	}
	if !reset.After(rd.started) {
		// The window last reported has ended: a new one starts full.
		before, reset = limit, rd.started.Add(rateWindow)
	}
	rd.window = max(0, limit*pollSharePercent/100-(limit-before))
	rounds := max(1, int((reset.Sub(rd.started)+s.interval-1)/s.interval))
	rd.allowance = rd.window / rounds
}

// errRoundStopped ends the remaining reads of a rate-limited round.
var errRoundStopped = errors.New("round stopped by a rate limit")

// Poll runs one bounded round in priority order: notifications, watched
// threads with their PRs, default-branch CI, then the enabled observe
// streams. Each resource fails on its own and a failure commits nothing
// past what was read. The error is non-nil only when ctx ended.
func (s *Source) Poll(ctx context.Context) (model.PollReport, error) {
	s.polling.Lock()
	defer s.polling.Unlock()
	rd := s.newRound()
	s.mu.Lock()
	blocked := s.blockedUntil
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
		// budgeted steps start only while the round's allowance lasts;
		// the others spend it first and follow their own bounds.
		budgeted bool
	}{
		{"notifications", s.pollNotifications, false},
		{"watched", s.pollWatched, false},
		{string(model.CursorBranchCI), s.pollBranchCI, true},
		{string(model.CursorIssues), s.pollIssuesStream, true},
		{string(model.CursorComments), s.pollCommentsStream, true},
		{string(model.CursorReviewComments), s.pollReviewCommentsStream, true},
	}
	for _, step := range steps {
		if rd.stopped() {
			break
		}
		if step.budgeted && rd.spare() <= 0 {
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

// newRound starts a round with its request budget from the rate limit the
// last round saw.
func (s *Source) newRound() *round {
	rd := &round{started: s.clock.Now()}
	rd.report.StartedAt = rd.started
	s.mu.Lock()
	rd.rate = s.health.RateLimit
	s.mu.Unlock()
	s.rounds++
	s.forgetReplies()
	s.budget(rd)
	return rd
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

// get sends one request, charging the round's budget unless GitHub
// answers 304, which it does not count, and stopping the round on a rate
// limit.
//
// A first-page request without validators of its own is revalidated: the
// last reply to the same URL is kept with its ETag, and a 304 (free) means
// that reply is still current, so the caller gets it back with the 304's
// headers (GitHub's Date and the rate), as if GitHub had sent it again. An
// unchanged watched thread therefore costs no budget. Replies unused for
// replyCacheRounds rounds are dropped.
func (s *Source) get(ctx context.Context, rd *round, request model.GHRequest) (model.GHResponse, error) {
	if rd.stopped() {
		return model.GHResponse{}, errRoundStopped
	}
	request.Method = http.MethodGet
	key, stored, revalidated := s.validator(&request)
	response, err := s.client.Do(ctx, request)
	if response.Status != http.StatusNotModified {
		rd.spent++
	}
	if response.RateLimit.Limit > 0 {
		rd.rate = response.RateLimit
		s.budget(rd)
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
	switch {
	case key == "":
	case response.Status == http.StatusNotModified && revalidated:
		header := stored.Header.Clone()
		maps.Copy(header, response.Header)
		stored.Header, stored.RateLimit = header, response.RateLimit
		if response.PollInterval > 0 {
			stored.PollInterval = response.PollInterval
		}
		response = stored
		s.replies[key] = storedReply{response: response, round: s.rounds}
	case response.Status == http.StatusOK && response.ETag != "":
		s.replies[key] = storedReply{response: response, round: s.rounds}
	}
	return response, nil
}

// storedReply is a reply kept for revalidation, with the round that last
// used it.
type storedReply struct {
	response model.GHResponse
	round    int
}

// validator returns the cache key of a request get revalidates, empty for
// one that carries its own validators or asks past the first page, and
// sets the stored reply's ETag on it when there is one.
func (s *Source) validator(request *model.GHRequest) (string, model.GHResponse, bool) {
	path, query, _ := strings.Cut(request.Path, "?")
	inline, err := url.ParseQuery(query)
	if err != nil || request.ETag != "" || request.LastModified != "" || request.Query.Has("page") || inline.Has("page") {
		return "", model.GHResponse{}, false
	}
	maps.Copy(inline, request.Query)
	key := path + "?" + inline.Encode()
	stored, found := s.replies[key]
	if found {
		request.ETag = stored.response.ETag
	}
	return key, stored.response, found
}

// forgetReplies drops stored replies no round used lately.
func (s *Source) forgetReplies() {
	maps.DeleteFunc(s.replies, func(_ string, stored storedReply) bool { return s.rounds-stored.round >= replyCacheRounds })
}

// walk reads a snapshot listing (check runs, commit statuses) from first
// by Link rel="next" until it ends (complete) or MaxPages pages were read.
// Nothing positions on it: an incomplete walk is read again next round.
func (s *Source) walk(ctx context.Context, rd *round, first model.GHRequest, visit func(model.GHResponse) error) (bool, error) {
	request := first
	for range s.maxPages {
		response, err := s.get(ctx, rd, request)
		if err != nil {
			return false, err
		}
		if err := visit(response); err != nil {
			return false, err
		}
		if response.NextPath == "" {
			return true, nil
		}
		request = model.GHRequest{Path: response.NextPath}
	}
	return false, nil
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
	return a.Key == b.Key && a.Since.Equal(b.Since) && a.LastID == b.LastID && a.LastModified == b.LastModified && a.ETag == b.ETag &&
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

func (s *Source) repoPath(format string, args ...any) string {
	return "repos/" + s.repo + "/" + fmt.Sprintf(format, args...)
}
