package mcpclient

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/mcpconfig"
)

func promptCommands(prompts []Prompt) []string {
	commands := make([]string, 0, len(prompts))
	for _, prompt := range prompts {
		commands = append(commands, prompt.Command())
	}
	return commands
}

func TestMCPPromptsListedAndExpanded(t *testing.T) {
	var mu sync.Mutex
	var announced [][]string
	manager, _ := openFixture(t, "normal", func(_ *mcpconfig.Server, options *Options) {
		options.OnPromptsChanged = func(prompts []Prompt) {
			mu.Lock()
			announced = append(announced, promptCommands(prompts))
			mu.Unlock()
		}
	})
	prompts := manager.Prompts()
	if got := promptCommands(prompts); !slices.Equal(got, []string{"fixture:dialogue", "fixture:review"}) {
		t.Fatalf("prompts = %v", got)
	}
	if review := prompts[1]; review.Hint() != "<path> [focus]" || review.Summary() != "Review a file" {
		t.Fatalf("review prompt = %+v", review)
	}
	ctx := context.Background()
	text := func(blocks []types.ContentBlock) string {
		var parts []string
		for _, block := range blocks {
			if block.Type == types.BlockText {
				parts = append(parts, block.Text)
			}
		}
		return strings.Join(parts, "|")
	}
	blocks, ok, err := manager.ExpandPrompt(ctx, "/fixture:review main.go security and speed")
	if err != nil || !ok || text(blocks) != "Review main.go focusing on security and speed" || len(blocks) != 2 || blocks[1].Type != types.BlockImage {
		t.Fatalf("positional expansion = %+v %v %v", blocks, ok, err)
	}
	if blocks, ok, err = manager.ExpandPrompt(ctx, "/fixture:review focus=tests path=a.go"); err != nil || !ok || text(blocks) != "Review a.go focusing on tests" {
		t.Fatalf("named expansion = %+v %v %v", blocks, ok, err)
	}
	if blocks, ok, err = manager.ExpandPrompt(ctx, "/fixture:dialogue"); err != nil || !ok || text(blocks) != "[user]|What is 2+2?|[assistant]|4|[user]|Now explain." {
		t.Fatalf("mixed roles = %q %v %v", text(blocks), ok, err)
	}
	if _, ok, err = manager.ExpandPrompt(ctx, "/fixture:review"); !ok || err == nil || !strings.Contains(err.Error(), "/fixture:review needs path; usage: /fixture:review <path> [focus]") {
		t.Fatalf("missing argument = %v %v", ok, err)
	}
	if _, ok, err = manager.ExpandPrompt(ctx, "/fixture:dialogue extra"); !ok || err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Fatalf("extra argument = %v %v", ok, err)
	}
	for _, plain := range []string{"review main.go", "/fixture:unknown x", "/other:review x", "/fixture"} {
		if _, ok, err = manager.ExpandPrompt(ctx, plain); ok || err != nil {
			t.Fatalf("%q expanded: %v %v", plain, ok, err)
		}
	}

	// prompts/list_changed refreshes the catalog and announces the new list.
	if _, _, err := manager.ReadResource(ctx, "mcp://fixture/fixture://bump"); err != nil {
		t.Fatal(err)
	}
	announcedAdded := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(announced) >= 2 && slices.Contains(announced[len(announced)-1], "fixture:added")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(promptCommands(manager.Prompts()), "fixture:added") || !announcedAdded() {
		if time.Now().After(deadline) {
			t.Fatalf("prompts/list_changed did not refresh and announce: %v", promptCommands(manager.Prompts()))
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(announced[0], []string{"fixture:dialogue", "fixture:review"}) {
		t.Fatalf("announced = %v", announced)
	}
}
