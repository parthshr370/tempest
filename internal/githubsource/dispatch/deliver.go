package dispatch

import (
	"context"
	"errors"

	"go.harness.dev/harness/internal/githubsource/model"
	"go.harness.dev/harness/tempest"
)

// queuedInput is one Input rendered for a worker's steering queue.
type queuedInput struct {
	seq   uint64
	input tempest.QueuedInput
}

// deliver steers Inputs that arrived after a run was submitted into the
// running worker, one keyed queue item per Input, and records how far the
// session accepted them. A queue receipt means accepted, not read: what a
// run consumed is read from the receipts when the dispatch settles.
func (d *Dispatcher) deliver(ctx context.Context) {
	deliveries, err := d.store.Deliveries(ctx)
	if err != nil {
		d.log.Warn("github dispatch: list deliveries", "error", err)
		return
	}
	for _, delivery := range deliveries {
		execution := d.runningExecution(delivery.Ticket, delivery.Gen)
		if execution == nil {
			continue
		}
		items := make([]queuedInput, 0, len(delivery.Inputs))
		for _, view := range delivery.Inputs {
			key := model.DeliveryRequestID(delivery.Ticket, view.Input.Seq, delivery.Gen)
			items = append(items, queuedInput{seq: view.Input.Seq, input: tempest.QueuedInput{Text: d.renderDelivery(delivery.Ticket, view), Key: key}})
		}
		through, err := execution.steer(ctx, items)
		if err != nil {
			d.log.Warn("github dispatch: steer input into worker", "ticket", delivery.Ticket, "deliveredThrough", through, "error", err)
		}
		if through == 0 {
			continue
		}
		if err := d.store.Delivered(ctx, delivery.Ticket, delivery.Gen, through); err != nil {
			d.log.Warn("github dispatch: record delivery", "ticket", delivery.Ticket, "through", through, "error", err)
		}
	}
}

// runningExecution finds the executor of the Ticket's generation.
func (d *Dispatcher) runningExecution(ticket model.TicketID, gen uint32) *execution {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, execution := range d.executions {
		if execution.ticket == ticket && execution.gen == gen {
			return execution
		}
	}
	return nil
}

// steer queues items in order while the execution accepts deliveries and
// returns the last seq the session accepted. A full queue stops delivery
// without error: the rest wait for a later pass or the next dispatch.
func (e *execution) steer(ctx context.Context, items []queuedInput) (uint64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.accepting || e.session == nil {
		return 0, nil
	}
	var through uint64
	for _, item := range items {
		if _, err := e.session.Steer(ctx, item.input); err != nil {
			if errors.Is(err, tempest.ErrQueueFull) {
				return through, nil
			}
			return through, err
		}
		through = item.seq
	}
	return through, nil
}
