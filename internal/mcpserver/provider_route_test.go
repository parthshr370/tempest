package mcpserver

import (
	"go.harness.dev/harness/tempest"
	"testing"
)

func TestRunModelAllowsVersionButNotEndpointOverride(t *testing.T) {
	model := "vertex:claude-haiku-4-5@20251001"
	got, err := (RunInput{Prompt: "hi", Model: model}).settings(tempest.NewSession{}, nil)
	if err != nil || got.Model != model {
		t.Fatalf("version route: model=%s err=%v", got.Model, err)
	}
	for _, suffix := range []string{"@https://evil.test", "@http://evil.test"} {
		if _, err := (RunInput{Prompt: "hi", Model: model + suffix}).settings(tempest.NewSession{}, nil); err == nil {
			t.Fatal("MCP allowed host URL override")
		}
	}
}
