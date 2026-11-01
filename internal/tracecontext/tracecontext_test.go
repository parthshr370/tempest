package tracecontext

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
)

const validTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func TestParseFollowsTheW3CRules(t *testing.T) {
	parsed, ok := Parse(validTraceparent)
	if !ok || parsed.TraceID.String() != "4bf92f3577b34da6a3ce929d0e0e4736" || parsed.SpanID.String() != "00f067aa0ba902b7" || !parsed.Sampled {
		t.Fatalf("Parse(valid) = %+v, %v", parsed, ok)
	}
	if parsed.Traceparent() != validTraceparent {
		t.Fatalf("round trip = %q", parsed.Traceparent())
	}
	if unsampled, ok := Parse("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"); !ok || unsampled.Sampled {
		t.Fatal("flags 00 must parse as not sampled")
	}
	if future, ok := Parse("cc-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra"); !ok || future.SpanID != parsed.SpanID {
		t.Fatal("a later version's leading fields must parse")
	}
	for _, invalid := range []string{
		"",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra",
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"00_4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-0x",
	} {
		if _, ok := Parse(invalid); ok {
			t.Errorf("Parse(%q) accepted an invalid traceparent", invalid)
		}
	}
}

func TestRunsAndChildrenShareTheTrace(t *testing.T) {
	remote, _ := Parse(validTraceparent)
	run := StartRun(WithRemoteParent(context.Background(), remote), true)
	if FromContext(run).TraceID != remote.TraceID || ParentFromContext(run) != remote || FromContext(run).SpanID == remote.SpanID {
		t.Fatal("a run under a remote parent must be its child in the same trace")
	}
	child := StartChild(run)
	if FromContext(child).TraceID != remote.TraceID || ParentFromContext(child) != FromContext(run) {
		t.Fatal("a request or tool span must be a child of the run")
	}
	root := StartRun(context.Background(), true)
	if !FromContext(root).IsValid() || !FromContext(root).Sampled || ParentFromContext(root).IsValid() {
		t.Fatal("a run without a parent must root a new sampled trace")
	}
	untraced := StartRun(WithRemoteParent(context.Background(), remote), false)
	if FromContext(untraced).IsValid() || FromContext(StartChild(untraced)).IsValid() {
		t.Fatal("an untraced run must shadow the remote parent")
	}
}

func TestInjectExtractAndEnviron(t *testing.T) {
	remote, _ := Parse(validTraceparent)
	ctx := WithRemoteParent(context.Background(), remote)
	header := http.Header{}
	Inject(ctx, header)
	if extracted, ok := Extract(header); !ok || extracted != remote {
		t.Fatalf("Extract(Inject) = %+v, %v", extracted, ok)
	}
	header.Add(Header, validTraceparent)
	if _, ok := Extract(header); ok {
		t.Fatal("a repeated traceparent header must be ignored")
	}
	Inject(context.Background(), http.Header{})

	env := Environ(ctx, []string{"PATH=/bin", EnvVar + "=stale"})
	if !slices.Equal(env, []string{"PATH=/bin", EnvVar + "=" + validTraceparent}) {
		t.Fatalf("Environ = %v", env)
	}
	if Environ(context.Background(), nil) != nil {
		t.Fatal("an untraced command must keep inheriting the environment")
	}
	inherited := Environ(ctx, nil)
	if len(inherited) == 0 || !strings.HasPrefix(inherited[len(inherited)-1], EnvVar+"=") {
		t.Fatalf("Environ(nil) = %v, want the process environment plus TRACEPARENT", inherited)
	}
}

// A received tracestate travels with its trace to every outgoing request and
// subprocess below it, as W3C Trace Context requires of a participant; a
// malformed one is dropped without affecting the traceparent.
func TestTracestateTravelsWithTheTrace(t *testing.T) {
	incoming := http.Header{}
	incoming.Set("Traceparent", validTraceparent)
	incoming.Add("Tracestate", "congo=t61rcWkgMzE, ")
	incoming.Add("Tracestate", "tenant@vendor=a b")
	remote, ok := Extract(incoming)
	if !ok {
		t.Fatal("Extract rejected a valid traceparent")
	}
	const want = "congo=t61rcWkgMzE,tenant@vendor=a b"
	child := StartChild(StartRun(WithRemoteParent(context.Background(), remote), true))
	outgoing := http.Header{}
	Inject(child, outgoing)
	if got := outgoing.Values("tracestate"); len(got) != 1 || got[0] != want {
		t.Fatalf("outgoing tracestate = %q, want %q", got, want)
	}
	env := Environ(child, []string{"TRACESTATE=stale=1"})
	if !slices.Contains(env, "TRACESTATE="+want) || slices.Contains(env, "TRACESTATE=stale=1") {
		t.Fatalf("Environ = %v, want TRACESTATE=%s", env, want)
	}
	if root := Environ(StartRun(context.Background(), true), []string{"TRACESTATE=stale=1"}); slices.ContainsFunc(root, func(entry string) bool { return strings.HasPrefix(entry, "TRACESTATE=") }) {
		t.Fatalf("a new trace must not pass on an inherited tracestate: %v", root)
	}

	for _, invalid := range []string{
		"Upper=1",
		"key=has,=empty-key",
		"key=has=equals",
		"dup=1,dup=2",
		"key=" + strings.Repeat("v", 257),
		manyMembers(33),
	} {
		incoming := http.Header{}
		incoming.Set("traceparent", validTraceparent)
		incoming.Set("tracestate", invalid)
		remote, ok := Extract(incoming)
		if !ok {
			t.Fatalf("an invalid tracestate %q must not reject the traceparent", invalid)
		}
		outgoing := http.Header{}
		Inject(StartChild(StartRun(WithRemoteParent(context.Background(), remote), true)), outgoing)
		if got := outgoing.Get("tracestate"); got != "" {
			t.Errorf("invalid tracestate %q was passed on as %q", invalid, got)
		}
	}
}

func manyMembers(count int) string {
	members := make([]string, count)
	for i := range members {
		members[i] = "k" + strings.Repeat("x", i) + "=v"
	}
	return strings.Join(members, ",")
}
