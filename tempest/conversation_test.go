package tempest

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest/durable"
)

func TestForkWhileParentContinuesPinsHistoryAndPolicies(t *testing.T) {
	for _, backend := range []string{"jsonl", "memory"} {
		t.Run(backend, func(t *testing.T) {
			provider := faux.New(faux.Options{})
			provider.SetResponses(faux.Respond(faux.Text("first answer")), faux.Respond(faux.Text("sibling answer")), faux.Respond(faux.Text("parent answer")))
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			var sessionIDsMu sync.Mutex
			sessionIDs := make(map[string]bool)
			streamFn := func(ctx context.Context, model types.Model, c types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
				if options != nil {
					sessionIDsMu.Lock()
					sessionIDs[options.SessionID] = true
					sessionIDsMu.Unlock()
				}
				if len(c.Messages) > 0 {
					last := c.Messages[len(c.Messages)-1]
					encoded, err := json.Marshal(last)
					if err != nil {
						t.Error(err)
					}
					if last.Role() == "user" && strings.Contains(string(encoded), "parent continues") {
						close(entered)
						select {
						case <-release:
						case <-ctx.Done():
						}
					}
				}
				return provider.StreamSimple(ctx, model, c, options)
			}
			definitions := []durable.DocumentDefinition{
				{Namespace: "asof", Version: 1, Schema: json.RawMessage(`{"type":"integer"}`), Initial: json.RawMessage(`0`), History: durable.HistoryRewindable, Fork: durable.ForkAsOf, Reset: durable.ResetPreserve},
				{Namespace: "current", Version: 1, Schema: json.RawMessage(`{"type":"integer"}`), Initial: json.RawMessage(`0`), History: durable.HistoryLatest, Fork: durable.ForkCurrent, Reset: durable.ResetPreserve},
				{Namespace: "initial", Version: 1, Schema: json.RawMessage(`{"type":"integer"}`), Initial: json.RawMessage(`0`), History: durable.HistoryRewindable, Fork: durable.ForkInitial, Reset: durable.ResetInitial},
			}
			options := HostOptions{SessionDir: t.TempDir(), Stack: &StackConfig{Model: provider.Model(), StreamFn: streamFn, CustomPrompt: "Original instructions"}, Options: Options{Documents: definitions}}
			if backend == "memory" {
				options.Store = durable.NewMemory()
			}
			host, err := OpenHost(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			defer host.Close(context.Background())
			parent, err := host.Create(t.Context(), AgentOptions{Cwd: t.TempDir(), New: &NewSession{PermissionMode: permission.ModePlan}})
			if err != nil {
				t.Fatal(err)
			}
			setDocs := func(owner *Session, id, value string) {
				t.Helper()
				var changes []durable.Change
				for _, definition := range definitions {
					changes = append(changes, durable.Change{Kind: durable.DocumentKind, Namespace: definition.Namespace, ID: "default", Version: 1, Value: json.RawMessage(value)})
				}
				if _, err := owner.Commit(t.Context(), durable.Commit{ID: id, Changes: changes}); err != nil {
					t.Fatal(err)
				}
			}
			setDocs(parent, "before", "2")
			if result, err := parent.Prompt(t.Context(), Prompt{Text: "inherited needle"}); err != nil || result.Err != nil {
				t.Fatal(result, err)
			}
			entries, err := parent.core.store.Read(t.Context(), durable.Query{Kind: durable.EntryKind, Namespace: "_tempest/journal", Reverse: true, Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			cut := ""
			for _, entry := range entries.Records {
				var envelope struct{ Type, ID string }
				if err := json.Unmarshal(entry.Value, &envelope); err != nil {
					t.Fatal(err)
				}
				if envelope.Type == "message" {
					cut = envelope.ID
					break
				}
			}
			if cut == "" {
				t.Fatal("missing durable message id")
			}
			inherited := parent.core.agent.State().Messages
			setDocs(parent, "after", "3")
			if err := parent.SetPermissionMode(permission.ModeBypass); err != nil {
				t.Fatal(err)
			}
			run, err := parent.Begin(t.Context(), Prompt{Text: "parent continues"})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("parent didn't enter held provider request")
			}
			if _, err := parent.FollowUp(t.Context(), QueuedInput{Text: "queued only on parent", Key: "parent-queue"}); err != nil {
				t.Fatal(err)
			}
			parent.core.permissionPolicy.Overrides["bash"] = permission.DecideAllow
			sibling, err := host.Fork(t.Context(), parent.ID(), cut, ForkOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if sibling.ID() == parent.ID() || !reflect.DeepEqual(sibling.core.agent.State().Messages, inherited) {
				t.Fatal("fork changed prefix or reused identity")
			}
			_, inheritedGrant := sibling.core.permissionPolicy.Overrides["bash"]
			if sibling.Snapshot().Usage.Total.TotalTokens != 0 || len(sibling.core.queues.Snapshot()) != 0 || inheritedGrant {
				t.Fatal("lifetime state leaked into fork")
			}
			for namespace, want := range map[string]string{"asof": "2", "current": "3", "initial": "0"} {
				page, err := sibling.Read(t.Context(), durable.Query{Kind: durable.DocumentKind, Namespace: namespace, ID: "default"})
				if err != nil || len(page.Records) != 1 || string(page.Records[0].Value) != want {
					t.Fatalf("%s=%+v err=%v", namespace, page, err)
				}
			}
			if !strings.Contains(sibling.core.agent.State().SystemPrompt, "Original instructions") {
				t.Fatal("as-of instructions missing")
			}
			if sibling.Snapshot().PermissionMode != string(permission.ModePlan) {
				t.Fatal("fork didn't restore as-of plan mode")
			}
			if result, err := sibling.Prompt(t.Context(), Prompt{Text: "independent sibling"}); err != nil || result.Err != nil {
				t.Fatal(result, err)
			}
			reset, err := parent.Reset(t.Context(), "new context", "reset-parent")
			if err != nil || reset.State != "accepted" {
				t.Fatal(reset, err)
			}
			releaseOnce.Do(func() { close(release) })
			if _, err := run.Wait(); err != nil {
				t.Fatal(err)
			}
			settled, err := parent.Submission(t.Context(), "reset-parent")
			if err != nil || settled.State != "completed" || len(parent.core.agent.State().Messages) != 0 {
				t.Fatal(settled, err)
			}
			if parent.Snapshot().Usage.Total.TotalTokens == 0 {
				t.Fatal("reset erased lifetime usage")
			}
			history, err := parent.SearchHistory(t.Context(), HistoryOptions{Query: "inherited needle", Limit: 100})
			if err != nil || len(history.Matches) == 0 {
				t.Fatal("reset hid audit text", history, err)
			}
			parentPath := parent.core.store.Path()
			siblingID := sibling.ID()
			if err := host.CloseSession(t.Context(), parent.ID()); err != nil {
				t.Fatal(err)
			}
			if backend == "jsonl" {
				if err := os.Remove(parentPath); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(parentPath + ".d"); err != nil {
					t.Fatal(err)
				}
			}
			if err := host.CloseSession(t.Context(), siblingID); err != nil {
				t.Fatal(err)
			}
			reopened, err := host.Open(t.Context(), siblingID)
			if err != nil {
				t.Fatal("parent removal broke pinned archive", err)
			}
			if len(reopened.core.agent.State().Messages) < len(inherited) || !reflect.DeepEqual(reopened.core.agent.State().Messages[:len(inherited)], inherited) {
				t.Fatal("parent mutation changed pinned prefix")
			}
			grandchild, err := host.Fork(t.Context(), siblingID, cut, ForkOptions{})
			if err != nil {
				t.Fatal("forking an inherited ancestral entry", err)
			}
			if !reflect.DeepEqual(grandchild.core.agent.State().Messages, inherited) {
				t.Fatal("ancestral fork retained the sibling's tail")
			}
			greatGrandchild, err := host.Fork(t.Context(), grandchild.ID(), cut, ForkOptions{})
			if err != nil || !reflect.DeepEqual(greatGrandchild.core.agent.State().Messages, inherited) {
				t.Fatal("ancestral archive chain was not resolved", err)
			}
			for namespace, want := range map[string]string{"asof": "2", "current": "3", "initial": "0"} {
				page, err := grandchild.Read(t.Context(), durable.Query{Kind: durable.DocumentKind, Namespace: namespace, ID: "default"})
				if err != nil || len(page.Records) != 1 || string(page.Records[0].Value) != want {
					t.Fatalf("ancestral %s=%+v err=%v", namespace, page, err)
				}
			}
			sessionIDsMu.Lock()
			defer sessionIDsMu.Unlock()
			if !sessionIDs[parent.ID()] || !sessionIDs[siblingID] {
				t.Fatal("provider session identity wasn't renewed", sessionIDs)
			}
		})
	}
}

func TestResetAndHandoffWaitForWholeToolBatch(t *testing.T) {
	for _, control := range []string{"reset", "handoff"} {
		t.Run(control, func(t *testing.T) {
			provider := faux.New(faux.Options{})
			provider.SetResponses(faux.Respond(faux.ToolCall("first", json.RawMessage(`{}`), "first-call"), faux.ToolCall("second", json.RawMessage(`{}`), "second-call")), faux.Respond(faux.Text("must not request another turn")))
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			tool := func(name string) Tool {
				return Tool{Tool: types.Tool{Name: name, Parameters: json.RawMessage(`{"type":"object"}`)}, Execute: func(ctx context.Context, _ string, _ json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
					if name == "second" {
						close(entered)
						select {
						case <-release:
						case <-ctx.Done():
							return agent.AgentToolResult{}, ctx.Err()
						}
					}
					result := agent.AgentToolResult{Content: []types.ContentBlock{types.NewText(name + " result")}}
					if name == "first" && control == "handoff" {
						result.Control = "handoff"
					}
					return result, nil
				}}
			}
			owner, err := Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{Tools: []Tool{tool("first"), tool("second")}, PermissionMode: permission.ModeBypass}, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close(context.Background())
			run, err := owner.Begin(t.Context(), Prompt{Text: "run batch"})
			if err != nil {
				t.Fatal(err)
			}
			<-entered
			if control == "reset" {
				if _, err := owner.Reset(t.Context(), "after batch", "batch-reset"); err != nil {
					t.Fatal(err)
				}
			}
			if len(owner.core.agent.State().Messages) == 0 {
				t.Fatal("context changed before sibling settled")
			}
			releaseOnce.Do(func() { close(release) })
			if result, err := run.Wait(); err != nil || result.Err != nil {
				t.Fatal(result, err)
			}
			if provider.CallCount() != 1 || len(owner.core.agent.State().Messages) != 0 {
				t.Fatal("control continued generation or failed to reset")
			}
			loaded, err := owner.core.store.Load(t.Context())
			if err != nil || loaded.UsageTotal.TotalTokens == 0 {
				t.Fatal(loaded, err)
			}
			history, err := owner.SearchHistory(t.Context(), HistoryOptions{Query: "result", Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			results := 0
			for _, match := range history.Matches {
				var entry struct{ Type string }
				if err := json.Unmarshal(match.Entry, &entry); err != nil {
					t.Fatal(err)
				}
				if entry.Type == "message" {
					results++
				}
			}
			if results != 2 {
				t.Fatal("whole batch results weren't retained", history)
			}
		})
	}
}
