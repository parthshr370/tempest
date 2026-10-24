package tempest

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestCodingToolChildCapabilities(t *testing.T) {
	parent := []string{"read", "ast_grep", "ast_edit", "gh", "gh_checkout"}
	general := childToolNames("general", parent)
	for _, name := range parent[:len(parent)-1] {
		if !slices.Contains(general, name) {
			t.Errorf("general child missing %s", name)
		}
	}
	if slices.Contains(general, "gh_checkout") {
		t.Fatal("general child can export a nested Git worktree")
	}
	scout := childToolNames("scout", parent)
	for _, name := range []string{"ast_grep", "gh"} {
		if !slices.Contains(scout, name) {
			t.Errorf("scout missing %s", name)
		}
	}
	for _, name := range []string{"ast_edit", "gh_checkout"} {
		if slices.Contains(scout, name) {
			t.Errorf("scout got %s", name)
		}
	}
	for _, name := range childToolNames("general", []string{"read"}) {
		if name != "read" {
			t.Errorf("child widened parent allowlist with %s", name)
		}
	}
}
func TestCodingToolEventKinds(t *testing.T) {
	for name, want := range map[string]ToolKind{"ast_grep": "search", "ast_edit": "read", "gh": "read", "gh_checkout": "execute"} {
		_, kind := toolDescription(name, json.RawMessage(`{}`))
		if kind != want {
			t.Errorf("%s kind %s, want %s", name, kind, want)
		}
	}
	_, kind := toolDescription("ast_edit", json.RawMessage(`{"apply":true}`))
	if kind != "edit" {
		t.Fatalf("applied rewrite kind = %s", kind)
	}
}
