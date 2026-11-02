package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// stateGone is the Thread state of an issue GitHub answers 410 for.
const stateGone = "gone"

// pollWatched reads every watched thread, flagged ones first. When the
// rate budget is low only flagged threads are read.
func (s *Source) pollWatched(ctx context.Context, rd *round) error {
	watched, err := s.intake.Watched(ctx, rd.started)
	if err != nil {
		return fmt.Errorf("list watched threads: %w", err)
	}
	slices.SortStableFunc(watched, func(a, b model.WatchedThread) int {
		if a.Thread.NeedsReconcile != b.Thread.NeedsReconcile {
			if a.Thread.NeedsReconcile {
				return -1
			}
			return 1
		}
		return a.Thread.Ref.Number - b.Thread.Ref.Number
	})
	for _, item := range watched {
		if rd.stopped() {
			return nil
		}
		if item.Thread.State == stateGone || (rd.lowBudget() && !item.Thread.NeedsReconcile) {
			continue
		}
		if err := s.readThread(ctx, rd, item); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.fail(rd, threadResource(item.Thread.Ref.Number, ""), err)
		}
	}
	return nil
}

// prTracker holds a PR record as it is being read and what of it was
// committed, so batches carry the record only when it changed.
type prTracker struct {
	record    model.PullRequest
	committed []byte
}

func newPRTracker(record model.PullRequest, known *model.PullRequest) *prTracker {
	tracker := &prTracker{record: record}
	if known != nil {
		tracker.committed = mustJSON(*known)
	}
	return tracker
}

// pending returns the record when it differs from the committed one.
func (t *prTracker) pending() []model.PullRequest {
	if t == nil || bytes.Equal(mustJSON(t.record), t.committed) {
		return nil
	}
	return []model.PullRequest{t.record}
}

func (t *prTracker) markCommitted() {
	if t != nil {
		t.committed = mustJSON(t.record)
	}
}

// mustJSON marshals model records, which contain no unmarshalable types.
func mustJSON(value model.PullRequest) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("source: marshal pull request: %v", err))
	}
	return raw
}

// readThread reads one Issue or PR completely from its committed
// positions: the issue, the PR and its facts, conversation comments, and
// for a PR its reviews, inline comments and head CI. The last batch of a
// complete read of a flagged thread clears the flag.
func (s *Source) readThread(ctx context.Context, rd *round, watched model.WatchedThread) error {
	thread := watched.Thread
	number := thread.Ref.Number
	readAt := s.clock.Now()
	resource := threadResource(number, "")
	response, err := s.get(ctx, rd, model.GHRequest{Path: s.repoPath("issues/%d", number)})
	var ghErr *model.GHError
	if errors.As(err, &ghErr) && ghErr.Status == http.StatusGone {
		if thread.State == stateGone {
			return nil
		}
		return s.ingest(ctx, rd, model.IntakeBatch{Resource: resource, Threads: []model.ThreadUpdate{{Ref: thread.Ref, State: stateGone}}})
	}
	if err != nil {
		return err
	}
	issue, err := decode[ghIssue](response, "issue")
	if err != nil {
		return err
	}
	ref := model.ThreadRef{Number: number, IsPR: issue.PullRequest != nil}
	observedAt := s.clock.Now()
	header := model.IntakeBatch{Resource: resource}
	author := actor(issue.User)
	update := model.ThreadUpdate{Ref: ref, Title: issue.Title, State: issue.State, Author: &author, LastActivityAt: issue.UpdatedAt}
	if thread.Ref != ref || thread.Title != update.Title || thread.State != update.State || thread.Author != author || !thread.LastActivityAt.Equal(update.LastActivityAt) {
		// The opened event commits with the metadata that records this
		// updated_at, so an unchanged issue has nothing new to send.
		header.Threads = []model.ThreadUpdate{update}
		if event, ok := s.openedEvent(issue, observedAt); ok {
			header.Events = append(header.Events, event)
		}
	}
	var pr *prTracker
	if ref.IsPR {
		response, err := s.get(ctx, rd, model.GHRequest{Path: s.repoPath("pulls/%d", number)})
		if err != nil {
			return err
		}
		pull, err := decode[ghPull](response, "pull request")
		if err != nil {
			return err
		}
		pr = newPRTracker(pullRecord(pull, watched.PR), watched.PR)
		header.Events = append(header.Events, pullFacts(pull, watched.PR, ref, observedAt)...)
		header.PRs = pr.pending()
	}
	if len(header.Events) > 0 || len(header.Threads) > 0 || len(header.PRs) > 0 {
		if err := s.ingest(ctx, rd, header); err != nil {
			return err
		}
		pr.markCommitted()
	}

	complete, err := s.readComments(ctx, rd, ref, thread.Comments)
	if err != nil {
		return err
	}
	if ref.IsPR {
		reviewsComplete, err := s.readReviews(ctx, rd, ref, thread.Reviews, pr)
		if err != nil {
			return err
		}
		inlineComplete, err := s.readReviewComments(ctx, rd, ref, thread.ReviewComments)
		if err != nil {
			return err
		}
		complete = complete && reviewsComplete && inlineComplete
		if pr.record.State == stateOpen && pr.record.HeadSHA != "" {
			if err := s.readPRCI(ctx, rd, ref, pr); err != nil {
				return err
			}
		}
	}
	if !complete || !thread.NeedsReconcile {
		// A read cut by MaxPages resumes from its committed positions next
		// round; the flag stays until a read completes.
		return nil
	}
	done := false
	// ReadAt is when the read began: a notification newer than that keeps
	// the flag for another read.
	return s.ingest(ctx, rd, model.IntakeBatch{Resource: resource, ReadAt: readAt, Threads: []model.ThreadUpdate{{Ref: ref, Reconcile: &done}}})
}

// pullRecord is the gh/pr record of pull, keeping control's Owner and the
// facts that still describe it: reviews, and CI only for the same head.
func pullRecord(pull ghPull, known *model.PullRequest) model.PullRequest {
	record := model.PullRequest{
		Number: pull.Number, Author: actor(pull.User), HeadRef: pull.Head.Ref, HeadSHA: pull.Head.SHA, BaseRef: pull.Base.Ref,
		State: pull.State, Merged: pull.Merged, Draft: pull.Draft, UpdatedAt: pull.UpdatedAt,
	}
	if pull.Head.Repo != nil {
		record.HeadRepo = pull.Head.Repo.FullName
	}
	if pull.Merged && pull.MergeCommitSHA != nil {
		record.MergeSHA = *pull.MergeCommitSHA
	}
	if pull.ClosedAt != nil {
		record.ClosedAt = *pull.ClosedAt
	}
	if known != nil {
		record.Owner = known.Owner
		record.Reviews = slices.Clone(known.Reviews)
		if known.CI != nil && known.CI.SHA == record.HeadSHA {
			ci := *known.CI
			record.CI = &ci
		}
	}
	return record
}

// pullFacts are the PR state changes since known: a head move, a close or
// merge, a reopen. A first sighting records the head without an event.
func pullFacts(pull ghPull, known *model.PullRequest, ref model.ThreadRef, observedAt time.Time) []model.Event {
	base := model.Event{Thread: ref, ObservedAt: observedAt, URL: pull.HTMLURL}
	var events []model.Event
	if known != nil && known.HeadSHA != "" && known.HeadSHA != pull.Head.SHA {
		event := base
		event.ID, event.Kind, event.At, event.CommitSHA = model.PRHeadID(pull.Number, pull.Head.SHA), model.EventPRHead, pull.UpdatedAt, pull.Head.SHA
		events = append(events, event)
	}
	switch {
	case pull.State == stateClosed:
		var closedAt time.Time
		if pull.ClosedAt != nil {
			closedAt = *pull.ClosedAt
		}
		if known != nil && known.State == stateClosed && known.Merged == pull.Merged && known.ClosedAt.Equal(closedAt) {
			break
		}
		event := base
		if pull.Merged {
			mergeSHA := ""
			if pull.MergeCommitSHA != nil {
				mergeSHA = *pull.MergeCommitSHA
			}
			event.ID, event.Kind, event.CommitSHA, event.At = model.PRMergedID(pull.Number, mergeSHA), model.EventPRMerged, mergeSHA, closedAt
			if pull.MergedAt != nil {
				event.At = *pull.MergedAt
			}
		} else {
			event.ID, event.Kind, event.At = model.PRClosedID(pull.Number, closedAt), model.EventPRClosed, closedAt
		}
		events = append(events, event)
	case known != nil && known.State == stateClosed:
		event := base
		event.ID, event.Kind, event.At = model.PRReopenedID(pull.Number, pull.UpdatedAt), model.EventPRReopened, pull.UpdatedAt
		events = append(events, event)
	}
	return events
}

// positionedRead is one paginated thread stream committed page by page.
type positionedRead struct {
	resource string
	request  model.GHRequest
	old      model.Position
	entries  func(raw []byte, observedAt time.Time) ([]entry, error)
	position func(batch *model.IntakeBatch, position model.Position)
	// pr, when set, rides along in every batch that changes it.
	pr *prTracker
}

// readPositioned walks a stream and commits each page in bounded runs.
// Before the last page the position moves to the last run's mark, which
// every unread item is at or after; the last run of a complete walk moves
// it to the newest updated_at read, never backward.
func (s *Source) readPositioned(ctx context.Context, rd *round, read positionedRead) (bool, error) {
	committed := read.old
	newest := read.old.Since
	return s.walk(ctx, rd, read.request, func(response model.GHResponse, final bool) (bool, error) {
		entries, err := read.entries(response.Body, s.clock.Now())
		if err != nil {
			return false, err
		}
		fresh := false
		for _, item := range entries {
			newest = later(newest, item.updated)
			fresh = fresh || item.updated.After(read.old.Since) || item.id > read.old.LastID
		}
		runs := split(entries)
		if final && len(runs) == 0 {
			runs = [][]entry{nil}
		}
		for i, run := range runs {
			position := committed
			if len(run) > 0 {
				last := run[len(run)-1]
				position.Since = later(position.Since, last.mark)
				position.LastID = max(position.LastID, last.id)
			}
			if final && i == len(runs)-1 {
				position.Since = later(position.Since, newest)
			}
			events := eventsOf(run)
			prs := read.pr.pending()
			moved := !position.Since.Equal(committed.Since) || position.LastID != committed.LastID
			if len(events) == 0 && len(prs) == 0 && !moved {
				continue
			}
			batch := model.IntakeBatch{Resource: read.resource, Events: events, PRs: prs}
			read.position(&batch, position)
			if err := s.ingest(ctx, rd, batch); err != nil {
				return false, err
			}
			committed = position
			read.pr.markCommitted()
		}
		return fresh, nil
	})
}

func (s *Source) readComments(ctx context.Context, rd *round, ref model.ThreadRef, old model.Position) (bool, error) {
	return s.readPositioned(ctx, rd, positionedRead{
		resource: threadResource(ref.Number, "comments"),
		// The per-issue listing is in ID order and filters by updated_at.
		request: model.GHRequest{Path: s.repoPath("issues/%d/comments", ref.Number), Query: listQuery(s.rereadFrom(old.Since), false)},
		old:     old,
		entries: func(raw []byte, observedAt time.Time) ([]entry, error) {
			rows, err := decodeBytes[[]ghComment](raw, "comments")
			if err != nil {
				return nil, err
			}
			entries := make([]entry, 0, len(rows))
			for _, row := range rows {
				// Unread items have higher IDs, so they were created no
				// earlier than this one and updated no earlier still.
				entries = append(entries, entry{events: s.commentEvents(row, ref, false, observedAt), mark: row.CreatedAt, updated: row.UpdatedAt})
			}
			return entries, nil
		},
		position: func(batch *model.IntakeBatch, position model.Position) {
			batch.Threads = []model.ThreadUpdate{{Ref: ref, Comments: &position}}
		},
	})
}

func (s *Source) readReviewComments(ctx context.Context, rd *round, ref model.ThreadRef, old model.Position) (bool, error) {
	return s.readPositioned(ctx, rd, positionedRead{
		resource: threadResource(ref.Number, "review_comments"),
		request:  model.GHRequest{Path: s.repoPath("pulls/%d/comments", ref.Number), Query: listQuery(s.rereadFrom(old.Since), true)},
		old:      old,
		entries: func(raw []byte, observedAt time.Time) ([]entry, error) {
			rows, err := decodeBytes[[]ghComment](raw, "review comments")
			if err != nil {
				return nil, err
			}
			entries := make([]entry, 0, len(rows))
			for _, row := range rows {
				entries = append(entries, entry{events: s.commentEvents(row, ref, true, observedAt), mark: row.UpdatedAt, updated: row.UpdatedAt})
			}
			return entries, nil
		},
		position: func(batch *model.IntakeBatch, position model.Position) {
			batch.Threads = []model.ThreadUpdate{{Ref: ref, ReviewComments: &position}}
		},
	})
}

// readReviews reads every review page (the listing has no since). Reviews
// after LastID become events; a dismissal of an earlier one becomes a
// dismissed event. Each review is kept on the PR record pinned to the
// commit it reviewed.
func (s *Source) readReviews(ctx context.Context, rd *round, ref model.ThreadRef, old model.Position, pr *prTracker) (bool, error) {
	return s.readPositioned(ctx, rd, positionedRead{
		resource: threadResource(ref.Number, "reviews"),
		request:  model.GHRequest{Path: s.repoPath("pulls/%d/reviews", ref.Number), Query: listQuery(time.Time{}, false)},
		old:      old,
		pr:       pr,
		entries: func(raw []byte, observedAt time.Time) ([]entry, error) {
			rows, err := decodeBytes[[]ghReview](raw, "reviews")
			if err != nil {
				return nil, err
			}
			entries := make([]entry, 0, len(rows))
			for _, row := range rows {
				if row.State == reviewPending || s.isAccount(row.User) {
					continue
				}
				item := entry{id: row.ID}
				if row.SubmittedAt != nil {
					item.mark, item.updated = *row.SubmittedAt, *row.SubmittedAt
				}
				index := slices.IndexFunc(pr.record.Reviews, func(fact model.ReviewFact) bool { return fact.ID == row.ID })
				switch {
				case row.ID > old.LastID:
					item.events = append(item.events, s.reviewEvent(row, ref, false, observedAt))
					if row.State == reviewDismissed {
						item.events = append(item.events, s.reviewEvent(row, ref, true, observedAt))
					}
				case row.State == reviewDismissed && (index < 0 || pr.record.Reviews[index].State != reviewDismissed):
					item.events = append(item.events, s.reviewEvent(row, ref, true, observedAt))
				}
				fact := model.ReviewFact{ID: row.ID, Actor: actor(row.User), State: row.State, CommitSHA: row.CommitID, SubmittedAt: item.mark}
				if index < 0 {
					pr.record.Reviews = append(pr.record.Reviews, fact)
				} else {
					pr.record.Reviews[index] = fact
				}
				entries = append(entries, item)
			}
			return entries, nil
		},
		position: func(batch *model.IntakeBatch, position model.Position) {
			batch.Threads = []model.ThreadUpdate{{Ref: ref, Reviews: &position}}
		},
	})
}

// readPRCI emits the settled CI of the PR head when it differs from the
// snapshot already recorded for that head.
func (s *Source) readPRCI(ctx context.Context, rd *round, ref model.ThreadRef, pr *prTracker) error {
	sha := pr.record.HeadSHA
	ci, settled, err := s.readCI(ctx, rd, sha)
	if err != nil || !settled {
		return err
	}
	if known := pr.record.CI; known != nil && known.SHA == sha && known.Fingerprint == ci.Fingerprint {
		return nil
	}
	pr.record.CI = &ci
	url := fmt.Sprintf("https://github.com/%s/pull/%d/checks", s.repo, ref.Number)
	event := ciEvent(model.PRCIID(ref.Number, sha, ci.Fingerprint), model.EventPRCI, ref, ci, url, s.clock.Now())
	if err := s.ingest(ctx, rd, model.IntakeBatch{Resource: threadResource(ref.Number, "ci"), Events: []model.Event{event}, PRs: pr.pending()}); err != nil {
		return err
	}
	pr.markCommitted()
	return nil
}
