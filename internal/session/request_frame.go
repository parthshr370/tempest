package session

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/session/journal"
	"go.harness.dev/harness/tempest/durable"
)

func recordRequestFrames(store *journal.Store, next agent.StreamFn) agent.StreamFn {
	if store == nil {
		return next
	}
	var mu sync.Mutex
	return func(ctx context.Context, model types.Model, providerContext types.Context, options *types.SimpleStreamOptions) *stream.AssistantStream {
		mu.Lock()
		prompt := struct {
			System string       `json:"system"`
			Tools  []types.Tool `json:"tools"`
		}{providerContext.SystemPrompt, providerContext.Tools}
		promptDigest, promptErr := stableHash(prompt)
		epoch, err := store.ContextEpoch(ctx)
		if err == nil {
			err = promptErr
		}
		if err == nil {
			var digest string
			digest, err = stableHash(providerContext.Messages)
			if err == nil {
				frame := struct {
					Model         string                     `json:"model"`
					Provider      string                     `json:"provider"`
					Epoch         string                     `json:"epoch"`
					TranscriptCut uint64                     `json:"transcriptCut"`
					MessageDigest string                     `json:"messageDigest"`
					MessageCount  int                        `json:"messageCount"`
					PromptDigest  string                     `json:"promptDigest"`
					Options       *types.SimpleStreamOptions `json:"options,omitempty"`
				}{Model: model.ID, Provider: model.Provider, Epoch: epoch, TranscriptCut: store.Position().Seq, MessageDigest: digest, MessageCount: len(providerContext.Messages), PromptDigest: promptDigest}
				if options != nil {
					sanitized := *options
					sanitized.Headers = nil
					sanitized.Metadata = nil
					sanitized.Env = nil
					sanitized.APIKey = ""
					sanitized.Query = nil
					frame.Options = &sanitized
				}
				var raw []byte
				raw, err = json.Marshal(frame)
				if err == nil {
					id := store.NewRunID()
					changes := []durable.Change{{SessionID: store.ID(), Kind: durable.EntryKind, Namespace: "_tempest/requests", ID: id, Version: 1, Value: raw}}
					var prior durable.Page
					prior, err = store.Read(ctx, durable.Query{Kind: durable.EntryKind, Namespace: "_tempest/request-prompts", ID: promptDigest, Limit: 1})
					if err == nil && len(prior.Records) == 0 {
						var payload []byte
						payload, err = json.Marshal(prompt)
						changes = append(changes, durable.Change{SessionID: store.ID(), Kind: durable.EntryKind, Namespace: "_tempest/request-prompts", ID: promptDigest, Version: 1, Value: payload})
					}
					if err == nil {
						_, err = store.Commit(ctx, durable.Commit{ID: "request-frame:" + id, Changes: changes})
					}
				}
			}
		}
		mu.Unlock()
		if err == nil {
			return next(ctx, model, providerContext, options)
		}
		failed := stream.NewAssistantStream(model.API, model.Provider, model.ID)
		failed.Push(types.StreamEvent{Type: types.EvError, Reason: types.StopError, Err: &types.AssistantMessage{API: model.API, Provider: model.Provider, Model: model.ID, StopReason: types.StopError, ErrorMessage: fmt.Sprintf("persist request frame: %v", err), Timestamp: time.Now().UnixMilli()}})
		failed.End()
		return failed
	}
}
