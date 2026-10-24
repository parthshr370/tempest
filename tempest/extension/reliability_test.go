package extension_test

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

func installReliablePeer(t *testing.T, timeout time.Duration, configure func(*extension.ProcessOptions)) (*extension.Registry, *extension.Lease) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	fixture, err := filepath.Abs("testdata/reliability.py")
	if err != nil {
		t.Fatal(err)
	}
	registry := extension.NewRegistry()
	t.Cleanup(func() {
		if err := registry.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	options := extension.ProcessOptions{ID: "sample", Argv: []string{python, fixture}, Timeout: timeout, Capabilities: []string{"next"}}
	if configure != nil {
		configure(&options)
	}
	if err := registry.InstallProcess(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	lease, err := registry.Acquire([]extension.Selection{{ID: "sample", Required: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return registry, lease
}
func processGrant(ctx context.Context) *extension.Grant {
	return extension.NewGrant(ctx, extension.GrantOptions{Identity: extension.InvocationIdentity{SessionID: "s", InvocationID: "call"}})
}
func TestProcessCallFailuresPreserveSharedGeneration(t *testing.T) {
	for _, mode := range []string{"cancel", "peer_cancel", "error", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			registry, lease := installReliablePeer(t, time.Second, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			arguments, err := json.Marshal(map[string]string{"mode": mode})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "cancel" {
				arguments = json.RawMessage(`{"mode":"block"}`)
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			}
			if mode == "oversize" {
				arguments, _ = json.Marshal(map[string]string{"large": strings.Repeat("x", 1<<20)})
			}
			grant := processGrant(ctx)
			if _, err := lease.Definitions()[0].Tools[0].Execute(ctx, extension.Call{Arguments: arguments}, grant); err == nil {
				t.Fatal("call unexpectedly succeeded")
			}
			grant.Revoke()
			next, err := registry.Acquire([]extension.Selection{{ID: "sample", Required: true}})
			if err != nil {
				t.Fatal("ordinary call failure retired process", err)
			}
			defer next.Close(context.Background())
			grant = processGrant(context.Background())
			defer grant.Revoke()
			text, err := next.Definitions()[0].Sections[0].Render(context.Background(), grant)
			if err != nil || text != "section ok" {
				t.Fatalf("generation bricked: %q %v", text, err)
			}
		})
	}
}
func TestProcessHardFailureMakesRequiredSelectionUnavailable(t *testing.T) {
	for _, mode := range []string{"block", "invalid", "exit"} {
		t.Run(mode, func(t *testing.T) {
			registry, lease := installReliablePeer(t, 100*time.Millisecond, nil)
			grant := processGrant(context.Background())
			defer grant.Revoke()
			arguments, _ := json.Marshal(map[string]string{"mode": mode})
			if _, err := lease.Definitions()[0].Tools[0].Execute(context.Background(), extension.Call{Arguments: arguments}, grant); err == nil {
				t.Fatal("hard failure succeeded")
			}
			if _, err := registry.Acquire([]extension.Selection{{ID: "sample", Required: true}}); !errors.Is(err, extension.ErrUnavailable) {
				t.Fatalf("dead generation remains available: %v", err)
			}
		})
	}
}
func TestWrapperBudgetExcludesNext(t *testing.T) {
	_, lease := installReliablePeer(t, 100*time.Millisecond, nil)
	grant := processGrant(context.Background())
	defer grant.Revoke()
	tool := extension.Tool{Execute: func(ctx context.Context, _ extension.Call, _ extension.Invocation) (extension.Result, error) {
		select {
		case <-time.After(250 * time.Millisecond):
			return extension.Result{Content: []extension.Content{{Type: "text", Text: "slow success"}}}, nil
		case <-ctx.Done():
			return extension.Result{}, ctx.Err()
		}
	}}
	result, err := extension.Execute(context.Background(), tool, extension.Call{Arguments: json.RawMessage(`{}`)}, grant, lease.Definitions()[0].Wraps)
	if err != nil || len(result.Content) != 1 || result.Content[0].Text != "slow success" {
		t.Fatalf("Next consumed wrapper budget: %+v %v", result, err)
	}
}
func TestProcessEnvironmentAndOperatorCeilings(t *testing.T) {
	t.Setenv("TEMPEST_TEST_PROVIDER_SECRET", "must-not-inherit")
	_, lease := installReliablePeer(t, time.Second, nil)
	definition := lease.Definitions()[0]
	if definition.Tools[0].Effect != extension.EffectExecute || definition.Tools[0].Replay != extension.ReplayNever || definition.Wraps[0].Replay != extension.ReplayNever {
		t.Fatal("manifest self-authorized read or replay", definition)
	}
	grant := processGrant(context.Background())
	defer grant.Revoke()
	result, err := definition.Tools[0].Execute(context.Background(), extension.Call{Arguments: json.RawMessage(`{"mode":"environment"}`)}, grant)
	if err != nil || strings.Contains(result.Content[0].Text, "must-not-inherit") {
		t.Fatalf("environment leaked: %v", err)
	}
	_, authorized := installReliablePeer(t, time.Second, func(options *extension.ProcessOptions) {
		options.ReadTools = []string{"t"}
		options.SafeHandlers = []string{"tool:t", "wrap:w"}
	})
	definition = authorized.Definitions()[0]
	if definition.Tools[0].Effect != extension.EffectRead || definition.Tools[0].Replay != extension.ReplaySafe || definition.Wraps[0].Replay != extension.ReplaySafe {
		t.Fatal("operator authorization ignored")
	}
}
func TestRegistryPreservesEmptyPresetAndUniqueToolNames(t *testing.T) {
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	if err := registry.Install(context.Background(), extension.Definition{ID: "sample", Revision: "v1", Presets: []extension.AgentPreset{{ID: "none", Version: 1, AllowedTools: []string{}}}}); err != nil {
		t.Fatal(err)
	}
	lease, err := registry.Acquire([]extension.Selection{{ID: "sample"}})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close(context.Background())
	preset := lease.Definitions()[0].Presets[0]
	if preset.AllowedTools == nil {
		t.Fatal("empty restriction became unrestricted")
	}
	raw, err := json.Marshal(preset)
	if err != nil {
		t.Fatal(err)
	}
	var decoded extension.AgentPreset
	if err = json.Unmarshal(raw, &decoded); err != nil || decoded.AllowedTools == nil {
		t.Fatalf("empty restriction lost in persistence: %s %v", raw, err)
	}
	if extension.ToolName("a", "b__c") == extension.ToolName("a__b", "c") || extension.ToolName("a_b", "c") == extension.ToolName("a-ub", "c") {
		t.Fatal("model tool name collision")
	}
}
func TestReplacementCloseFailureDoesNotInvalidatePublishedGeneration(t *testing.T) {
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	old := toolDefinition("v1")
	old.Close = func(context.Context) error { return errors.New("old cleanup failure") }
	if err := registry.Install(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if err := registry.Install(context.Background(), toolDefinition("v2")); err != nil {
		t.Fatal("published replacement reported failure", err)
	}
	lease, err := registry.Acquire([]extension.Selection{{ID: "sample", Required: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close(context.Background())
	if lease.Definitions()[0].Revision != "v2" {
		t.Fatal("replacement not published")
	}
}
func TestRegistryRejectsIncompatibleDocumentsBeforePublication(t *testing.T) {
	registry := extension.NewRegistry()
	defer registry.Close(context.Background())
	definition := extension.Definition{ID: "sample", Revision: "v1", Documents: []durable.DocumentDefinition{{Namespace: "sample", Version: 1, Schema: json.RawMessage(`{"type":"object"}`), Initial: json.RawMessage(`{}`), History: durable.HistoryRewindable, Fork: durable.ForkAsOf, Reset: durable.ResetPreserve}}}
	if err := registry.Install(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	definition.Revision = "v2"
	definition.Documents[0].Initial = json.RawMessage(`{"changed":true}`)
	if err := registry.Install(context.Background(), definition); err == nil {
		t.Fatal("incompatible document replacement published")
	}
	definition.Documents[0].Version = 2
	if err := registry.Install(context.Background(), definition); err == nil {
		t.Fatal("unsupported migration version accepted")
	}
}
func TestGrantIsolatesReadsAndCommitIDs(t *testing.T) {
	var commits []string
	grant := extension.NewGrant(context.Background(), extension.GrantOptions{Identity: extension.InvocationIdentity{SessionID: "s", InvocationID: "call", ExtensionID: "sample", Handler: "tool:echo"}, Namespaces: []string{"sample"}, Read: func(context.Context, durable.Query) (durable.Page, error) { return durable.Page{}, nil }, Commit: func(_ context.Context, commit durable.Commit) (durable.Receipt, error) {
		commits = append(commits, commit.ID)
		return durable.Receipt{}, nil
	}})
	defer grant.Revoke()
	for _, query := range []durable.Query{{}, {Namespace: "_host.extensions"}, {Namespace: "other"}, {Namespace: "sample", Kind: durable.MemoKind}} {
		if _, err := grant.Read(context.Background(), query); err == nil {
			t.Fatal("unauthorized read", query)
		}
	}
	if _, err := grant.Read(context.Background(), durable.Query{Namespace: "sample"}); err != nil {
		t.Fatal(err)
	}
	commit := durable.Commit{ID: "extension:call:result", Changes: []durable.Change{{SessionID: "s", Kind: durable.DocumentKind, Namespace: "sample", ID: "state", Version: 1, Value: json.RawMessage(`{}`)}}}
	for range 2 {
		if _, err := grant.Commit(context.Background(), commit); err != nil {
			t.Fatal(err)
		}
	}
	if len(commits) != 2 || commits[0] == commit.ID || commits[0] != commits[1] || !strings.HasPrefix(commits[0], "callback:") {
		t.Fatal("callback commit ID not scoped", commits)
	}
}
