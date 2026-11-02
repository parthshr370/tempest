package control

import (
	"encoding/json"
	"slices"
	"strings"

	"go.harness.dev/harness/internal/githubsource/model"
)

// feed calls subscribers after every durable commit.
type feed struct {
	next        uint64
	subscribers map[uint64]func(seq uint64, tickets []model.TicketID)
}

// Subscribe registers notify to run after every durable commit of the
// control journal (intake, dispatch, outbox, worker tool and operator
// commits), in commit order, with the commit's sequence number and the
// Tickets whose records it changed (possibly none). A call that commits
// nothing notifies nothing.
//
// notify runs with the Store's mutex held: it must not block and must not
// call the Store or the returned cancel. cancel stops later notifications.
func (s *Store) Subscribe(notify func(seq uint64, tickets []model.TicketID)) (cancel func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.feed.subscribers == nil {
		s.feed.subscribers = make(map[uint64]func(uint64, []model.TicketID))
	}
	s.feed.next++
	id := s.feed.next
	s.feed.subscribers[id] = notify
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.feed.subscribers, id)
	}
}

// publish notifies subscribers of a commit. Called with s.mu held.
func (s *Store) publish(seq uint64, perTable [][]staged) {
	if len(s.feed.subscribers) == 0 {
		return
	}
	tickets := changedTickets(perTable)
	ids := make([]uint64, 0, len(s.feed.subscribers))
	for id := range s.feed.subscribers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		s.feed.subscribers[id](seq, slices.Clone(tickets))
	}
}

// changedTickets names the Tickets a commit's records belong to, sorted.
// Ticket IDs contain no colon, so the Ticket of a record keyed
// "<TicketID>:<rest>" is the part before the first colon.
func changedTickets(perTable [][]staged) []model.TicketID {
	seen := make(map[model.TicketID]bool)
	for _, changes := range perTable {
		for _, c := range changes {
			var id string
			switch c.namespace {
			case model.NamespaceTicket, namespaceReport:
				id = c.id
			case model.NamespaceMandate, model.NamespaceInput, model.NamespaceAssignment, model.NamespaceDispatch, model.NamespaceOutbox:
				id, _, _ = strings.Cut(c.id, ":")
			case model.NamespaceLedger:
				id, _ = strings.CutPrefix(c.id, "ticket:")
				if id == c.id {
					continue
				}
			case model.NamespaceSession:
				var binding model.SessionBinding
				if json.Unmarshal(c.value, &binding) == nil {
					id = string(binding.Ticket)
				}
			}
			if id != "" {
				seen[model.TicketID(id)] = true
			}
		}
	}
	tickets := make([]model.TicketID, 0, len(seen))
	for id := range seen {
		tickets = append(tickets, id)
	}
	slices.Sort(tickets)
	return tickets
}
