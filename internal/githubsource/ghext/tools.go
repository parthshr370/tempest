package ghext

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/tempest/extension"
)

// settlePoll is how often a waiting write tool rereads its intent.
const settlePoll = 200 * time.Millisecond

type tools struct {
	gateway model.WorkerGateway
	wake    func()
	wait    time.Duration
	git     git
}

var (
	reportSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["status","summary"],"properties":{` +
		`"status":{"type":"string","enum":["progress","waiting","ready","blocked","failed","dropped"]},` +
		`"summary":{"type":"string","description":"What was done and what is next, for the humans on the thread."},` +
		`"question":{"type":"object","additionalProperties":false,"required":["text"],"description":"With blocked: what a human must answer.","properties":{"text":{"type":"string"},"options":{"type":"array","items":{"type":"string"}}}}}}`)
	replySchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["body"],"properties":{` +
		`"body":{"type":"string","description":"Markdown comment text."},` +
		`"in_reply_to":{"type":"integer","description":"ID of an inline review comment to answer in its thread."},` +
		`"number":{"type":"integer","description":"Issue or pull request number: the ticket's thread (default) or one of its pull requests."}}}`)
	emptySchema  = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`)
	openPRSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["title"],"properties":{` +
		`"title":{"type":"string"},"body":{"type":"string","description":"Markdown description."},"draft":{"type":"boolean"}}}`)
	threadSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{` +
		`"number":{"type":"integer","description":"The ticket's thread (default) or one of its pull requests."},` +
		`"cursor":{"type":"string","description":"nextCursor of the previous page."},` +
		`"limit":{"type":"integer","minimum":1,"maximum":100}}}`)
)

func (t *tools) definitions() []extension.Tool {
	return []extension.Tool{
		{Name: model.ToolReport, Description: "Report the ticket's status to the controller and the humans following it.", InputSchema: reportSchema, Effect: extension.EffectWrite, Replay: extension.ReplaySafe, Execute: t.report},
		{Name: model.ToolReply, Description: "Comment on the ticket's GitHub thread or one of its pull requests, or reply in an inline review thread. The controller posts it; the result says whether it is posted yet.", InputSchema: replySchema, Effect: extension.EffectWrite, Replay: extension.ReplaySafe, Execute: t.reply},
		{Name: model.ToolPush, Description: "Push the worktree's committed HEAD to the ticket's branch on GitHub. The controller pushes it; the result says whether it landed yet.", InputSchema: emptySchema, Effect: extension.EffectWrite, Replay: extension.ReplayNever, Execute: t.push},
		{Name: model.ToolOpenPR, Description: "Push the committed HEAD if needed, then open the ticket's pull request against the base branch.", InputSchema: openPRSchema, Effect: extension.EffectWrite, Replay: extension.ReplayNever, Execute: t.openPR},
		{Name: model.ToolStatus, Description: "The ticket's state: pull requests with head, CI and reviews pinned to commits, your writes and their states, budget left and inputs not yet delivered.", InputSchema: emptySchema, Effect: extension.EffectRead, Replay: extension.ReplaySafe, Execute: t.status},
		{Name: model.ToolThread, Description: "Page the stored events of the ticket's thread or one of its pull requests, oldest first.", InputSchema: threadSchema, Effect: extension.EffectRead, Replay: extension.ReplaySafe, Execute: t.thread},
	}
}

// call is the worker identity of a tool call: the session, whose binding
// names the Ticket, and the call ID that keys idempotency.
func call(c extension.Call, invocation extension.Invocation) model.WorkerCall {
	return model.WorkerCall{SessionID: invocation.Identity().SessionID, ToolCallID: c.ID}
}

// refuse is a tool error the model reads and can act on.
func refuse(err error) (extension.Result, error) {
	return extension.Result{Content: []extension.Content{{Type: "text", Text: err.Error()}}, IsError: true}, nil
}

func textResult(text string, structured any) (extension.Result, error) {
	result := extension.Result{Content: []extension.Content{{Type: "text", Text: text}}}
	if structured != nil {
		raw, err := json.Marshal(structured)
		if err != nil {
			return extension.Result{}, err
		}
		result.StructuredContent = raw
	}
	return result, nil
}

func decodeArguments(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func (t *tools) report(ctx context.Context, c extension.Call, invocation extension.Invocation) (extension.Result, error) {
	var request model.ReportRequest
	if err := decodeArguments(c.Arguments, &request); err != nil {
		return refuse(err)
	}
	ticket, err := t.gateway.Report(ctx, call(c, invocation), request)
	if err != nil {
		return refuse(err)
	}
	text := fmt.Sprintf("Recorded %s for ticket %s.", request.Status, ticket.ID)
	if ticket.Lifecycle == model.LifecycleEnded {
		text += fmt.Sprintf(" The ticket ended: %s.", ticket.Outcome)
	}
	return textResult(text, map[string]any{"ticket": ticket.ID, "status": request.Status, "lifecycle": ticket.Lifecycle, "hold": ticket.Hold})
}

func (t *tools) reply(ctx context.Context, c extension.Call, invocation extension.Invocation) (extension.Result, error) {
	var input struct {
		Body      string `json:"body"`
		InReplyTo int64  `json:"in_reply_to"`
		Number    int    `json:"number"`
	}
	if err := decodeArguments(c.Arguments, &input); err != nil {
		return refuse(err)
	}
	request := model.EffectRequest{Kind: model.EffectComment, Payload: model.EffectPayload{Number: input.Number, Body: input.Body}}
	if input.InReplyTo != 0 {
		request.Kind, request.Payload.ReplyTo = model.EffectReviewReply, input.InReplyTo
	}
	intent, err := t.enqueue(ctx, call(c, invocation), request)
	if err != nil {
		return refuse(err)
	}
	return t.settled(ctx, invocation.Identity().SessionID, intent)
}

// head is the worktree's committed HEAD on the Ticket's branch, resolved
// from the binding's Ticket, never from arguments.
func (t *tools) head(ctx context.Context, sessionID string) (model.TicketDetail, string, error) {
	detail, err := t.gateway.Status(ctx, sessionID)
	if err != nil {
		return model.TicketDetail{}, "", err
	}
	ticket := detail.Ticket
	if ticket.Worktree == "" {
		return model.TicketDetail{}, "", fmt.Errorf("ticket %s has no worktree yet", ticket.ID)
	}
	sha, err := t.git.committedHead(ctx, ticket.Worktree, ticket.Branch)
	if err != nil {
		return model.TicketDetail{}, "", err
	}
	return detail, sha, nil
}

// lastPush is the newest push that is pending, in flight or confirmed:
// what the branch will hold once the outbox catches up.
func lastPush(detail model.TicketDetail) *model.OutboxIntent {
	var latest *model.OutboxIntent
	for i := range detail.Outbox {
		intent := &detail.Outbox[i]
		if intent.Kind != model.EffectPush {
			continue
		}
		switch intent.State {
		case model.EffectPending, model.EffectAttempting, model.EffectConfirmed:
			if latest == nil || intent.Seq > latest.Seq {
				latest = intent
			}
		}
	}
	return latest
}

func (t *tools) push(ctx context.Context, c extension.Call, invocation extension.Invocation) (extension.Result, error) {
	sessionID := invocation.Identity().SessionID
	detail, sha, err := t.head(ctx, sessionID)
	if err != nil {
		return refuse(err)
	}
	if last := lastPush(detail); last != nil && last.Payload.SHA == sha {
		return t.settled(ctx, sessionID, *last)
	}
	intent, err := t.enqueue(ctx, call(c, invocation), model.EffectRequest{Kind: model.EffectPush, Payload: model.EffectPayload{Branch: detail.Ticket.Branch, SHA: sha}})
	if err != nil {
		return refuse(err)
	}
	return t.settled(ctx, sessionID, intent)
}

func (t *tools) openPR(ctx context.Context, c extension.Call, invocation extension.Invocation) (extension.Result, error) {
	var input struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Draft *bool  `json:"draft"`
	}
	if err := decodeArguments(c.Arguments, &input); err != nil {
		return refuse(err)
	}
	sessionID := invocation.Identity().SessionID
	detail, sha, err := t.head(ctx, sessionID)
	if err != nil {
		return refuse(err)
	}
	for _, link := range detail.Ticket.PRs {
		if link.Own && !link.Closed {
			return refuse(fmt.Errorf("pull request #%d of this ticket is open; push to update it instead", link.Number))
		}
	}
	worker := call(c, invocation)
	if last := lastPush(detail); last == nil || last.Payload.SHA != sha {
		push := worker
		push.ToolCallID = c.ID + "/push"
		if _, err := t.enqueue(ctx, push, model.EffectRequest{Kind: model.EffectPush, Payload: model.EffectPayload{Branch: detail.Ticket.Branch, SHA: sha}}); err != nil {
			return refuse(err)
		}
	}
	intent, err := t.enqueue(ctx, worker, model.EffectRequest{Kind: model.EffectOpenPR, Payload: model.EffectPayload{
		Branch: detail.Ticket.Branch, SHA: sha, Title: input.Title, Body: input.Body, Draft: input.Draft,
	}})
	if err != nil {
		return refuse(err)
	}
	return t.settled(ctx, sessionID, intent)
}

func (t *tools) enqueue(ctx context.Context, worker model.WorkerCall, request model.EffectRequest) (model.OutboxIntent, error) {
	intent, err := t.gateway.Enqueue(ctx, worker, request)
	if err != nil {
		return model.OutboxIntent{}, err
	}
	if t.wake != nil {
		t.wake()
	}
	return intent, nil
}

func final(state model.EffectState) bool {
	switch state {
	case model.EffectConfirmed, model.EffectFailed, model.EffectUnknown, model.EffectCancelled:
		return true
	}
	return false
}

// settled waits up to the settle bound for the outbox, then describes the
// intent as it stands.
func (t *tools) settled(ctx context.Context, sessionID string, intent model.OutboxIntent) (extension.Result, error) {
	deadline := time.Now().Add(t.wait)
	for !final(intent.State) && time.Now().Before(deadline) {
		timer := time.NewTimer(min(settlePoll, time.Until(deadline)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return extension.Result{}, ctx.Err()
		case <-timer.C:
		}
		current, err := t.gateway.Effect(ctx, sessionID, intent.ID)
		if err != nil {
			return refuse(err)
		}
		intent = current
	}
	view := effectOf(intent)
	result, err := textResult(describeEffect(view), view)
	if err == nil && (intent.State == model.EffectFailed || intent.State == model.EffectUnknown) {
		result.IsError = true
	}
	return result, err
}

// effectView is what a worker sees of one write.
type effectView struct {
	ID        model.OutboxID    `json:"id"`
	Kind      model.EffectKind  `json:"kind"`
	State     model.EffectState `json:"state"`
	Number    int               `json:"number,omitempty"`
	SHA       string            `json:"sha,omitempty"`
	Attempts  int               `json:"attempts,omitempty"`
	RemoteID  int64             `json:"remoteId,omitempty"`
	URL       string            `json:"url,omitempty"`
	LastError string            `json:"lastError,omitempty"`
}

func effectOf(intent model.OutboxIntent) effectView {
	view := effectView{ID: intent.ID, Kind: intent.Kind, State: intent.State, Number: intent.Payload.Number, SHA: intent.Payload.SHA, Attempts: intent.Attempts, LastError: intent.LastError}
	if result := intent.Result; result != nil {
		view.RemoteID, view.URL = result.RemoteID, result.URL
		if result.SHA != "" {
			view.SHA = result.SHA
		}
	}
	return view
}

func describeEffect(view effectView) string {
	var text strings.Builder
	fmt.Fprintf(&text, "%s %s: %s", view.Kind, view.ID, view.State)
	switch view.State {
	case model.EffectPending, model.EffectAttempting:
		text.WriteString(" (the controller publishes it; check later with " + tool(model.ToolStatus) + ")")
	case model.EffectUnknown:
		text.WriteString(" (the controller cannot tell whether it happened; do not repeat it, an operator decides)")
	}
	if view.SHA != "" {
		fmt.Fprintf(&text, "\ncommit: %s", view.SHA)
	}
	if view.URL != "" {
		fmt.Fprintf(&text, "\nurl: %s", view.URL)
	}
	if view.LastError != "" {
		fmt.Fprintf(&text, "\nlast error: %s", view.LastError)
	}
	return text.String()
}

func (t *tools) status(ctx context.Context, c extension.Call, invocation extension.Invocation) (extension.Result, error) {
	detail, err := t.gateway.Status(ctx, invocation.Identity().SessionID)
	if err != nil {
		return refuse(err)
	}
	view := statusOf(detail)
	raw, err := json.Marshal(view)
	if err != nil {
		return extension.Result{}, err
	}
	return extension.Result{Content: []extension.Content{{Type: "text", Text: string(raw)}}, StructuredContent: raw}, nil
}

func (t *tools) thread(ctx context.Context, c extension.Call, invocation extension.Invocation) (extension.Result, error) {
	var query model.ThreadQuery
	if err := decodeArguments(c.Arguments, &query); err != nil {
		return refuse(err)
	}
	page, err := t.gateway.Thread(ctx, invocation.Identity().SessionID, query)
	if err != nil {
		return refuse(err)
	}
	return textResult(renderThread(query, page), map[string]any{"events": len(page.Events), "nextCursor": page.NextCursor})
}
