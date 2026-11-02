package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// pollNotifications lists the account's notifications and flags matching
// Issue and PR threads for a full reread. The flags and the cursor commit
// together; the cursor is a listing position only and never bounds what a
// thread read returns. Nothing is marked read on GitHub.
func (s *Source) pollNotifications(ctx context.Context, rd *round) error {
	stored, found, err := s.intake.Cursor(ctx, model.CursorNotifications)
	if err != nil {
		return fmt.Errorf("read notifications cursor: %w", err)
	}
	base := stored.Since
	if base.IsZero() {
		// First start: notifications from one lookback before now count.
		base = rd.started.Add(-s.lookback)
	}
	query := url.Values{"all": {"true"}, "per_page": {strconv.Itoa(notificationsPage)}, "since": {base.Add(-s.lookback).UTC().Format(time.RFC3339)}}
	first := model.GHRequest{Path: "notifications", Query: query, LastModified: stored.LastModified}
	flags := map[int]model.ThreadUpdate{}
	newest := base
	var lastModified string
	notModified, pages := false, 0
	complete, err := s.walk(ctx, rd, first, func(response model.GHResponse, final bool) (bool, error) {
		pages++
		if pages == 1 {
			rd.pollInterval = response.PollInterval
			lastModified = response.LastModified
			notModified = response.Status == http.StatusNotModified
		}
		rows, err := decode[[]ghNotification](response, "notifications")
		if err != nil {
			return false, err
		}
		for _, row := range rows {
			newest = later(newest, row.UpdatedAt)
			if !strings.EqualFold(row.Repository.FullName, s.repo) || (row.Subject.Type != "Issue" && row.Subject.Type != "PullRequest") {
				continue
			}
			number, ok := s.subjectNumber(row.Subject.URL)
			if !ok {
				continue
			}
			if known, seen := flags[number]; seen && !row.UpdatedAt.After(known.NotifiedAt) {
				continue
			}
			reconcile := true
			flags[number] = model.ThreadUpdate{
				Ref:       model.ThreadRef{Number: number, IsPR: row.Subject.Type == "PullRequest"},
				Reconcile: &reconcile, NotifiedAt: row.UpdatedAt,
			}
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	if notModified {
		return nil
	}
	cursor := stored
	cursor.Key = model.CursorNotifications
	if complete {
		cursor.Since, cursor.LastModified = newest, lastModified
	} else {
		// Unread older pages stay inside the window: keep the position and
		// validator so the next round lists them again.
		cursor.Since = base
	}
	updates := make([]model.ThreadUpdate, 0, len(flags))
	for _, update := range flags {
		updates = append(updates, update)
	}
	slices.SortFunc(updates, func(a, b model.ThreadUpdate) int { return a.Ref.Number - b.Ref.Number })
	if len(updates) == 0 && found && sameCursor(cursor, stored) {
		return s.truncated(complete, "notifications")
	}
	for start := 0; start == 0 || start < len(updates); start += model.MaxBatchEvents {
		end := min(start+model.MaxBatchEvents, len(updates))
		batch := model.IntakeBatch{Resource: "notifications", Threads: updates[start:end]}
		if end == len(updates) {
			batch.Cursor = &cursor
		}
		if err := s.ingest(ctx, rd, batch); err != nil {
			return err
		}
	}
	return s.truncated(complete, "notifications")
}

// truncated reports a notification listing cut by MaxPages. Unlike a
// thread or stream, whose committed position advances page by page, the
// newest-first listing cannot resume, so its unread pages are listed again
// from the kept cursor next round; the operator must see that.
func (s *Source) truncated(complete bool, resource string) error {
	if complete {
		return nil
	}
	return fmt.Errorf("%s: more than %d pages; the cursor stays until a round lists them all", resource, s.maxPages)
}

// streamSpec describes one repository-wide observe stream.
type streamSpec struct {
	key  model.CursorKey
	path string
	// extra query parameters beside the oldest-first defaults.
	extra url.Values
	// entries maps one decoded page to entries; enabledAt filters items.
	entries func(page []byte, enabledAt, observedAt time.Time) ([]entry, error)
}

func (s *Source) pollIssuesStream(ctx context.Context, rd *round) error {
	if !s.observe.Issues && !s.observe.Pulls {
		return nil
	}
	return s.pollStream(ctx, rd, streamSpec{
		key: model.CursorIssues, path: s.repoPath("issues"), extra: url.Values{"state": {"all"}},
		entries: func(page []byte, enabledAt, observedAt time.Time) ([]entry, error) {
			rows, err := decodeBytes[[]ghIssue](page, "issues")
			if err != nil {
				return nil, err
			}
			entries := make([]entry, 0, len(rows))
			for _, row := range rows {
				item := entry{mark: row.UpdatedAt, updated: row.UpdatedAt}
				wanted := s.observe.Issues
				if row.PullRequest != nil {
					wanted = s.observe.Pulls
				}
				if event, ok := s.openedEvent(row, observedAt); ok && wanted && !row.CreatedAt.Before(enabledAt) {
					item.events = []model.Event{event}
				}
				entries = append(entries, item)
			}
			return entries, nil
		},
	})
}

func (s *Source) pollCommentsStream(ctx context.Context, rd *round) error {
	if !s.observe.Comments {
		return nil
	}
	return s.pollStream(ctx, rd, streamSpec{key: model.CursorComments, path: s.repoPath("issues/comments"), entries: s.streamComments(false)})
}

func (s *Source) pollReviewCommentsStream(ctx context.Context, rd *round) error {
	if !s.observe.Comments {
		return nil
	}
	return s.pollStream(ctx, rd, streamSpec{key: model.CursorReviewComments, path: s.repoPath("pulls/comments"), entries: s.streamComments(true)})
}

// streamComments maps a repository-wide comment page to entries. The
// thread comes from the comment's issue or pull request API URL.
func (s *Source) streamComments(inline bool) func([]byte, time.Time, time.Time) ([]entry, error) {
	return func(page []byte, enabledAt, observedAt time.Time) ([]entry, error) {
		rows, err := decodeBytes[[]ghComment](page, "comments")
		if err != nil {
			return nil, err
		}
		entries := make([]entry, 0, len(rows))
		for _, row := range rows {
			item := entry{mark: row.UpdatedAt, updated: row.UpdatedAt}
			apiURL, isPR := row.IssueURL, strings.Contains(row.HTMLURL, "/pull/")
			if inline {
				apiURL, isPR = row.PullRequestURL, true
			}
			number, ok := s.subjectNumber(apiURL)
			if ok && !row.UpdatedAt.Before(enabledAt) {
				events := s.commentEvents(row, model.ThreadRef{Number: number, IsPR: isPR}, inline, observedAt)
				// A version older than the enable time is backlog.
				item.events = slices.DeleteFunc(events, func(event model.Event) bool { return event.At.Before(enabledAt) })
			}
			entries = append(entries, item)
		}
		return entries, nil
	}
}

// pollStream reads one observe stream oldest first from its cursor. The
// first enabled round only records EnabledAt: nothing older is imported.
// Each page commits with the cursor at its last included item; the ETag is
// kept only while the position, and so the request URL, is unchanged.
func (s *Source) pollStream(ctx context.Context, rd *round, spec streamSpec) error {
	stored, found, err := s.intake.Cursor(ctx, spec.key)
	if err != nil {
		return fmt.Errorf("read %s cursor: %w", spec.key, err)
	}
	resource := string(spec.key)
	if !found || stored.EnabledAt.IsZero() {
		baseline := model.Cursor{Key: spec.key, EnabledAt: rd.started, Since: rd.started}
		return s.ingest(ctx, rd, model.IntakeBatch{Resource: resource, Cursor: &baseline})
	}
	from := later(s.rereadFrom(stored.Since), stored.EnabledAt)
	query := listQuery(from, true)
	for key, values := range spec.extra {
		query[key] = values
	}
	cursor := stored
	var etag string
	pages := 0
	complete, err := s.walk(ctx, rd, model.GHRequest{Path: spec.path, Query: query, ETag: stored.ETag}, func(response model.GHResponse, final bool) (bool, error) {
		pages++
		if response.Status == http.StatusNotModified {
			etag = stored.ETag
			return false, nil
		}
		if pages == 1 {
			etag = response.ETag
		}
		entries, err := spec.entries(response.Body, stored.EnabledAt, s.clock.Now())
		if err != nil {
			return false, err
		}
		fresh := false
		for _, run := range split(entries) {
			next := cursor
			next.Since = later(cursor.Since, run[len(run)-1].mark)
			fresh = fresh || next.Since.After(stored.Since)
			events := eventsOf(run)
			if len(events) == 0 && sameCursor(next, cursor) {
				continue
			}
			next.ETag = ""
			if err := s.ingest(ctx, rd, model.IntakeBatch{Resource: resource, Events: events, Cursor: &next}); err != nil {
				return false, err
			}
			cursor = next
		}
		return fresh, nil
	})
	if err != nil {
		return err
	}
	if complete && pages == 1 && cursor.Since.Equal(stored.Since) && etag != cursor.ETag {
		// A one-page read that moved nothing: remember its validator.
		cursor.ETag = etag
		if err := s.ingest(ctx, rd, model.IntakeBatch{Resource: resource, Cursor: &cursor}); err != nil {
			return err
		}
	}
	return nil
}

// pollBranchCI watches the default branch head's settled CI. The first
// settled state is the baseline; later rounds emit only when the settled
// outcome fingerprint changes, pinned to the commit it describes.
func (s *Source) pollBranchCI(ctx context.Context, rd *round) error {
	if !s.observe.CI {
		return nil
	}
	if s.defaultBranch == "" {
		return errors.New("default branch unknown")
	}
	stored, _, err := s.intake.Cursor(ctx, model.CursorBranchCI)
	if err != nil {
		return fmt.Errorf("read %s cursor: %w", model.CursorBranchCI, err)
	}
	response, err := s.get(ctx, rd, model.GHRequest{Path: s.repoPath("branches/%s", url.PathEscape(s.defaultBranch))})
	if err != nil {
		return err
	}
	head, err := decode[ghBranchHead](response, "branch")
	if err != nil {
		return err
	}
	sha := head.Commit.SHA
	if sha == "" {
		return nil
	}
	ci, settled, err := s.readCI(ctx, rd, sha)
	if err != nil || !settled {
		return err
	}
	cursor := stored
	cursor.Key, cursor.BranchSHA, cursor.Fingerprint = model.CursorBranchCI, sha, ci.Fingerprint
	if sha == stored.BranchSHA && ci.Fingerprint == stored.Fingerprint {
		return nil
	}
	batch := model.IntakeBatch{Resource: string(model.CursorBranchCI), Cursor: &cursor}
	if stored.Fingerprint != "" && stored.Fingerprint != ci.Fingerprint {
		event := ciEvent(model.BranchCIID(s.defaultBranch, sha, ci.Fingerprint), model.EventBranchCI, model.ThreadRef{}, ci,
			fmt.Sprintf("https://github.com/%s/commit/%s", s.repo, sha), s.clock.Now())
		event.Branch = s.defaultBranch
		batch.Events = []model.Event{event}
	}
	return s.ingest(ctx, rd, batch)
}

// readCI reads check runs and commit statuses of sha. settled is false
// while anything is pending, when nothing reports, or when a listing was
// cut by MaxPages.
func (s *Source) readCI(ctx context.Context, rd *round, sha string) (model.CISnapshot, bool, error) {
	var outcomes []ciOutcome
	checks := model.GHRequest{Path: s.repoPath("commits/%s/check-runs", sha), Query: url.Values{"filter": {"latest"}, "per_page": {strconv.Itoa(listPageSize)}}}
	complete, err := s.walk(ctx, rd, checks, func(response model.GHResponse, final bool) (bool, error) {
		page, err := decode[ghCheckRuns](response, "check runs")
		if err != nil {
			return false, err
		}
		for _, run := range page.CheckRuns {
			outcome := ciOutcome{name: run.Name, pending: run.Status != "completed" || run.Conclusion == nil}
			if run.Conclusion != nil {
				outcome.outcome = *run.Conclusion
			}
			outcomes = append(outcomes, outcome)
		}
		return true, nil
	})
	if err != nil || !complete {
		return model.CISnapshot{}, false, err
	}
	statuses := model.GHRequest{Path: s.repoPath("commits/%s/status", sha), Query: url.Values{"per_page": {strconv.Itoa(listPageSize)}}}
	complete, err = s.walk(ctx, rd, statuses, func(response model.GHResponse, final bool) (bool, error) {
		page, err := decode[ghCombinedStatus](response, "combined status")
		if err != nil {
			return false, err
		}
		for _, status := range page.Statuses {
			outcomes = append(outcomes, ciOutcome{name: status.Context, outcome: status.State, pending: status.State == "pending"})
		}
		return true, nil
	})
	if err != nil || !complete {
		return model.CISnapshot{}, false, err
	}
	ci, settled := snapshot(sha, outcomes)
	return ci, settled, nil
}
