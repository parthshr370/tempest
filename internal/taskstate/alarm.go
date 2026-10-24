package taskstate

import (
	"time"

	"go.harness.dev/harness/tempest/extension"
)

// Alarm reduces task deadlines and immediately schedulable control transitions.
// Callers mark live invocations active so observation doesn't reschedule them.
func Alarm(tasks []extension.Task, now time.Time) (time.Time, bool) {
	byID := make(map[string]extension.Task, len(tasks))
	for _, task := range tasks {
		byID[task.ID] = task
	}
	var earliest time.Time
	for _, task := range tasks {
		var candidate *time.Time
		switch task.Status {
		case "sleeping":
			candidate = task.WakeAt
		case "runnable", "suspended", "running", "aborting":
			candidate = &now
		case "waiting":
			settled := true
			rejected := false
			for _, id := range task.Wait {
				dependency, exists := byID[id]
				settled = settled && exists && Terminal(dependency)
				rejected = rejected || exists && failed(dependency)
			}
			if settled || task.FailFast && rejected {
				candidate = &now
			}
		case "completing", "abort_requested":
			settled := true
			propagate := false
			for _, child := range tasks {
				if child.Owner == task.ID && !Terminal(child) {
					settled = false
					propagate = propagate || task.Status == "abort_requested" && child.AbortReason == ""
				}
			}
			if settled || propagate {
				candidate = &now
			}
		}
		if candidate != nil && (earliest.IsZero() || candidate.Before(earliest)) {
			earliest = *candidate
		}
	}
	return earliest, !earliest.IsZero()
}
