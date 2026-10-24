package conformance

import (
	"encoding/json"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/base"
)

func retryHistoryCleanup(t *testing.T, factory Factory) {
	if factory.Dialect != "responses" && factory.Dialect != "bedrock" {
		t.Skip("Responses and Converse request replay regression")
	}
	partial := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"discarded partial attempt\"}\n\ndata: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_dead\",\"name\":\"lookup\"}}\n\n"
	if factory.Dialect == "bedrock" {
		partial = "event: contentBlockDelta\ndata: {\"contentBlockIndex\":0,\"delta\":{\"text\":\"discarded partial attempt\"}}\n\nevent: contentBlockStart\ndata: {\"contentBlockIndex\":1,\"start\":{\"toolUse\":{\"toolUseId\":\"call_dead\",\"name\":\"lookup\"}}}\n\n"
	}
	failedEndpoint := server(t, factory, partial, nil)
	routed := model(factory, failedEndpoint.URL)
	history := conversation()
	failed := result(t, factory, routed, history, options())
	if failed.StopReason != types.StopError || len(failed.Content) != 2 {
		t.Fatalf("expected retained partial failed attempt: %+v", failed)
	}
	requests := make(chan json.RawMessage, 1)
	endpoint := server(t, factory, fixture(t, factory, "text"), requests)
	routed.BaseURL = endpoint.URL
	invalidID := "foreign.call/" + strings.Repeat("x", 70)
	orphanID := "orphan.call/" + strings.Repeat("y", 70)
	history.Messages = append(history.Messages, failed,
		types.AssistantMessage{StopReason: types.StopAborted, Content: []types.ContentBlock{types.NewText("discarded aborted attempt"), types.NewToolCall("call_aborted", "lookup", json.RawMessage(`{}`))}},
		types.AssistantMessage{Provider: "foreign", Content: []types.ContentBlock{types.NewToolCall(invalidID, "lookup", json.RawMessage(`{}`)), types.NewToolCall(orphanID, "lookup", json.RawMessage(`{}`))}},
		types.ToolResultMessage{ToolCallID: invalidID, Content: []types.ContentBlock{types.NewText("result"), types.NewImage("aW1hZ2U=", "image/png")}},
		types.UserMessage{Content: types.BlockContent(types.NewText("retry"), types.NewImage("aW1hZ2U=", "image/png"))})
	message := result(t, factory, routed, history, options())
	if message.StopReason != types.StopStop {
		t.Fatalf("retry failed: %+v", message)
	}
	body := string(<-requests)
	for _, absent := range []string{"call_dead", "call_aborted", "discarded partial attempt", "discarded aborted attempt", invalidID, orphanID, "aW1hZ2U="} {
		if strings.Contains(body, absent) {
			t.Errorf("replayed %q: %s", absent, body)
		}
	}
	for _, present := range []string{"No result provided", "image omitted: model does not support images", "tool image omitted: model does not support images"} {
		if !strings.Contains(body, present) {
			t.Errorf("missing %q: %s", present, body)
		}
	}
	for _, id := range []string{invalidID, orphanID} {
		normalized := base.NormalizeToolCallID(id, routed, types.AssistantMessage{})
		if strings.Count(body, normalized) != 2 {
			t.Errorf("tool ID not paired: %s", body)
		}
	}
}
