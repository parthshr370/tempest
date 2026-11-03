package control

import (
	"strings"
	"time"

	"go.harness.dev/harness/internal/githubsource/model"
)

// subscriber is a Ticket subscribed to the event's thread, as admission
// sees it.
type subscriber struct {
	ID        model.TicketID
	Lifecycle model.Lifecycle
	Stopped   bool
	// EndedAt is when an ended Ticket ended.
	EndedAt time.Time
	// OwnHead is true for a pr_head event whose SHA this Ticket's outbox
	// pushed: the worker caused it and is not woken by it.
	OwnHead bool
}

// admissionView is everything a rule reads besides the event: the policy
// and the thread's subscribers, oldest Ticket first.
type admissionView struct {
	Policy      model.Policy
	Account     int64
	Mode        model.Mode
	Subscribers []subscriber
}

// action is what a verdict does to the Tickets it names.
type action int

const (
	actNone action = iota
	actRequest
	actReopen
	actInput
	actStop
	actStatus
)

// verdict is the outcome of the admission rules for one event.
type verdict struct {
	Rule    int
	Kind    model.DecisionKind
	Action  action
	Reason  string
	Class   model.InputClass
	Wakes   bool
	Tickets []model.TicketID
	// NoWake lists Tickets whose Input must not wake them (own pushes).
	NoWake map[model.TicketID]bool
}

func (v admissionView) listed(actor model.Actor) bool {
	for _, a := range v.Policy.Actors {
		if a.ID == actor.ID {
			return true
		}
	}
	return false
}

func (v admissionView) bot(actor model.Actor) bool {
	if actor.Type == "Bot" || strings.HasSuffix(strings.ToLower(actor.Login), "[bot]") {
		return true
	}
	for _, b := range v.Policy.Bots {
		if b.ID == actor.ID {
			return true
		}
	}
	return false
}

func (v admissionView) open() []model.TicketID {
	var ids []model.TicketID
	for _, sub := range v.Subscribers {
		if sub.Lifecycle == model.LifecycleOpen {
			ids = append(ids, sub.ID)
		}
	}
	return ids
}

func isHumanKind(kind model.EventKind) bool {
	switch kind {
	case model.EventIssueOpened, model.EventPROpened, model.EventComment, model.EventReview, model.EventReviewComment, model.EventReviewDismissed:
		return true
	}
	return false
}

func isEdit(kind model.EventKind) bool {
	return kind == model.EventCommentEdited || kind == model.EventReviewCommentEdited
}

// admit applies the section 5 rules in order; the first match wins. It is
// a pure function of the event and the view.
func admit(event model.Event, view admissionView) verdict {
	actor := event.Actor
	if actor.ID == view.Account {
		return verdict{Rule: 1, Kind: model.DecisionIgnored, Reason: "the account's own activity"}
	}
	listed := view.listed(actor) && !view.bot(actor)
	open := view.open()

	if event.Kind == model.EventComment && event.Command == model.CommandStop && listed {
		if len(open) == 0 {
			return verdict{Rule: 2, Kind: model.DecisionIgnored, Reason: "stop with no open ticket on the thread"}
		}
		return verdict{Rule: 2, Kind: model.DecisionStop, Action: actStop, Reason: "stop by " + actor.Login, Tickets: open}
	}
	if event.Kind == model.EventComment && event.Command == model.CommandStatus && listed {
		if len(view.Subscribers) == 0 {
			return verdict{Rule: 3, Kind: model.DecisionIgnored, Reason: "status with no ticket on the thread"}
		}
		newest := view.Subscribers[len(view.Subscribers)-1]
		if newest.Stopped {
			return verdict{Rule: 3, Kind: model.DecisionIgnored, Reason: "status for a stopped ticket"}
		}
		return verdict{Rule: 3, Kind: model.DecisionInput, Action: actStatus, Reason: "status reply queued", Tickets: []model.TicketID{newest.ID}}
	}
	if event.Mention && listed && (event.Kind == model.EventIssueOpened || event.Kind == model.EventPROpened || event.Kind == model.EventComment) {
		if len(open) > 0 {
			return verdict{Rule: 4, Kind: model.DecisionInput, Action: actInput, Reason: "authorized mention on an open ticket", Class: model.InputAuthoritative, Wakes: true, Tickets: open}
		}
		// With no open Ticket, a mention must be fresh: GitHub's time of the
		// event (Event.At, a comment's created/updated time) strictly after
		// the EndedAt of every ended Ticket on the thread. An older mention
		// discovered late was written while that work was open or before it
		// ended; it neither reopens it nor starts a new Ticket.
		for _, sub := range view.Subscribers {
			if sub.Lifecycle == model.LifecycleEnded && !event.At.After(sub.EndedAt) {
				return verdict{Rule: 4, Kind: model.DecisionIgnored, Reason: "mention at " + event.At.UTC().Format(time.RFC3339) + " predates the end of ticket " + string(sub.ID) + " at " + sub.EndedAt.UTC().Format(time.RFC3339)}
			}
		}
		for i := len(view.Subscribers) - 1; i >= 0; i-- {
			sub := view.Subscribers[i]
			if sub.Lifecycle == model.LifecycleEnded && !sub.Stopped {
				if event.Kind != model.EventComment {
					break
				}
				return verdict{Rule: 4, Kind: model.DecisionReopen, Action: actReopen, Reason: "authorized mention reopens an ended ticket", Class: model.InputAuthoritative, Wakes: true, Tickets: []model.TicketID{sub.ID}}
			}
		}
		return verdict{Rule: 4, Kind: model.DecisionRequest, Action: actRequest, Reason: "authorized mention by " + actor.Login, Class: model.InputAuthoritative, Wakes: true}
	}
	if listed && len(open) > 0 && (event.Kind == model.EventComment || event.Kind == model.EventReview || event.Kind == model.EventReviewComment || event.Kind == model.EventReviewDismissed) {
		return verdict{Rule: 5, Kind: model.DecisionInput, Action: actInput, Reason: "authorized follow-up", Class: model.InputAuthoritative, Wakes: true, Tickets: open}
	}
	if isEdit(event.Kind) && len(open) > 0 {
		return verdict{Rule: 6, Kind: model.DecisionInput, Action: actInput, Reason: "edit is context only", Class: model.InputContext, Tickets: open}
	}
	bot := view.bot(actor)
	if !bot && !listed && event.Mention && (isHumanKind(event.Kind) || isEdit(event.Kind)) {
		return verdict{Rule: 7, Kind: model.DecisionDenied, Reason: "mention by unlisted actor " + actor.Login}
	}
	if !bot && !listed && isHumanKind(event.Kind) && len(open) > 0 {
		return verdict{Rule: 8, Kind: model.DecisionInput, Action: actInput, Reason: "context from unlisted actor", Class: model.InputContext, Tickets: open}
	}
	if bot && len(open) > 0 && (event.Kind == model.EventComment || event.Kind == model.EventReview || event.Kind == model.EventReviewComment) {
		return verdict{Rule: 9, Kind: model.DecisionInput, Action: actInput, Reason: "bot input", Class: model.InputMachine, Wakes: true, Tickets: open}
	}
	if (event.Kind == model.EventPRHead || event.Kind == model.EventPRCI) && len(open) > 0 {
		v := verdict{Rule: 10, Kind: model.DecisionInput, Action: actInput, Reason: "pull request fact", Class: model.InputMachine, Wakes: true, Tickets: open}
		for _, sub := range view.Subscribers {
			if sub.OwnHead && event.Kind == model.EventPRHead {
				if v.NoWake == nil {
					v.NoWake = make(map[model.TicketID]bool)
				}
				v.NoWake[sub.ID] = true
			}
		}
		return v
	}
	if (event.Kind == model.EventPRClosed || event.Kind == model.EventPRMerged || event.Kind == model.EventPRReopened) && len(open) > 0 {
		return verdict{Rule: 11, Kind: model.DecisionInput, Action: actInput, Reason: "pull request state", Class: model.InputMachine, Tickets: open}
	}
	if view.Mode == model.ModeObserve {
		return verdict{Rule: 12, Kind: model.DecisionObserved, Reason: "recorded"}
	}
	return verdict{Rule: 12, Kind: model.DecisionIgnored, Reason: "no reader"}
}
