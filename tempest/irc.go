package tempest

import (
	"context"
	"fmt"

	"go.harness.dev/harness/internal/irc"
	"go.harness.dev/harness/internal/work"
)

// openMessaging creates the session tree's message bus and registers the
// root's durable queue as the "parent" mailbox. Only a root that can start
// task children has peers, so only it gets the bus.
func (c *sessionCore) openMessaging(cfg *StackConfig) {
	if !cfg.EnableTask || cfg.Plan {
		return
	}
	c.irc = irc.New(c.publishWork, c.describeAgent)
	cfg.Messenger = c.irc.Register(irc.Parent, c.queues)
}

// describeAgent explains why a job id cannot receive messages.
func (c *sessionCore) describeAgent(id string) string {
	if c.jobs == nil {
		return ""
	}
	for _, job := range c.jobs.Snapshot() {
		if job.ID != id {
			continue
		}
		if job.Kind != "task" {
			return fmt.Sprintf("%s is a %s job; only task children receive messages", id, job.Kind)
		}
		if terminal := job.State == "completed" || job.State == "failed" || job.State == "cancelled" || job.State == "unknown"; terminal {
			return fmt.Sprintf("%s is %s and no longer receives messages; its result arrives as a completion notice", id, job.State)
		}
		return fmt.Sprintf("%s is %s and has not opened its mailbox yet; retry shortly", id, job.State)
	}
	return ""
}

// childMessaging is a task child's run hook: at its last step boundary a
// pending message gives it another turn instead of ending the task, and a
// child with nothing pending seals its mailbox so later sends fail cleanly.
type childMessaging struct{ endpoint *irc.Endpoint }

func (hooks childMessaging) Pending() bool                             { return false }
func (hooks childMessaging) Start(context.Context, work.RunInfo) error { return nil }
func (hooks childMessaging) Idle(context.Context) (bool, error)        { return hooks.endpoint.Idle(), nil }
func (hooks childMessaging) Finish(context.Context, string) error {
	hooks.endpoint.Close()
	return nil
}
