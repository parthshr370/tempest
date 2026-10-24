package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/types"
)

func TestLoadFauxScript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "script.json")
	if err := os.WriteFile(path, []byte(`[{"text":"hello"},{"toolCalls":[{"name":"read","arguments":{"path":"x"}}]}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	provider, model, err := loadFauxScript(path)
	if err != nil {
		t.Fatal(err)
	}
	if model.Provider != "faux" || provider.PendingCount() != 2 {
		t.Fatalf("model/provider = %+v pending=%d", model, provider.PendingCount())
	}
}

func TestFauxScriptErrorAndCancellableFileWait(t *testing.T) {
	for _, operation := range []string{"error", "release", "cancel"} {
		t.Run(operation, func(t *testing.T) {
			path, gate := filepath.Join(t.TempDir(), "script.json"), filepath.Join(t.TempDir(), "release")
			response := fauxScriptResponse{Error: "context_length_exceeded"}
			if operation != "error" {
				response.WaitForFile, response.Text, response.Error = gate, "released", ""
			}
			raw, err := json.Marshal([]fauxScriptResponse{response})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			provider, model, err := loadFauxScript(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			output := provider.StreamSimple(ctx, model, types.Context{}, nil)
			done := make(chan *types.AssistantMessage, 1)
			go func() {
				for range output.Events() {
				}
				message, _ := output.Result(context.Background())
				done <- message
			}()
			if operation != "error" {
				select {
				case <-done:
					t.Fatal("file wait didn't hold the response")
				case <-time.After(20 * time.Millisecond):
				}
				if operation == "release" {
					if err := os.WriteFile(gate, nil, 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					cancel()
				}
			}
			select {
			case message := <-done:
				if message == nil || operation == "error" && message.ErrorMessage != "context_length_exceeded" || operation == "cancel" && message.ErrorMessage != context.Canceled.Error() || operation == "release" && message.StopReason != types.StopStop {
					t.Fatalf("%s response: %+v", operation, message)
				}
			case <-time.After(time.Second):
				t.Fatal("response didn't finish after release or cancellation")
			}
		})
	}
}
