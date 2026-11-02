package source

import (
	"bytes"
	"cmp"
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

// pollWatched reads watched threads, flagged ones first and within each
// group the one read longest ago first, while the round's allowance
// lasts. Two reads go on when it is spent: the first flagged thread, so
// flags always progress, and the overdue thread, the one read longest ago
// of those unread for watchFloorRounds rounds, so every watched thread is
// read within watchFloorRounds + N - 1 rounds of its last read, for N
// threads watched meanwhile, however much else the account spends (up to
// the outbox reserve). The rest wait for a later round, which reads them
// before the threads read this one.
func (s *Source) pollWatched(ctx context.Context, rd *round) error {
	watched, err := s.intake.Watched(ctx, rd.started)
	if err != nil {
		return fmt.Errorf("list watched threads: %w", err)
	}
	lastRead := make(map[int]int, len(watched))
	for _, item := range watched {
		number := item.Thread.Ref.Number
		lastRead[number] = s.readRound[number]
	}
	s.readRound = lastRead
	slices.SortStableFunc(watched, func(a, b model.WatchedThread) int {
		if a.Thread.NeedsReconcile != b.Thread.NeedsReconcile {
			if a.Thread.NeedsReconcile {
				return -1
			}
			return 1
		}
		return cmp.Or(lastRead[a.Thread.Ref.Number]-lastRead[b.Thread.Ref.Number], a.Thread.Ref.Number-b.Thread.Ref.Number)
	})
	// overdue is the floor's thread: of those unread for watchFloorRounds,
	// the one read longest ago.
	overdue := 0
	for _, item := range watched {
		number := item.Thread.Ref.Number
		if item.Thread.State == stateGone || s.rounds-lastRead[number] < watchFloorRounds {
			continue
		}
		if overdue == 0 || cmp.Or(lastRead[number]-lastRead[overdue], number-overdue) < 0 {
			overdue = number
		}
	}
	read := 0
	for _, item := range watched {
		if rd.stopped() {
			return nil
		}
		number := item.Thread.Ref.Number
		if item.Thread.State == stateGone {
			continue
		}
		if rd.spare() <= 0 && (read > 0 || !item.Thread.NeedsReconcile) && number != overdue {
			continue
		}
		read++
		s.readRound[number] = s.rounds
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
	// readAt is the Date of the pulls reply the record came from.
	readAt time.Time
}

func newPRTracker(record model.PullRequest, known *model.PullRequest, readAt time.Time) *prTracker {
	tracker := &prTracker{record: record, readAt: readAt}
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

// attach adds the record to batch when it changed. The batch is then read
// at the pulls reply's Date: control orders PR snapshots by ReadAt, and
// the local clock must not decide which snapshot is newer.
func (t *prTracker) attach(batch *model.IntakeBatch) {
	if prs := t.pending(); prs != nil {
		batch.PRs, batch.ReadAt = prs, t.readAt
	}
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
	// The read begins at GitHub's clock. Date has whole seconds, so a
	// notification in the same second may be newer than the read: count
	// the read from the second before.
	began, err := serverDate(response)
	if err != nil {
		return err
	}
	readAt := began.Add(-time.Second)
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
		pulledAt, err := serverDate(response)
		if err != nil {
			return err
		}
		pr = newPRTracker(pullRecord(pull, watched.PR), watched.PR, pulledAt)
		header.Events = append(header.Events, pullFacts(pull, watched.PR, ref, observedAt)...)
		pr.attach(&header)
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

// readComments scans the conversation, which GitHub lists in ID order.
func (s *Source) readComments(ctx context.Context, rd *round, ref model.ThreadRef, old model.Position) (bool, error) {
	return s.scan(ctx, rd, scanSpec{
		resource: threadResource(ref.Number, "comments"),
		path:     s.repoPath("issues/%d/comments", ref.Number),
		windowed: true,
		old:      old,
		entries:  s.commentEntries(ref, false),
		position: func(batch *model.IntakeBatch, position model.Position) {
			batch.Threads = []model.ThreadUpdate{{Ref: ref, Comments: &position}}
		},
	})
}

// readReviewComments scans the inline comments. Without a sort parameter
// GitHub lists them in ID order.
func (s *Source) readReviewComments(ctx context.Context, rd *round, ref model.ThreadRef, old model.Position) (bool, error) {
	return s.scan(ctx, rd, scanSpec{
		resource: threadResource(ref.Number, "review_comments"),
		path:     s.repoPath("pulls/%d/comments", ref.Number),
		windowed: true,
		old:      old,
		entries:  s.commentEntries(ref, true),
		position: func(batch *model.IntakeBatch, position model.Position) {
			batch.Threads = []model.ThreadUpdate{{Ref: ref, ReviewComments: &position}}
		},
	})
}

func (s *Source) commentEntries(ref model.ThreadRef, inline bool) func([]byte, time.Time) ([]entry, error) {
	return func(raw []byte, observedAt time.Time) ([]entry, error) {
		rows, err := decodeBytes[[]ghComment](raw, "comments")
		if err != nil {
			return nil, err
		}
		entries := make([]entry, 0, len(rows))
		for _, row := range rows {
			entries = append(entries, entry{events: s.commentEvents(row, ref, inline, observedAt), updated: row.UpdatedAt, id: row.ID})
		}
		return entries, nil
	}
}

// readReviews scans every review (the chronological listing has no
// since), so a scan also sees dismissals of reviews it read before. A
// review not yet on the PR record becomes an event, a dismissal not yet
// recorded a dismissed event; each review is kept on the record pinned to
// the commit it reviewed.
//
// GitHub promises this listing chronological, not in ID order. Every
// listing sampled so far was also in ID order, which the scan's walk
// assumes and checks at every join, a resumed scan's first included: a
// reply out of ID order, or one that does not list the review the scan
// stopped at, makes the scan read the reviews whole instead of skipping a
// lower ID listed later.
func (s *Source) readReviews(ctx context.Context, rd *round, ref model.ThreadRef, old model.Position, pr *prTracker) (bool, error) {
	return s.scan(ctx, rd, scanSpec{
		resource: threadResource(ref.Number, "reviews"),
		path:     s.repoPath("pulls/%d/reviews", ref.Number),
		old:      old,
		pr:       pr,
		entries: func(raw []byte, observedAt time.Time) ([]entry, error) {
			rows, err := decodeBytes[[]ghReview](raw, "reviews")
			if err != nil {
				return nil, err
			}
			entries := make([]entry, 0, len(rows))
			for _, row := range rows {
				item := entry{id: row.ID}
				if row.State == reviewPending || s.isAccount(row.User) {
					// Listed, so it orders the scan, but never recorded.
					entries = append(entries, item)
					continue
				}
				if row.SubmittedAt != nil {
					item.updated = *row.SubmittedAt
				}
				index := slices.IndexFunc(pr.record.Reviews, func(fact model.ReviewFact) bool { return fact.ID == row.ID })
				if index < 0 {
					item.events = append(item.events, s.reviewEvent(row, ref, false, observedAt))
				}
				if row.State == reviewDismissed && (index < 0 || pr.record.Reviews[index].State != reviewDismissed) {
					item.events = append(item.events, s.reviewEvent(row, ref, true, observedAt))
				}
				fact := model.ReviewFact{ID: row.ID, Actor: actor(row.User), State: row.State, CommitSHA: row.CommitID, SubmittedAt: item.updated}
				item.apply = func() {
					if index := slices.IndexFunc(pr.record.Reviews, func(known model.ReviewFact) bool { return known.ID == fact.ID }); index >= 0 {
						pr.record.Reviews[index] = fact
						return
					}
					pr.record.Reviews = append(pr.record.Reviews, fact)
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
	batch := model.IntakeBatch{Resource: threadResource(ref.Number, "ci"), Events: []model.Event{event}}
	pr.attach(&batch)
	if err := s.ingest(ctx, rd, batch); err != nil {
		return err
	}
	pr.markCommitted()
	return nil
}
