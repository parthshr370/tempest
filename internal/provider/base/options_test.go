package base

import (
	"regexp"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/engine/types"
)

func TestPortableToolCallIDs(t *testing.T) {
	pattern := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	seen := map[string]string{}
	for _, id := range []string{"call_valid-1", "", "foreign/id", "foreign:id", "foreign.id", strings.Repeat("a", 65), "工具"} {
		got := NormalizeToolCallID(id, types.Model{}, types.AssistantMessage{})
		if !pattern.MatchString(got) || got != NormalizeToolCallID(id, types.Model{}, types.AssistantMessage{}) {
			t.Fatalf("invalid or unstable ID %q: %q", id, got)
		}
		if other, ok := seen[got]; ok && id != other {
			t.Fatalf("ID collision: %q and %q", id, other)
		}
		seen[got] = id
		if pattern.MatchString(id) && got != id {
			t.Fatalf("portable ID changed: %q", id)
		}
	}
}

func TestPromptCacheKey(t *testing.T) {
	for _, id := range []string{"", "session", strings.Repeat("s", 64), strings.Repeat("s", 65)} {
		got := PromptCacheKey(id)
		if len(got) > 64 || got != PromptCacheKey(id) || len(id) <= 64 && got != id {
			t.Fatalf("invalid cache key for %q: %q", id, got)
		}
	}
}

func TestClampMaxTokens(t *testing.T) {
	for _, test := range []struct {
		Name            string
		Model           types.Model
		Requested, Want int
	}{
		{"model default", types.Model{MaxTokens: 100}, 0, 100},
		{"output bound", types.Model{MaxTokens: 100}, 1000, 100},
		{"smaller request", types.Model{MaxTokens: 100}, 10, 10},
		{"unknown limits", types.Model{}, 0, 1},
		{"context bound", types.Model{MaxTokens: 1000, ContextWindow: 4106}, 1000, 10},
		{"full context", types.Model{MaxTokens: 1000, ContextWindow: 100}, 1000, 1},
	} {
		t.Run(test.Name, func(t *testing.T) {
			if got := ClampMaxTokens(test.Model, types.Context{}, test.Requested); got != test.Want {
				t.Fatalf("max tokens: %d, want %d", got, test.Want)
			}
		})
	}
}
