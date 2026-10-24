package envrpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/envrpc"
	"go.harness.dev/harness/tempest/environment"
)

type receiptDaemon struct {
	mu             sync.Mutex
	reference      environment.Reference
	marker         string
	supportsLookup bool
	receipts       map[string]environment.Outcome
	digests        map[string]string
	drop           func() error
	lookupCalls    int
	mutations      int
	result         json.RawMessage
}

func (daemon *receiptDaemon) Reference() environment.Reference { return daemon.reference }
func (daemon *receiptDaemon) Capabilities() environment.Capabilities {
	capabilities := environment.Capabilities{environment.Files, environment.Cancel}
	if daemon.supportsLookup {
		capabilities = append(capabilities, environment.Lookup)
	}
	return capabilities
}
func (daemon *receiptDaemon) Call(_ context.Context, call environment.Call) (json.RawMessage, error) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if outcome, ok := daemon.receipts[call.Key]; ok {
		if daemon.digests[call.Key] != string(call.Arguments) {
			return nil, errors.New("invocation key conflicts")
		}
		return outcome.Result, nil
	}
	if err := os.WriteFile(daemon.marker, []byte("mutated"), 0600); err != nil {
		return nil, err
	}
	daemon.mutations++
	result := daemon.result
	if len(result) == 0 {
		result = json.RawMessage(`{"Content":[{"type":"text","text":"original result"}]}`)
	}
	daemon.receipts[call.Key] = environment.Outcome{Found: true, Complete: true, Result: result}
	daemon.digests[call.Key] = string(call.Arguments)
	// The fake daemon records its receipt before deliberately losing the response.
	if daemon.drop != nil {
		closeConnection := daemon.drop
		daemon.drop = nil
		_ = closeConnection()
	}
	return result, nil
}
func (daemon *receiptDaemon) Cancel(context.Context, string) (environment.Cancellation, error) {
	return environment.Cancellation{Requested: true}, errors.New("remote process stop could not be confirmed")
}
func (daemon *receiptDaemon) Lookup(_ context.Context, key string) (environment.Outcome, error) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.lookupCalls++
	return daemon.receipts[key], nil
}
func (daemon *receiptDaemon) Close() error { return nil }

func connectDaemon(t *testing.T, daemon *receiptDaemon, drop bool) *envrpc.Remote {
	t.Helper()
	clientStream, serverStream := net.Pipe()
	if drop {
		daemon.drop = serverStream.Close
	}
	serving := make(chan error, 1)
	go func() { serving <- envrpc.Serve(context.Background(), serverStream, daemon, daemon.Capabilities()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := envrpc.Connect(ctx, clientStream, daemon.Reference())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(); <-serving })
	return client
}

func TestDisconnectedMutationNeverRetriesAndLookupIsNegotiated(t *testing.T) {
	for _, supportsLookup := range []bool{false, true} {
		name := "without-lookup"
		if supportsLookup {
			name = "with-lookup"
		}
		t.Run(name, func(t *testing.T) {
			daemon := &receiptDaemon{reference: environment.Reference{ID: "workspace", Generation: "generation-one", Cwd: "."}, marker: filepath.Join(t.TempDir(), "effect"), supportsLookup: supportsLookup, receipts: map[string]environment.Outcome{}, digests: map[string]string{}}
			client := connectDaemon(t, daemon, true)
			call := environment.Call{Environment: daemon.Reference(), Key: "session:occurrence", Operation: "write", Arguments: json.RawMessage(`{"path":"effect","content":"mutated"}`)}
			if _, err := client.Call(context.Background(), call); !errors.Is(err, environment.ErrUnknown) {
				t.Fatalf("disconnected Never effect was not unknown: %v", err)
			}
			if contents, err := os.ReadFile(daemon.marker); err != nil || string(contents) != "mutated" {
				t.Fatalf("mutation did not precede lost response: %q %v", contents, err)
			}
			daemon.mu.Lock()
			mutations, lookups := daemon.mutations, daemon.lookupCalls
			daemon.mu.Unlock()
			if mutations != 1 || lookups != 0 {
				t.Fatalf("implicit retry/lookup: mutations=%d lookups=%d", mutations, lookups)
			}
			reconnected := connectDaemon(t, daemon, false)
			outcome, err := reconnected.Lookup(context.Background(), call.Key)
			if !supportsLookup {
				if !errors.Is(err, environment.ErrUnsupported) {
					t.Fatalf("unnegotiated lookup: %v", err)
				}
				return
			}
			if err != nil || !outcome.Found || !outcome.Complete || !strings.Contains(string(outcome.Result), "original result") {
				t.Fatalf("lookup did not return original receipt: %+v %v", outcome, err)
			}
			result, err := reconnected.Call(context.Background(), call)
			if err != nil || string(result) != string(outcome.Result) {
				t.Fatalf("daemon deduplication: %s %v", result, err)
			}
			daemon.mu.Lock()
			mutations = daemon.mutations
			daemon.mu.Unlock()
			if mutations != 1 {
				t.Fatalf("deduplicated call mutated %d times", mutations)
			}
		})
	}
}

func TestFailedRemoteCancelIsNotConfirmed(t *testing.T) {
	daemon := &receiptDaemon{reference: environment.Reference{ID: "workspace", Generation: "one", Cwd: "."}, receipts: map[string]environment.Outcome{}, digests: map[string]string{}}
	client := connectDaemon(t, daemon, false)
	cancellation, err := client.Cancel(context.Background(), "active-command")
	if err == nil || cancellation.Confirmed {
		t.Fatalf("failed cancel appeared confirmed: %+v %v", cancellation, err)
	}
	_ = client.Close()
	cancellation, err = client.Cancel(context.Background(), "active-command")
	if err == nil || cancellation.Confirmed {
		t.Fatalf("disconnected cancel appeared confirmed: %+v %v", cancellation, err)
	}
}

func TestRemoteHandshakeRejectsIdentityMismatch(t *testing.T) {
	daemon := &receiptDaemon{reference: environment.Reference{ID: "workspace", Generation: "replacement", Cwd: "."}}
	clientStream, serverStream := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- envrpc.Serve(context.Background(), serverStream, daemon, daemon.Capabilities()) }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := envrpc.Connect(ctx, clientStream, environment.Reference{ID: "workspace", Generation: "old", Cwd: "."})
	if client != nil {
		_ = client.Close()
		t.Fatal("mismatched generation accepted")
	}
	if err == nil {
		t.Fatal("mismatched generation lacked an error")
	}
	<-done
}
