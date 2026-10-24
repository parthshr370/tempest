package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/question"
	"go.harness.dev/harness/internal/rpc"
)

func TestAskMCPFormAndHeadless(t *testing.T) {
	for _, revision := range []string{"2025-06-18", "2025-11-25"} {
		for _, supported := range []bool{false, true} {
			t.Run(revision+"/"+fmt.Sprint(supported), func(t *testing.T) {
				provider := faux.New(faux.Options{})
				provider.AppendResponses(faux.Respond(faux.ToolCall("ask", json.RawMessage(`{"questions":[{"id":"q","question":"Choose","options":[{"label":"one","description":"First"},{"label":"two"}],"multi":true,"allow_text":true}]}`), "ask-call")))
				provider.AppendResponses(func(conversation types.Context, _ *types.StreamOptions, _ faux.State, _ types.Model) (types.AssistantMessage, error) {
					var found bool
					for _, message := range conversation.Messages {
						if result, ok := message.(types.ToolResultMessage); ok && result.ToolName == "ask" {
							encoded, err := json.Marshal(result)
							if err != nil {
								return types.AssistantMessage{}, err
							}
							if supported {
								found = bytes.Contains(encoded, []byte("context")) && !result.IsError
							} else {
								found = result.IsError && bytes.Contains(encoded, []byte("best judgment"))
							}
						}
					}
					if !found {
						return types.AssistantMessage{}, fmt.Errorf("ask result missing or incorrect")
					}
					return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("answered")}}, nil
				})
				capabilities := json.RawMessage(`{}`)
				if supported {
					capabilities = json.RawMessage(`{"elicitation":{"form":{}}}`)
					if revision == "2025-06-18" {
						capabilities = json.RawMessage(`{"elicitation":{}}`)
					}
				}
				var requests atomic.Int32
				connection := connect(t, options(t, provider), revision, capabilities, func(_ context.Context, method string, raw json.RawMessage) (interface{}, *rpc.Error) {
					if method != "elicitation/create" {
						return nil, nil
					}
					requests.Add(1)
					var form question.Elicitation
					if err := json.Unmarshal(raw, &form); err != nil {
						return nil, rpc.NewError(-32602, "invalid", err.Error())
					}
					expectedMode := "form"
					if revision == "2025-06-18" {
						expectedMode = ""
					}
					if form.Mode != expectedMode || form.SessionID != "" {
						return nil, rpc.NewError(-32602, "invalid", "wrong MCP mode or ACP scope")
					}
					if revision == "2025-06-18" {
						if form.RequestedSchema.Properties["choice_0"].Type != "boolean" {
							return nil, rpc.NewError(-32602, "invalid", "legacy form contains an array enum")
						}
						return json.RawMessage(`{"action":"accept","content":{"choice_0":true,"choice_1":true,"text":"context"}}`), nil
					}
					return json.RawMessage(`{"action":"accept","content":{"selected":["one","two"],"text":"context"}}`), nil
				})
				result, err := connection.run(RunInput{Prompt: "ask the user"}, nil)
				if err != nil || result.IsError || result.StructuredContent == nil || result.StructuredContent.Text != "answered" {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				want := int32(0)
				if supported {
					want = 1
				}
				if requests.Load() != want {
					t.Fatalf("elicitation calls=%d want=%d", requests.Load(), want)
				}
			})
		}
	}
}
