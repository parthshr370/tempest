package adapter

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/engine/stream"
	ptypes "go.harness.dev/harness/internal/engine/types"
)

// A reasoning level never reaches a model the catalog lists without
// reasoning support, and the operator hears about it once, not per call.
func TestRouterDropsReasoningForNonReasoningModelWithOneWarning(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var seen []string
	streamSimple := func(_ context.Context, model ptypes.Model, _ ptypes.Context, opts *ptypes.SimpleStreamOptions) *stream.AssistantStream {
		seen = append(seen, opts.Reasoning)
		return erroredStream(model, context.Canceled)
	}
	router := ProviderRouter{}.withStream(streamSimple, "key", "", nil)
	plain := ptypes.Model{ID: "plain", Provider: "openrouter"}
	thinking := ptypes.Model{ID: "thinker", Provider: "openrouter", Reasoning: true}
	for _, model := range []ptypes.Model{plain, plain, thinking} {
		router.StreamFn(context.Background(), model, ptypes.Context{}, &ptypes.SimpleStreamOptions{Reasoning: "low"})
	}
	if strings.Join(seen, ",") != ",,low" {
		t.Fatalf("reasoning sent = %q", seen)
	}
	if got := strings.Count(logs.String(), "reasoning effort ignored"); got != 1 {
		t.Fatalf("warnings = %d:\n%s", got, logs.String())
	}
}

func TestCatalogImageCapabilityBecomesModelInput(t *testing.T) {
	for spec, vision := range map[string]bool{
		"openrouter:~google/gemini-flash-latest":        true,
		"openrouter:~deepseek/deepseek-v4-flash-latest": false,
		"openrouter:vendor/not-in-catalog":              false,
		"claude-opus-5":                                 true,
	} {
		rt, err := resolveRoute(RoutingConfig{ModelID: spec}, func(string) string { return "" })
		if err != nil {
			t.Fatal(err)
		}
		if rt.Model.SupportsVision() != vision {
			t.Errorf("%s: vision=%v input=%v", spec, rt.Model.SupportsVision(), rt.Model.Input)
		}
	}
}
