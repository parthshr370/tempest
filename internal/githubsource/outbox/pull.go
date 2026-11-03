package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"go.harness.dev/harness/internal/githubsource/model"
)

// pull is the part of a pull request the outbox reads.
type pull struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	User    struct {
		ID int64 `json:"id"`
	} `json:"user"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p pull) reply(reconciled bool) *model.EffectReply {
	return &model.EffectReply{RemoteID: int64(p.Number), URL: p.HTMLURL, SHA: p.Head.SHA, Reconciled: reconciled}
}

// ours reports a PR the account opened from the Ticket's branch in the
// repository itself, against the intent's base.
func (o *Outbox) ours(p pull, intent model.OutboxIntent) bool {
	return p.User.ID == o.account && p.Head.Ref == intent.Payload.Branch && p.Head.Repo != nil &&
		strings.EqualFold(p.Head.Repo.FullName, o.repo) && p.Base.Ref == intent.Payload.Base
}

// attemptOpenPR opens the PR once the branch holds the commit the worker
// asked for: a PR is never opened over a push that failed or that someone
// else replaced.
func (o *Outbox) attemptOpenPR(ctx context.Context, intent model.OutboxIntent) step {
	payload := intent.Payload
	if payload.Branch != model.TicketBranch(intent.Ticket) || payload.Base == "" {
		return failed(fmt.Errorf("pull request from %q to %q is not the ticket's branch", payload.Branch, payload.Base))
	}
	sha, exists, err := o.branchHead(ctx, payload.Branch)
	switch {
	case err != nil:
		return o.retry(intent, fmt.Errorf("check branch before opening the pull request: %w", err), retryAfter(err))
	case !exists:
		return failed(fmt.Errorf("branch %s is not on GitHub; push it before opening a pull request", payload.Branch))
	case payload.SHA != "" && sha != payload.SHA:
		return failed(fmt.Errorf("branch %s is at %s on GitHub, not the requested %s; push again before opening a pull request", payload.Branch, sha, payload.SHA))
	}
	request := map[string]any{"title": payload.Title, "body": marked(payload.Body, intent.ID), "head": payload.Branch, "base": payload.Base}
	if payload.Draft != nil {
		request["draft"] = *payload.Draft
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return failed(err)
	}
	path := fmt.Sprintf("repos/%s/pulls", o.repo)
	response, err := o.do(ctx, model.GHRequest{Method: http.MethodPost, Path: path, Body: raw})
	if err == nil {
		var opened pull
		if json.Unmarshal(response.Body, &opened) != nil || opened.Number == 0 {
			err = &model.GHError{Message: fmt.Sprintf("POST %s: unreadable HTTP %d reply", path, response.Status)}
		} else {
			return confirmed(opened.reply(false))
		}
	}
	if status(err) == http.StatusUnprocessableEntity {
		// GitHub keeps one open PR per head and base: an earlier attempt
		// or an earlier open_pr of this Ticket may already have opened it.
		return o.existingPR(ctx, intent, err)
	}
	verdict, wait := classify(err)
	switch verdict {
	case refusedRetry:
		return o.retry(intent, err, wait)
	case refused:
		return failed(err)
	}
	if ctx.Err() != nil {
		return unread(ctx.Err())
	}
	return o.searchPR(ctx, intent, err)
}

// existingPR settles a 422: this intent's PR (by marker) or the open PR
// the account already has from the branch confirms it; anything else is
// GitHub's validation failure.
func (o *Outbox) existingPR(ctx context.Context, intent model.OutboxIntent, refusal error) step {
	var marked, open *pull
	err := o.scanPulls(ctx, intent, func(p pull) bool {
		switch {
		case carries(p.Body, intent.ID):
			marked = &p
			return true
		case p.State == "open" && open == nil:
			open = &p
		}
		return false
	})
	switch {
	case err != nil:
		return unread(fmt.Errorf("after %w: %w", refusal, err))
	case marked != nil:
		return confirmed(marked.reply(true))
	case open != nil:
		return confirmed(open.reply(true))
	}
	return failed(refusal)
}

// searchPR looks for the PR whose open may have reached GitHub by its
// marker. Not found is read again later, then unknown, never pending: a
// human may have rewritten the body of the very PR the attempt opened. The
// account's other PRs from the branch are named for the operator.
func (o *Outbox) searchPR(ctx context.Context, intent model.OutboxIntent, cause error) step {
	var found *pull
	var others []string
	err := o.scanPulls(ctx, intent, func(p pull) bool {
		if carries(p.Body, intent.ID) {
			found = &p
			return true
		}
		others = append(others, fmt.Sprintf("#%d (%s)", p.Number, p.State))
		return false
	})
	switch {
	case err != nil:
		return unread(err)
	case found != nil:
		return confirmed(found.reply(true))
	case len(others) > 0:
		cause = fmt.Errorf("%w; the account's pull requests from %s without the marker: %s", cause, intent.Payload.Branch, strings.Join(others, ", "))
	}
	return unsure("the pull request", cause)
}

// scanPulls pages every PR from the Ticket's branch to the intent's base,
// open or closed, and visits the account's own until visit returns true.
func (o *Outbox) scanPulls(ctx context.Context, intent model.OutboxIntent, visit func(pull) bool) error {
	query := url.Values{"state": {"all"}, "head": {o.owner + ":" + intent.Payload.Branch}, "base": {intent.Payload.Base}}
	return o.scan(ctx, fmt.Sprintf("repos/%s/pulls", o.repo), query, func(raw json.RawMessage) (bool, error) {
		var rows []pull
		if err := json.Unmarshal(raw, &rows); err != nil {
			return false, err
		}
		for _, row := range rows {
			if o.ours(row, intent) && visit(row) {
				return true, nil
			}
		}
		return false, nil
	})
}

// attemptUpdatePR edits an own PR's title or body. The edit sets values, so
// it is idempotent: reconciliation only reads whether they are in place.
func (o *Outbox) attemptUpdatePR(ctx context.Context, intent model.OutboxIntent) step {
	payload := intent.Payload
	if payload.Draft != nil {
		return failed(errors.New("the REST API cannot change a pull request's draft state"))
	}
	request := map[string]string{}
	if payload.Title != "" {
		request["title"] = payload.Title
	}
	if payload.Body != "" {
		request["body"] = payload.Body
	}
	if len(request) == 0 {
		return failed(errors.New("the update changes nothing"))
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return failed(err)
	}
	path := fmt.Sprintf("repos/%s/pulls/%d", o.repo, payload.Number)
	response, err := o.do(ctx, model.GHRequest{Method: http.MethodPatch, Path: path, Body: raw})
	verdict, wait := classify(err)
	switch verdict {
	case sent:
		var updated pull
		if json.Unmarshal(response.Body, &updated) == nil && updated.Number != 0 {
			return confirmed(updated.reply(false))
		}
	case refusedRetry:
		return o.retry(intent, err, wait)
	case refused:
		return failed(err)
	}
	if ctx.Err() != nil {
		return unread(ctx.Err())
	}
	return o.reconcileUpdatePR(ctx, intent)
}

// reconcileUpdatePR reads the PR: the requested title and body in place
// confirm the edit, otherwise it is proven not applied.
func (o *Outbox) reconcileUpdatePR(ctx context.Context, intent model.OutboxIntent) step {
	payload := intent.Payload
	response, err := o.do(ctx, model.GHRequest{Method: http.MethodGet, Path: fmt.Sprintf("repos/%s/pulls/%d", o.repo, payload.Number)})
	if err != nil {
		return unread(fmt.Errorf("read pull request #%d: %w", payload.Number, err))
	}
	var current pull
	if err := json.Unmarshal(response.Body, &current); err != nil || current.Number == 0 {
		return unread(fmt.Errorf("read pull request #%d: unreadable reply", payload.Number))
	}
	if (payload.Title == "" || current.Title == payload.Title) && (payload.Body == "" || current.Body == payload.Body) {
		return confirmed(current.reply(true))
	}
	return o.retry(intent, errors.New("reconciled: the pull request was not updated"), 0)
}
