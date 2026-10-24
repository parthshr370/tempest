package acp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest"
	"go.harness.dev/harness/tempest/durable"
)

func TestDurableIdentityPreservesOriginalContentBlocks(t *testing.T) {
	a, err := convertPrompt([]contentBlock{{Type: "text", Text: "first"}, {Type: "text", Text: "second"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := convertPrompt([]contentBlock{{Type: "text", Text: "first\n\nsecond"}})
	if err != nil {
		t.Fatal(err)
	}
	if a.Text != b.Text {
		t.Fatal("fixture does not test lossy text normalization")
	}
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	leftHash, e := durable.SubmissionHash("input", left)
	if e != nil {
		t.Fatal(e)
	}
	rightHash, e := durable.SubmissionHash("input", right)
	if e != nil {
		t.Fatal(e)
	}
	if leftHash == rightHash {
		t.Fatal("distinct content block orders collapsed to one durable request")
	}
}

func TestImageIdentityDoesNotDuplicatePayload(t *testing.T) {
	image := strings.Repeat("a", 614400)
	prompt, err := convertPrompt([]contentBlock{{Type: "image", MimeType: "image/png", Data: image}})
	if err != nil {
		t.Fatal(err)
	}
	if len(prompt.ContentIdentity) != len(`"sha256:"`)+64 || strings.Contains(string(prompt.ContentIdentity), image[:100]) {
		t.Fatalf("unbounded content identity: %d bytes", len(prompt.ContentIdentity))
	}
	if len(prompt.Media) != 1 || prompt.Media[0].Data != image {
		t.Fatal("identity digest changed executable image payload")
	}
	raw, marshalErr := json.Marshal(prompt)
	if marshalErr != nil || len(raw) > 700000 {
		t.Fatalf("image prompt encoded %d bytes: %v", len(raw), marshalErr)
	}
}

func TestKeyedPromptRetrySchedulesAcceptedSubmission(t *testing.T) {
	for _, method := range []string{"session/prompt", "_tempest/durable/submit"} {
		t.Run(method, func(t *testing.T) {
			provider := faux.New(faux.Options{})
			provider.SetResponses(faux.Respond(faux.Text("recovered accepted input")))
			store := durable.NewMemory()
			session, err := tempest.Open(t.Context(), tempest.Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), Store: store, New: &tempest.NewSession{}, Stack: &tempest.StackConfig{Model: provider.Model(), StreamFn: provider.StreamSimple}})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close(context.Background())
			blocks := []contentBlock{{Type: "text", Text: "accepted before crash"}}
			prompt, wire := convertPrompt(blocks)
			if wire != nil {
				t.Fatal(wire)
			}
			payload, _ := json.Marshal(prompt)
			hash, err := durable.SubmissionHash("input", payload)
			if err != nil {
				t.Fatal(err)
			}
			value, _ := json.Marshal(durable.Submission{ID: "stable-submission", RequestID: "retry", State: "accepted", Revision: 1, RequestHash: hash, Placement: "input", Payload: payload})
			_, err = store.Commit(t.Context(), durable.Commit{ID: "test-admission", Changes: []durable.Change{{SessionID: session.ID(), Kind: durable.SubmissionKind, Namespace: "_tempest/submissions", ID: "retry", Version: 1, Value: value}}})
			if err != nil {
				t.Fatal(err)
			}
			client := NewClient(nil, checkpointBackend{session: session}, NewBroker(time.Second, nil))
			client.initialized = true
			params := map[string]any{"sessionId": session.ID(), "prompt": blocks}
			if method == "session/prompt" {
				params["_meta"] = map[string]any{"tempest": map[string]any{"requestId": "retry"}}
			} else {
				params["requestId"] = "retry"
			}
			raw, _ := json.Marshal(params)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			result, wire := client.Handle(ctx, method, raw)
			if wire != nil {
				t.Fatal(wire)
			}
			if deferred, ok := result.(rpc.Deferred); ok {
				if _, wire := deferred(); wire != nil {
					t.Fatal(wire)
				}
			}
			settled, err := session.WaitSubmission(ctx, "retry")
			if err != nil || settled.State != "completed" || settled.Answer != "recovered accepted input" {
				t.Fatalf("retry did not schedule accepted input: %+v %v", settled, err)
			}
		})
	}
}
