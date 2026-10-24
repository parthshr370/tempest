package hooks

import "context"

type hookDecisionKey struct{}
type hookDecision struct{ decision, reason string }

// WithToolDecision carries a PreToolUse decision through permission gates.
func WithToolDecision(ctx context.Context, decision, reason string) context.Context {
	return context.WithValue(ctx, hookDecisionKey{}, hookDecision{decision, reason})
}

// ToolDecision returns the current invocation's hook decision and reason.
func ToolDecision(ctx context.Context) (string, string) {
	value, _ := ctx.Value(hookDecisionKey{}).(hookDecision)
	return value.decision, value.reason
}

type notificationKey struct{}

// WithNotification observes a real approval request, including a headless refusal.
func WithNotification(ctx context.Context, notify func(context.Context, string) error) context.Context {
	return context.WithValue(ctx, notificationKey{}, notify)
}

// Notify observes a real permission prompt, not a child policy probe.
func Notify(ctx context.Context, reason string) error {
	if notify, _ := ctx.Value(notificationKey{}).(func(context.Context, string) error); notify != nil {
		return notify(ctx, reason)
	}
	return nil
}
