package work

import (
	"context"
	"testing"
)

func TestInvocationContextScopedAndPreservesCancellation(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	if _, ok := InvocationFromContext(root); ok {
		t.Fatal("unexpected invocation")
	}
	first := Invocation{RunID: "r", CallID: "message:0", ToolCallID: "raw"}
	child := WithInvocation(root, first)
	if got, ok := InvocationFromContext(child); !ok || got != first {
		t.Fatal("lost occurrence", got)
	}
	second := WithInvocation(child, Invocation{RunID: "r", CallID: "message:1", ToolCallID: "raw"})
	if got, _ := InvocationFromContext(child); got != first {
		t.Fatal("sibling context overwritten")
	}
	if got, _ := InvocationFromContext(second); got.CallID == first.CallID {
		t.Fatal("occurrence not shadowed")
	}
	cancel()
	if child.Err() != context.Canceled || second.Err() != context.Canceled {
		t.Fatal("invocation detached cancellation")
	}
}
