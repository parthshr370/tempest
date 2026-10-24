package acp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest"
)

func TestACPResetAdmissionPrecedesFollowingPrompt(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.Text("prompt survived reset")))
	owner, err := tempest.Open(t.Context(), tempest.Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &tempest.NewSession{}, Stack: &tempest.StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	client := NewClient(nil, checkpointBackend{session: owner}, NewBroker(time.Second, nil))
	client.initialized = true
	raw, _ := json.Marshal(map[string]any{"sessionId": owner.ID(), "note": "fresh", "requestId": "reset-before-prompt"})
	result, wire := client.admit(t.Context(), "_tempest/durable/reset", raw)
	if wire != nil {
		t.Fatal(wire)
	}
	if _, deferred := result.(rpc.Deferred); deferred {
		t.Fatal("reset admission was deferred past following prompt")
	}
	receipt, err := owner.Submission(t.Context(), "reset-before-prompt")
	if err != nil || receipt.State != "completed" {
		t.Fatal(receipt, err)
	}
	raw, _ = json.Marshal(map[string]any{"sessionId": owner.ID(), "prompt": []contentBlock{{Type: "text", Text: "keep this prompt"}}, "_meta": map[string]any{"tempest": map[string]any{"requestId": "prompt-after-reset"}}})
	result, wire = client.admit(t.Context(), "session/prompt", raw)
	if wire != nil {
		t.Fatal(wire)
	}
	if deferred, ok := result.(rpc.Deferred); ok {
		if _, wire := deferred(); wire != nil {
			t.Fatal(wire)
		}
	}
	settled, err := owner.WaitSubmission(t.Context(), "prompt-after-reset")
	if err != nil || settled.State != "completed" || settled.Answer != "prompt survived reset" || provider.CallCount() != 1 {
		t.Fatal("ACP prompt lost", settled, err, provider.CallCount())
	}
}

func TestACPForkRejectsInvalidWorkspace(t *testing.T) {
	provider := faux.New(faux.Options{})
	owner, err := tempest.Open(t.Context(), tempest.Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &tempest.NewSession{}, Stack: &tempest.StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	client := NewClient(nil, checkpointBackend{session: owner}, nil)
	client.initialized = true
	for _, cwd := range []string{"relative/workspace", filepath.Join(t.TempDir(), "missing")} {
		raw, _ := json.Marshal(map[string]any{"sessionId": owner.ID(), "entryId": "unused", "forkOptions": map[string]any{"cwd": cwd}})
		_, wire := client.Handle(t.Context(), "_tempest/durable/fork", raw)
		if wire == nil || wire.Code != -32602 {
			t.Fatalf("invalid cwd %q accepted: %v", cwd, wire)
		}
	}
}
