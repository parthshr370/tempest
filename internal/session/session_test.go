package session

import (
	"context"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/agent"
	ptypes "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/toolio"
	"go.harness.dev/harness/internal/tools"
)

func TestSelectActive(t *testing.T) {
	dir := t.TempDir()
	all := tools.AllTools(dir, tools.ToolsOptions{})
	active := SelectActive(all, nil, nil, "")
	if len(active) != 10 {
		t.Fatalf("all active = %d", len(active))
	}
	readonly := SelectActive(all, []string{"read", "grep", "find", "ls"}, nil, "")
	if len(readonly) != 4 || names(readonly) != "read,grep,find,ls" {
		t.Fatalf("readonly = %s", names(readonly))
	}
	excluded := SelectActive(all, nil, []string{"bash", "write"}, "")
	if len(excluded) != 8 {
		t.Fatalf("excluded = %d", len(excluded))
	}
}

func TestBuildToolSnippets(t *testing.T) {
	dir := t.TempDir()
	active := tools.CodingTools(dir, tools.ToolsOptions{})
	active = append(active, agent.AgentTool{Tool: ptypes.Tool{Name: "custom_agent"}, Label: "custom_agent"})
	snippets := BuildToolSnippets(active)
	for _, name := range []string{"read", "bash", "edit", "write", "custom_agent"} {
		if snippets[name] == "" {
			t.Fatalf("missing snippet for %s", name)
		}
	}
}

func TestRebuildSystemPrompt(t *testing.T) {
	dir := t.TempDir()
	active := tools.CodingTools(dir, tools.ToolsOptions{})
	got := RebuildSystemPrompt(RebuildSystemPromptOptions{
		Cwd:         "/tmp",
		ActiveTools: active,
	})
	for _, want := range []string{"helpful coding assistant operating in Harness", "read:", "bash:", "edit:", "write:", "Current working directory: /tmp"} {
		if !contains(got, want) {
			t.Fatalf("prompt missing %q:\n%s", want, got)
		}
	}
}

func TestSessionConcurrency(t *testing.T) {
	dir := t.TempDir()
	queue := toolio.NewFileMutationQueue()
	opts := tools.ToolsOptions{MutationQueue: queue}
	all := BuildToolRegistry(dir, opts)
	active := SelectActive(all, nil, nil, "")
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			read, ok := all[tools.ReadTool]
			if !ok {
				errs <- nil
				return
			}
			_, err := read.Execute(context.Background(), "id", nil, nil)
			if err != nil {
				errs <- err
			}
		}(i)
		go func(n int) {
			defer wg.Done()
			_, err := active[0].Execute(context.Background(), "id", nil, nil)
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Logf("concurrent error: %v", err)
	}
	_ = SelectActive(all, []string{"read", "ls"}, nil, "")
	RebuildSystemPrompt(RebuildSystemPromptOptions{Cwd: dir, ActiveTools: active})
}

func names(tools []agent.AgentTool) string {
	result := ""
	for i, tool := range tools {
		if i > 0 {
			result += ","
		}
		result += tool.Name
	}
	return result
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && checkContains(s, substr))
}

func checkContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestAppendCapabilitiesNamesChildTools(t *testing.T) {
	active := []agent.AgentTool{
		{Tool: ptypes.Tool{Name: "read"}, Label: "read"},
		{Tool: ptypes.Tool{Name: "custom_agent"}, Label: "custom_agent"},
	}
	got := AppendCapabilities("Child prompt.", active)
	for _, want := range []string{
		"Child prompt.",
		"<available_tools>",
		"- read: Read files, directories, archive members, office documents, JSON queries and internal URLs",
		"- custom_agent: custom_agent",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
}
