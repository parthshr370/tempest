package acp

import (
	"context"

	"go.harness.dev/harness/internal/githubsource/model"
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
