package extension_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

func toolDefinition(revision string) extension.Definition {
	return extension.Definition{ID: "sample", Revision: revision, Tools: []extension.Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`), Effect: extension.EffectRead, Replay: extension.ReplaySafe, Execute: func(context.Context, extension.Call, extension.Invocation) (extension.Result, error) {
		return extension.Result{Content: []extension.Content{{Type: "text", Text: revision}}}, nil
	}}}}
}

func TestImmutableGenerationsDrainAndRequiredRemoval(t *testing.T) {
	ctx := context.Background()
	registry := extension.NewRegistry()
	defer registry.Close(ctx)
	started, release := make(chan struct{}), make(chan struct{})
	var retired atomic.Bool
	first := toolDefinition("v1")
	first.Close = func(context.Context) error { retired.Store(true); return nil }
	first.Tools[0].Execute = func(context.Context, extension.Call, extension.Invocation) (extension.Result, error) {
		close(started)
		<-release
		return extension.Result{Content: []extension.Content{{Type: "text", Text: "v1"}}}, nil
	}
	if err := registry.Install(ctx, first); err != nil {
		t.Fatal(err)
	}
	first.Tools[0].InputSchema[0] = 'x'
	lease, err := registry.Acquire([]extension.Selection{{ID: "sample", Required: true}})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan extension.Result, 1)
	go func() {
		result, _ := lease.Definitions()[0].Tools[0].Execute(ctx, extension.Call{}, nil)
		results <- result
	}()
	<-started
	if err = registry.Install(ctx, toolDefinition("v2")); err != nil {
		t.Fatal(err)
	}
	if retired.Load() {
		t.Fatal("v1 retired with live lease")
	}
	close(release)
	if result := <-results; result.Content[0].Text != "v1" {
		t.Fatal(result)
	}
	if err = lease.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if !retired.Load() {
		t.Fatal("v1 did not retire after lease")
	}
	latest, err := registry.Acquire([]extension.Selection{{ID: "sample", Required: true}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := latest.Definitions()[0].Tools[0].Execute(ctx, extension.Call{}, nil)
	if err != nil || result.Content[0].Text != "v2" {
		t.Fatalf("%+v %v", result, err)
	}
	_ = latest.Close(ctx)
	if err = registry.Uninstall(ctx, "sample"); err != nil {
		t.Fatal(err)
	}
	if _, err = registry.Acquire([]extension.Selection{{ID: "sample", Required: true}}); !errors.Is(err, extension.ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestPinnedSingleUseNextAndGrantRevocation(t *testing.T) {
	ctx := context.Background()
	var calls int
	var escaped extension.Next
	tool := extension.Tool{Execute: func(_ context.Context, call extension.Call, _ extension.Invocation) (extension.Result, error) {
		calls++
		if string(call.Arguments) != `{"ok":true}` {
			t.Fatal("gated arguments changed", string(call.Arguments))
		}
		return extension.Result{}, nil
	}}
	wraps := []extension.Wrap{{Handle: func(ctx context.Context, call extension.Call, _ extension.Invocation, next extension.Next) (extension.Result, error) {
		escaped = next
		call.Arguments[0] = 'x'
		result, err := next.Call(ctx)
		if err != nil {
			return result, err
		}
		if _, err = next.Call(ctx); !errors.Is(err, extension.ErrNextUsed) {
			t.Fatal(err)
		}
		return result, nil
	}}}
	if _, err := extension.Execute(ctx, tool, extension.Call{Arguments: json.RawMessage(`{"ok":true}`)}, nil, wraps); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	if _, err := escaped.Call(ctx); !errors.Is(err, extension.ErrNextUsed) {
		t.Fatal(err)
	}
	grant := extension.NewGrant(ctx, extension.GrantOptions{Identity: extension.InvocationIdentity{SessionID: "session"}, Read: func(context.Context, durable.Query) (durable.Page, error) { return durable.Page{}, nil }})
	if _, err := grant.Read(ctx, durable.Query{SessionID: "foreign"}); err == nil {
		t.Fatal("foreign read accepted")
	}
	if _, err := grant.Commit(ctx, durable.Commit{ID: "attack", Changes: []durable.Change{{SessionID: "session", Kind: durable.SubmissionKind, ID: "control"}}}); err == nil {
		t.Fatal("host mutation accepted")
	}
	grant.Revoke()
	if _, err := grant.Read(ctx, durable.Query{}); !errors.Is(err, extension.ErrExpired) {
		t.Fatal(err)
	}
}

func TestConservativeReplayCompatibility(t *testing.T) {
	if extension.CanReplay(extension.ReplayNever, extension.ReplaySafe, "v1", "v1", "local", "local") {
		t.Fatal("default upgraded")
	}
	if extension.CanReplay(extension.ReplaySafe, extension.ReplaySafe, "v1", "v2", "local", "local") {
		t.Fatal("revision upgraded")
	}
	if extension.CanReplay(extension.ReplaySafe, extension.ReplaySafe, "v1", "v1", "old", "new") {
		t.Fatal("workspace changed")
	}
	if !extension.CanReplay(extension.ReplaySafe, extension.ReplaySafe, "v1", "v1", "local", "local") {
		t.Fatal("compatible safe call rejected")
	}
}

func TestSubprocessRejectsMalformedBoundsVersionsAndSchemas(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("Python fixture required:", err)
	}
	peers := map[string]string{
		"malformed": `import sys;sys.stdin.readline();print('not json',flush=True)`,
		"oversize":  `import sys;sys.stdin.readline();print('x'*(1048576+1),flush=True)`,
		"version":   `import json,sys;f=json.loads(sys.stdin.readline());print(json.dumps({'jsonrpc':'2.0','id':f['id'],'result':{'protocolVersion':'unsupported','id':'sample','revision':'v1'}}),flush=True)`,
		"schema":    `import json,sys;f=json.loads(sys.stdin.readline());print(json.dumps({'jsonrpc':'2.0','id':f['id'],'result':{'protocolVersion':'tempest-extension/1','id':'sample','revision':'v1','definition':{'tools':[{'name':'bad','effect':'read','inputSchema':{'type':'invalid'}}]}}}),flush=True)`,
	}
	for name, script := range peers {
		t.Run(name, func(t *testing.T) {
			registry := extension.NewRegistry()
			defer registry.Close(context.Background())
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := registry.InstallProcess(ctx, extension.ProcessOptions{ID: "sample", Argv: []string{python, "-c", script}, Timeout: time.Second}); err == nil {
				t.Fatal("invalid peer installed")
			}
		})
	}
}

func TestProcessOldGenerationRetiresAfterLease(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	fixture, err := filepath.Abs("testdata/peer.py")
	if err != nil {
		t.Fatal(err)
	}
	var held atomic.Int32
	admit := func(context.Context) (func(), error) { held.Add(1); return func() { held.Add(-1) }, nil }
	if err = registry.InstallProcess(context.Background(), extension.ProcessOptions{ID: "sample", Argv: []string{python, fixture}, Capabilities: []string{"read", "memo", "commit", "next"}, AdmitProcess: admit}); err != nil {
		t.Fatal(err)
	}
	old, err := registry.Acquire([]extension.Selection{{ID: "sample", Required: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Install(context.Background(), toolDefinition("v2")); err != nil {
		t.Fatal(err)
	}
	if held.Load() != 1 {
		t.Fatal("old process released its slot before lease retirement")
	}
	grant := extension.NewGrant(context.Background(), extension.GrantOptions{Identity: extension.InvocationIdentity{SessionID: "s", InvocationID: "section"}})
	defer grant.Revoke()
	text, err := old.Definitions()[0].Sections[0].Render(context.Background(), grant)
	if err != nil || text != "Extension section" {
		t.Fatalf("old process lost lease: %q %v", text, err)
	}
	if err = old.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if held.Load() != 0 {
		t.Fatal("retired process retained host slot")
	}
}

func TestExtensionProcessAdmissionReleasesOnStartFailure(t *testing.T) {
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	var held atomic.Int32
	admit := func(context.Context) (func(), error) { held.Add(1); return func() { held.Add(-1) }, nil }
	if err := registry.InstallProcess(context.Background(), extension.ProcessOptions{ID: "sample", Argv: []string{"/no/such/tempest-extension"}, AdmitProcess: admit}); err == nil || held.Load() != 0 {
		t.Fatalf("failed start retained slot: %d %v", held.Load(), err)
	}
	marker := filepath.Join(t.TempDir(), "must-not-run")
	denied := errors.New("process limit")
	if err := registry.InstallProcess(context.Background(), extension.ProcessOptions{ID: "sample", Argv: []string{"/usr/bin/touch", marker}, AdmitProcess: func(context.Context) (func(), error) { return nil, denied }}); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("denied process started", err)
	}
}

func TestRedactionCannotRewriteUsageOrFailureAccounting(t *testing.T) {
	original := extension.Result{Content: []extension.Content{{Type: "text", Text: "secret"}}, Usage: json.RawMessage(`{"charged":1}`), IsError: true, Control: "terminate"}
	decision := extension.HookDecision{Kind: "redact", Result: &extension.Result{Content: []extension.Content{{Type: "text", Text: "redacted"}}, StructuredContent: json.RawMessage(`null`), Usage: json.RawMessage(`{"charged":0}`)}}
	output, err := extension.ApplyDecision(extension.HookInput{Event: extension.AfterTool, Result: &original}, decision)
	if err != nil {
		t.Fatal(err)
	}
	if output.Result.Content[0].Text != "redacted" || string(output.Result.Usage) != `{"charged":1}` || !output.Result.IsError || output.Result.Control != "terminate" {
		t.Fatalf("redaction changed host accounting or control: %+v", output.Result)
	}
}
