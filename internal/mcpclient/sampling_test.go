package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/mcpconfig"
)

// callSample runs the fixture's sample tool with value as the
// sampling/createMessage params ("" sends the fixture's default request)
// and returns the text the server got back.
func callSample(t *testing.T, manager *Manager, value string) string {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"value": value})
	result, err := fixtureTool(t, manager, "sample").Execute(context.Background(), "sample", args, nil)
	if err != nil || result.IsError || len(result.Content) == 0 {
		t.Fatalf("sample: %#v %v", result, err)
	}
	return result.Content[0].Text
}

func TestMCPSamplingCapabilityOnlyForOptedInServers(t *testing.T) {
	var calls atomic.Int64
	sampler := func(context.Context, SamplingRequest) (types.AssistantMessage, error) {
		calls.Add(1)
		return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("x")}}, nil
	}
	for name, configure := range map[string]func(*mcpconfig.Server, *Options){
		"server not opted in": func(_ *mcpconfig.Server, options *Options) { options.Sample = sampler },
		"no sampler":          func(config *mcpconfig.Server, _ *Options) { config.Sampling = true },
	} {
		t.Run(name, func(t *testing.T) {
			manager, _ := openFixture(t, "normal", configure)
			if got := callSample(t, manager, ""); got != "sampling not declared" {
				t.Fatalf("reply = %q", got)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("sampler ran %d times without the capability", calls.Load())
	}
}

func TestMCPSamplingRequestIsConvertedAndAnswered(t *testing.T) {
	var got SamplingRequest
	manager, _ := openFixture(t, "normal", func(config *mcpconfig.Server, options *Options) {
		config.Sampling = true
		options.Sample = func(_ context.Context, request SamplingRequest) (types.AssistantMessage, error) {
			got = request
			return types.AssistantMessage{Content: []types.ContentBlock{types.NewText("the fixture "), types.NewText("is fine")}, Model: "smol-model", StopReason: types.StopLength}, nil
		}
	})
	var reply struct {
		Role    string `json:"role"`
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Model      string `json:"model"`
		StopReason string `json:"stopReason"`
	}
	raw := callSample(t, manager, "")
	if err := json.Unmarshal([]byte(raw), &reply); err != nil {
		t.Fatalf("reply %q: %v", raw, err)
	}
	if reply.Role != "assistant" || reply.Content.Type != "text" || reply.Content.Text != "the fixture is fine" || reply.Model != "smol-model" || reply.StopReason != "maxTokens" {
		t.Fatalf("reply = %+v", reply)
	}
	// includeContext "allServers" adds nothing: only the server's own
	// messages reach the sampler.
	if got.Server != "fixture" || len(got.ID) != 32 || got.Source() != "mcp-sampling:fixture:"+got.ID || got.SystemPrompt != "You are terse." || got.MaxTokens != 100 || len(got.Messages) != 1 {
		t.Fatalf("request = %+v", got)
	}
	user, ok := got.Messages[0].(types.UserMessage)
	if !ok || len(user.Content.Blocks) != 1 || user.Content.Blocks[0].Text != "Summarize the fixture" {
		t.Fatalf("messages = %#v", got.Messages)
	}

	// Content arrays (2025-11-25) and assistant turns are carried in order.
	multi := `{"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","data":"iVBORw0KGgo=","mimeType":"image/png"}]},{"role":"assistant","content":{"type":"text","text":"a pixel"}},{"role":"user","content":{"type":"text","text":"and?"}}],"maxTokens":5}`
	callSample(t, manager, multi)
	if len(got.Messages) != 3 || got.MaxTokens != 5 {
		t.Fatalf("multi request = %+v", got)
	}
	if user, ok := got.Messages[0].(types.UserMessage); !ok || len(user.Content.Blocks) != 2 || user.Content.Blocks[1].Type != types.BlockImage || user.Content.Blocks[1].MimeType != "image/png" {
		t.Fatalf("first message = %#v", got.Messages[0])
	}
	if assistant, ok := got.Messages[1].(types.AssistantMessage); !ok || assistant.Content[0].Text != "a pixel" {
		t.Fatalf("second message = %#v", got.Messages[1])
	}
}

func TestMCPSamplingRefusalsAndInvalidRequests(t *testing.T) {
	var calls atomic.Int64
	manager, _ := openFixture(t, "normal", func(config *mcpconfig.Server, options *Options) {
		config.Sampling = true
		options.Sample = func(context.Context, SamplingRequest) (types.AssistantMessage, error) {
			calls.Add(1)
			return types.AssistantMessage{}, fmt.Errorf("%w: the user declined sampling", ErrSamplingRefused)
		}
	})
	if got := callSample(t, manager, ""); !strings.HasPrefix(got, "sampling error:") || !strings.Contains(got, "the user declined sampling") || !strings.Contains(got, "-1") {
		t.Fatalf("refusal = %q", got)
	}
	for name, params := range map[string]string{
		"tools":        `{"messages":[{"role":"user","content":{"type":"text","text":"x"}}],"maxTokens":10,"tools":[{"name":"t","inputSchema":{"type":"object"}}]}`,
		"tool_use":     `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"1","name":"t","input":{}}]}],"maxTokens":10}`,
		"no maxTokens": `{"messages":[{"role":"user","content":{"type":"text","text":"x"}}]}`,
		"audio":        `{"messages":[{"role":"user","content":{"type":"audio","data":"AAAA","mimeType":"audio/wav"}}],"maxTokens":10}`,
	} {
		if got := callSample(t, manager, params); !strings.HasPrefix(got, "sampling error:") || !strings.Contains(got, "-32602") {
			t.Fatalf("%s: reply = %q", name, got)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("sampler ran %d times; invalid requests must not reach it", calls.Load())
	}
}
