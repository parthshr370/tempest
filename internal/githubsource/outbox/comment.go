package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// comment is the part of a conversation or inline comment the outbox reads.
type comment struct {
	ID      int64  `json:"id"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	User    struct {
		ID int64 `json:"id"`
	} `json:"user"`
}

func (c comment) reply(reconciled bool) *model.EffectReply {
	return &model.EffectReply{RemoteID: c.ID, URL: c.HTMLURL, Reconciled: reconciled}
}

// attemptComment posts a conversation comment or a review reply. A retry
// (an earlier attempt was proven absent) first searches again, because an
// earlier write may surface late in GitHub's listings.
func (o *Outbox) attemptComment(ctx context.Context, intent model.OutboxIntent) step {
	if intent.Attempts > 1 {
		found, err := o.findComment(ctx, intent)
		switch {
		case err != nil:
			return unread(fmt.Errorf("search before retry: %w", err))
		case found != nil:
			return confirmed(found)
		}
	}
	payload := intent.Payload
	if intent.Kind == model.EffectReviewReply {
		posted, err := o.postComment(ctx, fmt.Sprintf("repos/%s/pulls/%d/comments/%d/replies", o.repo, payload.Number, payload.ReplyTo), marked(payload.Body, intent.ID))
		if status(err) != http.StatusNotFound {
			return o.commentOutcome(ctx, intent, posted, err)
		}
		// The review thread is gone: answer in the PR conversation.
		note := fmt.Sprintf("_Reply to review comment %d, whose thread is no longer available:_\n\n", payload.ReplyTo)
		posted, err = o.postComment(ctx, fmt.Sprintf("repos/%s/issues/%d/comments", o.repo, payload.Number), marked(note+payload.Body, intent.ID))
		return o.commentOutcome(ctx, intent, posted, err)
	}
	posted, err := o.postComment(ctx, fmt.Sprintf("repos/%s/issues/%d/comments", o.repo, payload.Number), marked(payload.Body, intent.ID))
	return o.commentOutcome(ctx, intent, posted, err)
}

func (o *Outbox) commentOutcome(ctx context.Context, intent model.OutboxIntent, posted comment, err error) step {
	verdict, retryAfter := classify(err)
	switch verdict {
	case sent:
		return confirmed(posted.reply(false))
	case refusedRetry:
		return o.absent(intent, err, retryAfter)
	case refused:
		return failed(err)
	}
	if ctx.Err() != nil {
		return unread(ctx.Err())
	}
	return o.reconcileComment(ctx, intent)
}

func (o *Outbox) postComment(ctx context.Context, path, body string) (comment, error) {
	raw, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return comment{}, err
	}
	response, err := o.client.Do(ctx, model.GHRequest{Method: http.MethodPost, Path: path, Body: raw})
	if err != nil {
		return comment{}, err
	}
	var posted comment
	if err := json.Unmarshal(response.Body, &posted); err != nil || posted.ID == 0 {
		// GitHub accepted the write; only the reply is unreadable.
		return comment{}, &model.GHError{Message: fmt.Sprintf("POST %s: unreadable HTTP %d reply", path, response.Status)}
	}
	return posted, nil
}

// reconcileComment searches for the marker: found is confirmed, a complete
// search without it proves the write absent.
func (o *Outbox) reconcileComment(ctx context.Context, intent model.OutboxIntent) step {
	found, err := o.findComment(ctx, intent)
	switch {
	case err != nil:
		return unread(err)
	case found != nil:
		return confirmed(found)
	}
	return o.absent(intent, errors.New("reconciled: the comment was not posted"), 0)
}

// findComment pages the target thread's comments by the account since the
// intent was created and returns the one carrying the intent's marker. A
// review reply is searched among the PR's inline comments and, for the
// fallback, its conversation.
func (o *Outbox) findComment(ctx context.Context, intent model.OutboxIntent) (*model.EffectReply, error) {
	since := intent.CreatedAt.Add(-reconcileSkew).UTC().Format(time.RFC3339)
	paths := []string{fmt.Sprintf("repos/%s/issues/%d/comments", o.repo, intent.Payload.Number)}
	if intent.Kind == model.EffectReviewReply {
		paths = append([]string{fmt.Sprintf("repos/%s/pulls/%d/comments", o.repo, intent.Payload.Number)}, paths...)
	}
	marker := model.OutboxMarker(intent.ID)
	for _, path := range paths {
		var found *comment
		err := o.scan(ctx, path, url.Values{"since": {since}}, func(raw json.RawMessage) (bool, error) {
			var rows []comment
			if err := json.Unmarshal(raw, &rows); err != nil {
				return false, err
			}
			for _, row := range rows {
				if row.User.ID == o.account && strings.Contains(row.Body, marker) {
					found = &row
					return true, nil
				}
			}
			return false, nil
		})
		if err != nil {
			return nil, err
		}
		if found != nil {
			return found.reply(true), nil
		}
	}
	return nil, nil
}

// scan pages a listing until visit reports a match or the listing ends.
// An error means the listing was not read to its end, so absence is not
// proven.
func (o *Outbox) scan(ctx context.Context, path string, query url.Values, visit func(json.RawMessage) (bool, error)) error {
	query.Set("per_page", strconv.Itoa(scanPageSize))
	request := model.GHRequest{Method: http.MethodGet, Path: path, Query: query}
	for range maxScanPages {
		response, err := o.client.Do(ctx, request)
		if err != nil {
			return fmt.Errorf("list %s: %w", path, err)
		}
		done, err := visit(response.Body)
		if err != nil {
			return fmt.Errorf("list %s: %w", path, err)
		}
		if done || response.NextPath == "" {
			return nil
		}
		request = model.GHRequest{Method: http.MethodGet, Path: response.NextPath}
	}
	return fmt.Errorf("list %s: more than %d pages", path, maxScanPages)
}
