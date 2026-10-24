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
	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest/environment"
)

func TestRemoteOversizedMutationReplyIsUnknownAfterDispatch(t *testing.T) {
	result, err := json.Marshal(strings.Repeat("x", (1<<20)+1))
	if err != nil {
		t.Fatal(err)
	}
	daemon := &receiptDaemon{
		reference: environment.Reference{ID: "workspace", Generation: "generation-one", Cwd: "."},
		marker:    filepath.Join(t.TempDir(), "effect"),
		receipts:  map[string]environment.Outcome{},
		digests:   map[string]string{},
		result:    result,
	}
	client := connectDaemon(t, daemon, false)
	call := environment.Call{Environment: daemon.Reference(), Key: "large-write", Operation: "write", Arguments: json.RawMessage(`{}`)}
	if _, err := client.Call(context.Background(), call); !errors.Is(err, environment.ErrUnknown) {
		t.Fatalf("oversized dispatched result = %v, want unknown", err)
	}
	daemon.mu.Lock()
	mutations := daemon.mutations
	daemon.mu.Unlock()
	if mutations != 1 {
		t.Fatalf("effect count = %d, want one", mutations)
	}
}

func TestRemoteImageResultsUseBoundedLargerFrame(t *testing.T) {
	imageResult := json.RawMessage(`{"Content":[{"type":"image","data":"` + strings.Repeat("A", 5<<20) + `","mimeType":"image/png"}]}`)
	for _, operation := range []string{"view_image", "read"} {
		t.Run(operation, func(t *testing.T) {
			daemon := &receiptDaemon{
				reference: environment.Reference{ID: "workspace", Generation: "generation-one", Cwd: "."},
				marker:    filepath.Join(t.TempDir(), "effect"),
				receipts:  map[string]environment.Outcome{},
				digests:   map[string]string{},
				result:    imageResult,
			}
			client := connectDaemon(t, daemon, false)
			call := environment.Call{Environment: daemon.Reference(), Key: operation, Operation: operation, Arguments: json.RawMessage(`{}`)}
			got, err := client.Call(context.Background(), call)
			if err != nil || string(got) != string(imageResult) {
				t.Fatalf("large image result length=%d err=%v", len(got), err)
			}
		})
	}
}

func TestRemoteFrameTooLargeReplyIsUnknown(t *testing.T) {
	clientStream, serverStream := net.Pipe()
	server := rpc.NewStreamWithLimit(serverStream, 8<<20)
	reference := environment.Reference{ID: "workspace", Generation: "generation-one", Cwd: "."}
	type initialization struct {
		ProtocolVersion string                   `json:"protocolVersion"`
		MaxFrameSize    int                      `json:"maxFrameSize"`
		Environment     environment.Reference    `json:"environment"`
		Capabilities    environment.Capabilities `json:"capabilities"`
	}
	serving := make(chan error, 1)
	go func() {
		serving <- server.Serve(func(_ context.Context, method string, _ json.RawMessage) (any, *rpc.Error) {
			if method == "initialize" {
				return initialization{
					ProtocolVersion: envrpc.ProtocolVersion,
					MaxFrameSize:    8 << 20,
					Environment:     reference,
					Capabilities:    environment.Capabilities{environment.Files, environment.Cancel},
				}, nil
			}
			if strings.HasSuffix(method, "call") {
				return nil, rpc.NewError(-32000, "frame_too_large", "response exceeds frame cap")
			}
			return nil, nil
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := envrpc.Connect(ctx, clientStream, reference)
	if err != nil {
		_ = server.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		<-serving
	})
	call := environment.Call{Environment: reference, Key: "too-large", Operation: "write", Arguments: json.RawMessage(`{}`)}
	if _, err := client.Call(ctx, call); !errors.Is(err, environment.ErrUnknown) {
		t.Fatalf("frame_too_large response = %v, want unknown", err)
	}
}

func TestRemoteCallAfterDisconnectIsNotUnknownBeforeDispatch(t *testing.T) {
	daemon := &receiptDaemon{
		reference: environment.Reference{ID: "workspace", Generation: "generation-one", Cwd: "."},
		marker:    filepath.Join(t.TempDir(), "effect"),
		receipts:  map[string]environment.Outcome{},
		digests:   map[string]string{},
	}
	client := connectDaemon(t, daemon, false)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	call := environment.Call{Environment: daemon.Reference(), Key: "after-close", Operation: "write", Arguments: json.RawMessage(`{}`)}
	if _, err := client.Call(context.Background(), call); err == nil || errors.Is(err, environment.ErrUnknown) || !errors.Is(err, rpc.ErrClosed) {
		t.Fatalf("call after disconnect = %v, want known closed-before-send error", err)
	}
	daemon.mu.Lock()
	mutations := daemon.mutations
	daemon.mu.Unlock()
	if mutations != 0 {
		t.Fatalf("disconnected call dispatched %d times", mutations)
	}
}

type cancelBarrierLease struct {
	reference      environment.Reference
	entered        chan struct{}
	allowRegister  chan struct{}
	finish         chan struct{}
	cancelObserved chan struct{}
	cancelOnce     sync.Once
	mu             sync.Mutex
	cancelCalls    int
}

func (lease *cancelBarrierLease) Reference() environment.Reference { return lease.reference }
func (lease *cancelBarrierLease) Capabilities() environment.Capabilities {
	return environment.Capabilities{environment.Files, environment.Cancel}
}
func (lease *cancelBarrierLease) Call(ctx context.Context, _ environment.Call) (json.RawMessage, error) {
	close(lease.entered)
	go func() {
		<-ctx.Done()
		lease.cancelOnce.Do(func() { close(lease.cancelObserved) })
	}()
	<-lease.allowRegister
	if err := ctx.Err(); err != nil {
		<-lease.finish
		return nil, err
	}
	return json.RawMessage(`{}`), nil
}
func (lease *cancelBarrierLease) Cancel(context.Context, string) (environment.Cancellation, error) {
	lease.mu.Lock()
	lease.cancelCalls++
	lease.mu.Unlock()
	return environment.Cancellation{}, errors.New("invocation is not live")
}
func (lease *cancelBarrierLease) Lookup(context.Context, string) (environment.Outcome, error) {
	return environment.Outcome{}, environment.ErrUnsupported
}
func (lease *cancelBarrierLease) Close() error { return nil }

func connectTestLease(t *testing.T, lease environment.Lease) *envrpc.Remote {
	t.Helper()
	clientStream, serverStream := net.Pipe()
	serving := make(chan error, 1)
	go func() {
		serving <- envrpc.Serve(context.Background(), serverStream, lease, lease.Capabilities())
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := envrpc.Connect(ctx, clientStream, lease.Reference())
	if err != nil {
		_ = clientStream.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		<-serving
	})
	return client
}

func TestCancelWaitsForCallReservedBeforeLeaseRegistration(t *testing.T) {
	lease := &cancelBarrierLease{
		reference:      environment.Reference{ID: "workspace", Generation: "generation-one", Cwd: "."},
		entered:        make(chan struct{}),
		allowRegister:  make(chan struct{}, 1),
		finish:         make(chan struct{}, 1),
		cancelObserved: make(chan struct{}),
	}
	client := connectTestLease(t, lease)
	t.Cleanup(func() {
		select {
		case lease.allowRegister <- struct{}{}:
		default:
		}
		select {
		case lease.finish <- struct{}{}:
		default:
		}
	})
	callCtx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, err := client.Call(callCtx, environment.Call{Environment: lease.Reference(), Key: "reserved", Operation: "write", Arguments: json.RawMessage(`{}`)})
		finished <- err
	}()
	select {
	case <-lease.entered:
	case <-time.After(time.Second):
		t.Fatal("lease call did not enter")
	}
	cancel()
	select {
	case <-lease.cancelObserved:
	case <-time.After(time.Second):
		t.Fatal("daemon did not cancel the reserved invocation context")
	}
	select {
	case err := <-finished:
		t.Fatalf("call returned before its lease handler joined: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	lease.allowRegister <- struct{}{}
	lease.finish <- struct{}{}
	select {
	case err := <-finished:
		if !errors.Is(err, environment.ErrUnknown) {
			t.Fatalf("cancelled call = %v, want unknown", err)
		}
	case <-time.After(time.Second):
		t.Fatal("call did not finish after releasing the lease handler")
	}
	lease.mu.Lock()
	cancelCalls := lease.cancelCalls
	lease.mu.Unlock()
	if cancelCalls != 0 {
		t.Fatalf("daemon used lease.Cancel %d times for its reserved call", cancelCalls)
	}
}

func TestLaunchPassesSSHIdentityEnvironment(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "test-socket")
	t.Setenv("USER", "wire-user")
	t.Setenv("LOGNAME", "wire-login")
	cwd := t.TempDir()
	daemonPath := filepath.Join(cwd, "daemon.sh")
	script := `#!/bin/sh
IFS= read -r request || exit 2
if [ "$SSH_AUTH_SOCK" != "test-socket" ] || [ "$USER" != "wire-user" ] || [ "$LOGNAME" != "wire-login" ]; then
  printf '%s\n' '{"jsonrpc":"2.0","id":"tempest_1","error":{"code":-32000,"message":"launch identity environment missing","data":{"code":"launch_error"}}}'
  exit 0
fi
printf '%s\n' '{"jsonrpc":"2.0","id":"tempest_1","result":{"protocolVersion":"tempest-environment/1","maxFrameSize":8388608,"environment":{"id":"workspace","generation":"generation-one","cwd":"."},"capabilities":["files","cancel"]}}'
IFS= read -r initialized || exit 3
`
	if err := os.WriteFile(daemonPath, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	remote, err := envrpc.Launch(context.Background(), envrpc.ProcessOptions{Argv: []string{daemonPath}, Cwd: cwd, Timeout: 5 * time.Second}, environment.Reference{ID: "workspace", Generation: "generation-one", Cwd: "."})
	if err != nil {
		t.Fatalf("launch with SSH identity environment: %v", err)
	}
	if err := remote.Close(); err != nil {
		t.Fatal(err)
	}
}
