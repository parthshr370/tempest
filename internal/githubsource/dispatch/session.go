package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf8"

	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/internal/worktree"
	"go.harness.dev/harness/tempest"
	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

// listPageLimit is the page size of worker-session catalog reads.
const listPageLimit = 100

// workerSession returns the Ticket's one worker session with its
// WorkerTicket document committed, binding it in control first if it is
// new. A Ticket bound before reuses its session. Otherwise the sessions the
// Host lists in the Ticket's worktree are examined: one already carrying
// this Ticket's document is adopted; else the oldest never-run session
// without a document (a create whose document commit was lost) is adopted
// and given the document; other never-run, document-less sessions there are
// retired. Only when none exists is a session created. The worktree path is
// unique to the Ticket, so adoption cannot take another Ticket's session.
func (d *Dispatcher) workerSession(ctx context.Context, admitted model.AdmittedDispatch, checkout worktree.BranchCheckout) (*tempest.Session, error) {
	document, err := d.workerDocument(ctx, admitted, checkout)
	if err != nil {
		return nil, err
	}
	if admitted.Ticket.SessionID != "" {
		session, err := d.host.OpenWithOptions(ctx, admitted.Ticket.SessionID, WorkerResume())
		if err != nil {
			return nil, transientFailure("open worker session "+admitted.Ticket.SessionID, err)
		}
		if err := writeDocument(ctx, session, document); err != nil {
			return nil, err
		}
		return session, nil
	}
	session, err := d.adoptOrCreate(ctx, checkout.Path, document)
	if err != nil {
		return nil, err
	}
	binding := model.SessionBinding{SessionID: session.ID(), Ticket: admitted.Ticket.ID, Worktree: checkout.Path, BoundAt: d.clock.Now()}
	if err := d.store.Bind(ctx, binding); err != nil {
		if isConflict(err) {
			return nil, permanentFailure("bind worker session", err)
		}
		return nil, err
	}
	return session, nil
}

// adoptOrCreate finds or creates the worker session in cwd and commits its
// document before it is bound or run.
func (d *Dispatcher) adoptOrCreate(ctx context.Context, cwd string, document model.WorkerTicket) (*tempest.Session, error) {
	candidates, err := d.unstartedSessions(ctx, cwd)
	if err != nil {
		return nil, err
	}
	var chosen *tempest.Session
	var orphans []string
	for _, candidate := range candidates {
		session, err := d.host.OpenWithOptions(ctx, candidate.SessionID, WorkerResume())
		if err != nil {
			d.log.Warn("github dispatch: skip unopenable session in ticket worktree", "session", candidate.SessionID, "error", err)
			continue
		}
		owner, granted, err := documentOwner(ctx, session)
		if err != nil {
			return nil, transientFailure("read worker document", err)
		}
		switch {
		case !granted:
			// Not created by the dispatcher: it lacks the github bundle.
		case owner == document.Ticket && chosen == nil:
			chosen = session
		case owner == "":
			orphans = append(orphans, session.ID())
		}
	}
	if chosen == nil && len(orphans) > 0 {
		session, err := d.host.OpenWithOptions(ctx, orphans[0], WorkerResume())
		if err != nil {
			return nil, transientFailure("open orphan worker session", err)
		}
		chosen, orphans = session, orphans[1:]
	}
	if chosen == nil {
		created, err := d.createSession(ctx, cwd)
		if err != nil {
			return nil, err
		}
		chosen = created
		if err := d.point("created"); err != nil {
			return nil, err
		}
	}
	if err := writeDocument(ctx, chosen, document); err != nil {
		return nil, err
	}
	if err := d.point("document"); err != nil {
		return nil, err
	}
	for _, orphan := range orphans {
		if err := d.host.CloseSession(ctx, orphan); err != nil {
			d.log.Warn("github dispatch: close retired worker session", "session", orphan, "error", err)
			continue
		}
		d.log.Info("github dispatch: retired unstarted duplicate worker session", "session", orphan, "kept", chosen.ID())
	}
	return chosen, nil
}

// unstartedSessions lists the published sessions in cwd that never ran,
// oldest first. A session the dispatcher created is bound before its first
// run, so an unbound worker session has no run.
func (d *Dispatcher) unstartedSessions(ctx context.Context, cwd string) ([]tempest.SessionInfo, error) {
	var sessions []tempest.SessionInfo
	cursor := ""
	for {
		page, err := d.host.List(ctx, tempest.ListOptions{Cwd: cwd, Cursor: cursor, Limit: listPageLimit})
		if err != nil {
			return nil, transientFailure("list worker sessions", err)
		}
		for _, info := range page.Sessions {
			if info.LastRun == nil {
				sessions = append(sessions, info)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	sort.Slice(sessions, func(i, j int) bool {
		if !sessions[i].UpdatedAt.Equal(sessions[j].UpdatedAt) {
			return sessions[i].UpdatedAt.Before(sessions[j].UpdatedAt)
		}
		return sessions[i].SessionID < sessions[j].SessionID
	})
	return sessions, nil
}

// WorkerResume reopens a worker session as createSession made it: the
// model and permission mode come from its journal, and it has no MCP
// servers. Every opener of a worker uses it, so a live worker is the same
// whoever opened it first.
func WorkerResume() tempest.ResumeSession {
	return tempest.ResumeSession{MCPServers: []tempest.MCPServerConfig{}}
}

func (d *Dispatcher) createSession(ctx context.Context, cwd string) (*tempest.Session, error) {
	workers := d.config.Workers
	session, err := d.host.Create(ctx, tempest.AgentOptions{
		Cwd: cwd,
		New: &tempest.NewSession{
			Model:          workers.Model,
			PermissionMode: tempest.PermissionMode(workers.PermissionMode),
			MCPServers:     []tempest.MCPServerConfig{},
		},
		Extensions: []extension.Selection{{ID: model.ExtensionID, Required: true}},
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, transientFailure("create worker session", err)
	}
	return session, nil
}

// documentPageLimit covers the namespace's records: the definition's
// initial "default" record and the WorkerTicket record.
const documentPageLimit = 10

// readDocument returns the session's WorkerTicket record. A session whose
// github bundle granted the namespace holds its initial "default" record;
// granted is false without it. Before the first write the WorkerTicket
// record is absent: it is returned as the definition's initial value at
// revision zero, which a commit's revision check reads as "still absent".
func readDocument(ctx context.Context, session *tempest.Session) (durable.Record, bool, error) {
	page, err := session.Read(ctx, durable.Query{Kind: durable.DocumentKind, Namespace: model.WorkerNamespace, Limit: documentPageLimit})
	if err != nil || len(page.Records) == 0 {
		return durable.Record{}, false, err
	}
	for _, record := range page.Records {
		if record.ID == model.WorkerDocument {
			return record, true, nil
		}
	}
	return durable.Record{Change: durable.Change{Value: model.WorkerDocumentDefinition().Initial}}, true, nil
}

func documentOwner(ctx context.Context, session *tempest.Session) (model.TicketID, bool, error) {
	record, granted, err := readDocument(ctx, session)
	if err != nil || !granted {
		return "", granted, err
	}
	var current model.WorkerTicket
	if err := json.Unmarshal(record.Value, &current); err != nil {
		return "", true, fmt.Errorf("worker document of session %s: %w", session.ID(), err)
	}
	return current.Ticket, true, nil
}

// writeDocument commits the Ticket's identity document unless the session
// already carries it. The document is written once and never changed, so
// the prompt sections rendered from it stay byte-identical.
func writeDocument(ctx context.Context, session *tempest.Session, document model.WorkerTicket) error {
	record, granted, err := readDocument(ctx, session)
	if err != nil {
		return transientFailure("read worker document", err)
	}
	if !granted {
		return permanentFailure(fmt.Sprintf("worker session %s lacks the %s extension document", session.ID(), model.ExtensionID), nil)
	}
	var current model.WorkerTicket
	if err := json.Unmarshal(record.Value, &current); err != nil {
		return permanentFailure("worker document of session "+session.ID(), err)
	}
	switch current.Ticket {
	case document.Ticket:
		return nil
	case "":
	default:
		return permanentFailure(fmt.Sprintf("worker session %s belongs to ticket %s", session.ID(), current.Ticket), nil)
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return err
	}
	commit := durable.Commit{
		ID:       "bind:" + string(document.Ticket),
		Expected: []durable.RevisionCheck{{Kind: durable.DocumentKind, Namespace: model.WorkerNamespace, ID: model.WorkerDocument, Revision: record.Revision}},
		Changes:  []durable.Change{{Kind: durable.DocumentKind, Namespace: model.WorkerNamespace, ID: model.WorkerDocument, Version: model.WorkerDocumentDefinition().Version, Value: raw}},
	}
	if _, err := session.Commit(ctx, commit); err != nil {
		return transientFailure("commit worker document", err)
	}
	return nil
}

// workerDocument builds the Ticket's identity document: the facts its
// worker's system prompt sections render, fixed at first write.
func (d *Dispatcher) workerDocument(ctx context.Context, admitted model.AdmittedDispatch, checkout worktree.BranchCheckout) (model.WorkerTicket, error) {
	ticket := admitted.Ticket
	first, err := d.store.Inputs(ctx, ticket.ID, 0, 1)
	if err != nil {
		return model.WorkerTicket{}, err
	}
	if len(first) == 0 {
		return model.WorkerTicket{}, permanentFailure(fmt.Sprintf("ticket %s has no request input", ticket.ID), nil)
	}
	return model.WorkerTicket{
		Ticket:   ticket.ID,
		Repo:     d.config.Repository,
		Thread:   ticket.Thread,
		Title:    ticket.Title,
		URL:      threadURL(d.config.Repository, ticket.Thread),
		Branch:   model.TicketBranch(ticket.ID),
		Base:     d.base,
		Worktree: checkout.Path,
		Mandate:  admitted.Mandate,
		Request:  truncateUTF8(first[0].Event.Body, d.wakeBytes),
		Team:     d.config.Prompts,
	}, nil
}

// threadURL is the GitHub page of an Issue or PR conversation.
func threadURL(repo string, thread model.ThreadRef) string {
	kind := "issues"
	if thread.IsPR {
		kind = "pull"
	}
	return fmt.Sprintf("https://github.com/%s/%s/%d", repo, kind, thread.Number)
}

// truncateUTF8 cuts text to at most limit bytes on a rune boundary.
func truncateUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
