package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"go.harness.dev/harness/internal/githubsource/model"
)

// comment is the part of a conversation or inline comment the outbox reads.
type comment struct {
	ID      int64  `json:"id"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	// InReplyTo is the root of an inline reply's review thread.
	InReplyTo int64 `json:"in_reply_to_id"`
	User      struct {
		ID int64 `json:"id"`
	} `json:"user"`
}

func (c comment) reply(reconciled bool) *model.EffectReply {
	return &model.EffectReply{RemoteID: c.ID, URL: c.HTMLURL, Reconciled: reconciled}
}

// maxThreadDepth bounds the walk from an inline comment to its thread's
// root; GitHub threads are one level deep.
const maxThreadDepth = 8

// attemptComment posts a conversation comment or a review reply. A review
// reply goes to the root of the target's thread: GitHub's replies endpoint
// accepts only a top-level comment.
func (o *Outbox) attemptComment(ctx context.Context, intent model.OutboxIntent) step {
	payload := intent.Payload
	conversation := fmt.Sprintf("repos/%s/issues/%d/comments", o.repo, payload.Number)
	if intent.Kind != model.EffectReviewReply {
		posted, err := o.postComment(ctx, conversation, marked(payload.Body, intent.ID))
		return o.commentOutcome(ctx, intent, 0, posted, err)
	}
	root, err := o.reviewRoot(ctx, payload.Number, payload.ReplyTo)
	if err != nil {
		return o.retry(intent, fmt.Errorf("find the review thread before replying: %w", err), retryAfter(err))
	}
	posted, err := o.postComment(ctx, fmt.Sprintf("repos/%s/pulls/%d/comments/%d/replies", o.repo, payload.Number, root), marked(payload.Body, intent.ID))
	if status(err) != http.StatusNotFound {
		return o.commentOutcome(ctx, intent, root, posted, err)
	}
	// The review thread is gone: answer in the PR conversation.
	note := fmt.Sprintf("_Reply to review comment %d, whose thread is no longer available:_\n\n", payload.ReplyTo)
	posted, err = o.postComment(ctx, conversation, marked(note+payload.Body, intent.ID))
	return o.commentOutcome(ctx, intent, root, posted, err)
}

func (o *Outbox) commentOutcome(ctx context.Context, intent model.OutboxIntent, root int64, posted comment, err error) step {
	verdict, wait := classify(err)
	switch verdict {
	case sent:
		return confirmed(posted.reply(false))
	case refusedRetry:
		return o.retry(intent, err, wait)
	case refused:
		return failed(err)
	}
	if ctx.Err() != nil {
		return unread(ctx.Err())
	}
	return o.searchComment(ctx, intent, root, err)
}

func (o *Outbox) postComment(ctx context.Context, path, body string) (comment, error) {
	raw, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return comment{}, err
	}
	response, err := o.do(ctx, model.GHRequest{Method: http.MethodPost, Path: path, Body: raw})
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

// reconcileComment decides a comment found attempting after a restart.
func (o *Outbox) reconcileComment(ctx context.Context, intent model.OutboxIntent) step {
	var root int64
	if intent.Kind == model.EffectReviewReply {
		var err error
		if root, err = o.reviewRoot(ctx, intent.Payload.Number, intent.Payload.ReplyTo); err != nil {
			return unread(err)
		}
	}
	return o.searchComment(ctx, intent, root, errors.New("the process stopped after the attempt was recorded"))
}

// searchComment looks for a comment whose request may have reached GitHub.
// Found is confirmed; not found settles unknown, never pending.
func (o *Outbox) searchComment(ctx context.Context, intent model.OutboxIntent, root int64, cause error) step {
	found, err := o.findComment(ctx, intent, root)
	switch {
	case err != nil:
		return unread(err)
	case found != nil:
		return confirmed(found)
	}
	return unsure("the comment", cause)
}

// findComment pages the target thread's comments and returns the account's
// comment ending with the intent's marker. A review reply is searched in its
// review thread (root) among the PR's inline comments and, for the
// fallback, in the PR conversation. The listings are read whole: a time
// bound would mix the controller's clock with GitHub's.
func (o *Outbox) findComment(ctx context.Context, intent model.OutboxIntent, root int64) (*model.EffectReply, error) {
	type listing struct {
		path   string
		inline bool
	}
	listings := []listing{{path: fmt.Sprintf("repos/%s/issues/%d/comments", o.repo, intent.Payload.Number)}}
	if intent.Kind == model.EffectReviewReply {
		listings = append([]listing{{path: fmt.Sprintf("repos/%s/pulls/%d/comments", o.repo, intent.Payload.Number), inline: true}}, listings...)
	}
	for _, list := range listings {
		var found *comment
		err := o.scan(ctx, list.path, url.Values{}, func(raw json.RawMessage) (bool, error) {
			var rows []comment
			if err := json.Unmarshal(raw, &rows); err != nil {
				return false, err
			}
			for _, row := range rows {
				if row.User.ID == o.account && (!list.inline || row.InReplyTo == root) && carries(row.Body, intent.ID) {
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

// reviewRoot returns the root of the review thread an inline comment of PR
// number belongs to, or the comment itself when the listing does not show
// it, in which case GitHub's answer to the reply decides.
func (o *Outbox) reviewRoot(ctx context.Context, number int, id int64) (int64, error) {
	parents := make(map[int64]int64)
	err := o.scan(ctx, fmt.Sprintf("repos/%s/pulls/%d/comments", o.repo, number), url.Values{}, func(raw json.RawMessage) (bool, error) {
		var rows []comment
		if err := json.Unmarshal(raw, &rows); err != nil {
			return false, err
		}
		for _, row := range rows {
			parents[row.ID] = row.InReplyTo
		}
		return false, nil
	})
	if err != nil {
		return 0, err
	}
	root := id
	for range maxThreadDepth {
		parent, found := parents[root]
		if !found || parent == 0 {
			break
		}
		root = parent
	}
	return root, nil
}

// scan pages a listing until visit reports a match or the listing ends.
// An error means the listing was not read to its end, so absence is not
// proven.
func (o *Outbox) scan(ctx context.Context, path string, query url.Values, visit func(json.RawMessage) (bool, error)) error {
	query.Set("per_page", strconv.Itoa(scanPageSize))
	request := model.GHRequest{Method: http.MethodGet, Path: path, Query: query}
	for range maxScanPages {
		response, err := o.do(ctx, request)
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
