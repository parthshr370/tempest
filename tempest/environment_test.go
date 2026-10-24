package tempest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/internal/envlocal"
	"go.harness.dev/harness/internal/envrpc"
	"go.harness.dev/harness/internal/prompt"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/tools"
	"go.harness.dev/harness/tempest/environment"
)

type droppingEnvironment struct {
	environment.Lease
	stream    net.Conn
	mutations *atomic.Int32
}

func (lease *droppingEnvironment) Call(ctx context.Context, call environment.Call) (json.RawMessage, error) {
	result, err := lease.Lease.Call(ctx, call)
	if call.Operation == "write" && err == nil {
		lease.mutations.Add(1)
		_ = lease.stream.Close()
	}
	return result, err
}

func testEnvironmentResolver(t *testing.T, workspace *string, drop bool, mutations *atomic.Int32) environment.Resolver {
	t.Helper()
	return func(ctx context.Context, reference environment.Reference) (environment.Lease, error) {
		local, err := envlocal.NewNamed(*workspace, "sdk-remote")
		if err != nil {
			return nil, err
		}
		if err = tools.BindEnvironment(local, tools.ToolsOptions{SessionDataDir: t.TempDir()}); err != nil {
			_ = local.Close()
			return nil, err
		}
		bindEnvironmentInspection(local)
		clientStream, serverStream := net.Pipe()
		var daemon environment.Lease = local
		if drop {
			daemon = &droppingEnvironment{Lease: local, stream: serverStream, mutations: mutations}
		}
		done := make(chan error, 1)
		go func() {
			done <- envrpc.Serve(context.Background(), serverStream, daemon, environment.Capabilities{environment.Files, environment.Search, environment.Exec, environment.Cancel, environment.Permissions})
		}()
		client, err := envrpc.Connect(ctx, clientStream, reference)
		t.Cleanup(func() {
			if client != nil {
				_ = client.Close()
			}
			_ = local.Close()
			<-done
		})
		return client, err
	}
}

func TestSDKRemoteEnvironmentPersistsIdentityAndRejectsReplacement(t *testing.T) {
	workspace, host, sessions := t.TempDir(), t.TempDir(), t.TempDir()
	resolver := testEnvironmentResolver(t, &workspace, false, nil)
	provider := faux.New(faux.Options{})
	provider.AppendResponses(faux.Respond(faux.ToolCall("write", json.RawMessage(`{"path":"note.txt","content":"remote bytes"}`), "reused-provider-id")), faux.Respond(faux.Text("done")))
	stack := &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple, ContextFiles: []prompt.ContextFile{}}
	session, err := Open(t.Context(), Options{Cwd: host, SessionDir: sessions, New: &NewSession{PermissionMode: PermissionMode("bypass")}, Stack: stack, Environment: environment.Reference{ID: "sdk-remote", Cwd: "."}, EnvironmentResolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	if _, err = session.Prompt(t.Context(), Prompt{Text: "write remotely"}); err != nil {
		t.Fatal(err)
	}
	reference := session.Environment()
	if reference.Generation == "" {
		t.Fatal("environment generation not resolved")
	}
	if _, err = session.CreateCheckpoint(t.Context(), "must not create local checkpoint"); !errors.Is(err, environment.ErrUnsupported) {
		t.Fatalf("remote checkpoint: %v", err)
	}
	path, id := session.core.store.Path(), session.ID()
	if err = session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	encodedReference, err := json.Marshal(reference.Key())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(journal, encodedReference) {
		t.Fatalf("tool intent omitted complete environment identity: %s", journal)
	}
	contents, err := os.ReadFile(filepath.Join(workspace, "note.txt"))
	if err != nil || string(contents) != "remote bytes" {
		t.Fatalf("remote bytes: %q %v", contents, err)
	}
	if _, err = os.Stat(filepath.Join(host, "note.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("host cwd touched: %v", err)
	}
	reopened, err := Open(t.Context(), Options{SessionDir: sessions, Resume: &ResumeSession{SessionID: id}, Stack: stack, EnvironmentResolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Environment() != reference {
		t.Fatalf("reference changed: %+v", reopened.Environment())
	}
	if err = reopened.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	workspace = t.TempDir()
	replaced, err := Open(t.Context(), Options{SessionDir: sessions, Resume: &ResumeSession{SessionID: id}, Stack: stack, EnvironmentResolver: resolver})
	if replaced != nil {
		_ = replaced.Close(context.Background())
		t.Fatal("replacement generation opened")
	}
	if !errors.Is(err, environment.ErrIdentity) {
		t.Fatalf("replacement did not block recovery: %v", err)
	}
}

func TestSDKRemoteNeverConnectionLossKeepsUnknown(t *testing.T) {
	workspace := t.TempDir()
	var mutations atomic.Int32
	provider := faux.New(faux.Options{})
	provider.AppendResponses(faux.Respond(faux.ToolCall("write", json.RawMessage(`{"path":"effect","content":"performed"}`), "effect")), faux.Respond(faux.Text("unknown acknowledged")))
	session, err := Open(t.Context(), Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), New: &NewSession{PermissionMode: PermissionMode("bypass")}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple, ContextFiles: []prompt.ContextFile{}}, Environment: environment.Reference{ID: "sdk-remote", Cwd: "."}, EnvironmentResolver: testEnvironmentResolver(t, &workspace, true, &mutations)})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err = session.Prompt(ctx, Prompt{Text: "mutate once"}); err != nil {
		t.Fatal(err)
	}
	if mutations.Load() != 1 {
		t.Fatalf("Never mutation dispatched %d times", mutations.Load())
	}
	contents, err := os.ReadFile(filepath.Join(workspace, "effect"))
	if err != nil || string(contents) != "performed" {
		t.Fatalf("effect: %q %v", contents, err)
	}
	journal, err := os.ReadFile(session.core.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(journal), environment.ErrUnknown.Error()) {
		t.Fatalf("lost response became a known success or safe failure: %s", journal)
	}
}
