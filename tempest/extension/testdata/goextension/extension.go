// Package goextension is an external-style implementation of the shared fixture.
package goextension

import (
	"context"
	"encoding/json"

	"go.harness.dev/harness/tempest/durable"
	"go.harness.dev/harness/tempest/extension"
)

// Definition uses only public leaf contracts, like an independently built module.
func Definition() extension.Definition {
	return extension.Definition{
		ID: "sample", Revision: "v1",
		Sections:  []extension.Section{{Name: "instructions", Render: func(context.Context, extension.ReadView) (string, error) { return "Extension section", nil }}},
		Documents: []durable.DocumentDefinition{{Namespace: "sample", Version: 1, Schema: json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}`), Initial: json.RawMessage(`{"count":0}`), History: durable.HistoryRewindable, Fork: durable.ForkAsOf, Reset: durable.ResetPreserve}},
		Tools: []extension.Tool{{Name: "echo", Description: "Echo a message", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`), OutputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`), Effect: extension.EffectWrite, Replay: extension.ReplaySafe, Execute: func(ctx context.Context, call extension.Call, inv extension.Invocation) (extension.Result, error) {
			for _, memo := range []struct {
				key   string
				value json.RawMessage
			}{{"false", json.RawMessage(`false`)}, {"null", json.RawMessage(`null`)}} {
				if _, err := inv.Memo(ctx, memo.key, memo.value); err != nil {
					return extension.Result{}, err
				}
			}
			identity := inv.Identity()
			if _, err := inv.Commit(ctx, durable.Commit{ID: identity.InvocationID + ":document", Changes: []durable.Change{{SessionID: identity.SessionID, Kind: durable.DocumentKind, Namespace: "sample", ID: "state", Version: 1, Value: json.RawMessage(`{"count":1}`)}}}); err != nil {
				return extension.Result{}, err
			}
			var arguments struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
				return extension.Result{}, err
			}
			structured, err := json.Marshal(arguments)
			if err != nil {
				return extension.Result{}, err
			}
			return extension.Result{Content: []extension.Content{{Type: "text", Text: arguments.Text}}, StructuredContent: structured}, nil
		}}},
		Hooks: []extension.Hook{{Name: "approval", Event: extension.BeforeTool, Replay: extension.ReplaySafe, Handle: func(ctx context.Context, _ extension.HookInput, inv extension.Invocation) (extension.HookDecision, error) {
			_, err := inv.Memo(ctx, "approval", json.RawMessage(`true`))
			return extension.HookDecision{Kind: "continue"}, err
		}}},
		Wraps: []extension.Wrap{{Name: "brackets", Target: "sample/echo", Replay: extension.ReplaySafe, Handle: func(ctx context.Context, _ extension.Call, _ extension.Invocation, next extension.Next) (extension.Result, error) {
			result, err := next.Call(ctx)
			if err != nil {
				return result, err
			}
			result.Content[0].Text = "[" + result.Content[0].Text + "]"
			var structured struct {
				Text string `json:"text"`
			}
			if err = json.Unmarshal(result.StructuredContent, &structured); err != nil {
				return result, err
			}
			structured.Text = "[" + structured.Text + "]"
			result.StructuredContent, err = json.Marshal(structured)
			return result, err
		}}},
	}
}
