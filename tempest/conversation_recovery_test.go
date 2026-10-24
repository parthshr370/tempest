package tempest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/compaction"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest/durable"
)

func TestCheckpointRewindsDocumentsNotGrantsOrSubmissions(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.Text("first")), faux.Respond(faux.Text("second")))
	definitions := []durable.DocumentDefinition{{Namespace: "rewindable", Version: 1, Schema: json.RawMessage(`{"type":"integer"}`), Initial: json.RawMessage(`0`), History: durable.HistoryRewindable, Fork: durable.ForkAsOf, Reset: durable.ResetPreserve}, {Namespace: "latest", Version: 1, Schema: json.RawMessage(`{"type":"integer"}`), Initial: json.RawMessage(`0`), History: durable.HistoryLatest, Fork: durable.ForkCurrent, Reset: durable.ResetPreserve}}
	owner, err := Open(t.Context(), Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{}, Documents: definitions, Stack: &StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	update := func(value string) {
		t.Helper()
		if _, err := owner.Commit(t.Context(), durable.Commit{ID: "documents:" + value, Changes: []durable.Change{{Kind: durable.DocumentKind, Namespace: "rewindable", ID: "default", Version: 1, Value: json.RawMessage(value)}, {Kind: durable.DocumentKind, Namespace: "latest", ID: "default", Version: 1, Value: json.RawMessage(value)}}}); err != nil {
			t.Fatal(err)
		}
	}
	update("1")
	if _, err := owner.Prompt(t.Context(), Prompt{Text: "checkpoint input"}); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := owner.CreateCheckpoint(t.Context(), "documents at one")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := owner.core.store.ContextEpoch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	update("2")
	submission, err := owner.Submit(t.Context(), Input{RequestID: "after-checkpoint", Prompt: Prompt{Text: "second input"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.WaitSubmission(t.Context(), submission.RequestID); err != nil {
		t.Fatal(err)
	}
	queued, err := owner.FollowUp(t.Context(), QueuedInput{Text: "preserved queued input", Key: "queued-after-checkpoint"})
	if err != nil {
		t.Fatal(err)
	}
	owner.core.permissionPolicy.Overrides["bash"] = permission.DecideAllow
	usage := owner.Snapshot().Usage.Total.TotalTokens
	if err := owner.Rewind(t.Context(), RewindOptions{Checkpoint: checkpoint.ID}); err != nil {
		t.Fatal(err)
	}
	for namespace, want := range map[string]string{"rewindable": "1", "latest": "2"} {
		page, err := owner.Read(t.Context(), durable.Query{Kind: durable.DocumentKind, Namespace: namespace, ID: "default"})
		if err != nil || len(page.Records) != 1 || string(page.Records[0].Value) != want {
			t.Fatal(namespace, page, err)
		}
	}
	if current, err := owner.Submission(t.Context(), "after-checkpoint"); err != nil || current.State != "completed" {
		t.Fatal(current, err)
	}
	pending := owner.core.queues.Snapshot()
	if len(pending) != 1 || pending[0].ID != queued.ID || owner.core.permissionPolicy.Overrides["bash"] != permission.DecideAllow || owner.Snapshot().Usage.Total.TotalTokens != usage {
		t.Fatal("rewind changed lifetime facts")
	}
	nextEpoch, err := owner.core.store.ContextEpoch(t.Context())
	if err != nil || nextEpoch == epoch {
		t.Fatal("rewind didn't invalidate context epoch", err)
	}
}

func TestProviderOverflowRetriesOncePerInputAfterRestart(t *testing.T) {
	provider := faux.New(faux.Options{})
	var foreground, summary atomic.Int32
	streamFn := func(ctx context.Context, model types.Model, c types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
		if c.SystemPrompt == compaction.SummarizationSystemPrompt {
			summary.Add(1)
			summarizer := faux.New(faux.Options{})
			summarizer.SetResponses(faux.Respond(faux.Text("compact paid summary")))
			return summarizer.StreamSimple(ctx, model, c, options)
		}
		foreground.Add(1)
		failed := stream.NewAssistantStream(model.API, model.Provider, model.ID)
		failed.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopError, Err: &types.AssistantMessage{Model: model.ID, Provider: model.Provider, API: model.API, StopReason: types.StopError, ErrorMessage: "context_length_exceeded"}})
		failed.End()
		return failed
	}
	settings := CompactionSettings{Enabled: true, ReserveTokens: 1000, KeepRecentTokens: 10}
	options := Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{}, Stack: &StackConfig{Model: provider.Model(), StreamFn: streamFn, Compaction: &settings}}
	owner, err := Open(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	// Seed completed history so the provider's overflow has a useful older cut.
	for index := range 3 {
		if err := owner.core.store.AppendMessage(t.Context(), types.UserMessage{Content: types.StringContent(strings.Repeat(fmt.Sprintf("older %d ", index), 30))}); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := owner.core.store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner.core.agent.SetMessages(loaded.Messages)
	first, err := owner.Prompt(t.Context(), Prompt{Text: "new input needs overflow recovery"})
	if err != nil || first.Err == nil || foreground.Load() != 2 || summary.Load() == 0 {
		t.Fatal(first, err, foreground.Load(), summary.Load())
	}
	id := owner.ID()
	paid := summary.Load()
	if err := owner.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	options.New = nil
	options.Resume = &ResumeSession{SessionID: id}
	resumed, err := Open(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close(context.Background())
	result, err := resumed.Continue(t.Context())
	if err == nil || !strings.Contains(err.Error(), "Cannot continue without pending input") || foreground.Load() != 2 || summary.Load() != paid {
		t.Fatal("finished input was resurrected after restart", result, err, foreground.Load(), summary.Load())
	}
	claimed, err := resumed.core.store.ClaimOverflow(t.Context())
	if err != nil || claimed {
		t.Fatal("restart replenished overflow retry", claimed, err)
	}
}
