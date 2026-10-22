package work

import "context"

type invocationKey struct{}

// WithInvocation binds a durable tool occurrence to its execution context.
func WithInvocation(ctx context.Context, invocation Invocation) context.Context {
	return context.WithValue(ctx, invocationKey{}, invocation)
}

// InvocationFromContext reports the durable occurrence bound to ctx.
func InvocationFromContext(ctx context.Context) (Invocation, bool) {
	invocation, ok := ctx.Value(invocationKey{}).(Invocation)
	return invocation, ok
}
