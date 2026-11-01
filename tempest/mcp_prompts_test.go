package tempest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	types "go.harness.dev/harness/internal/engine/types"
)

// openFixtureMCP opens a faux session whose only MCP server is the
// hermetic fixture.
func openFixtureMCP(t *testing.T, script string, server MCPServerConfig, configure ...func(*Options)) *Session {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	server.Name, server.Transport, server.Command = "fixture", "stdio", mcpFixtureServer
	options := Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &NewSession{MCPServers: []MCPServerConfig{server}}, FauxScript: writeScript(t, script)}
	for _, change := range configure {
		change(&options)
	}
	s, err := Open(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestSessionMCPPromptCommandsAndResources(t *testing.T) {
	s := openFixtureMCP(t, `[{"text":"reviewed"},{"toolCalls":[{"id":"r1","name":"read","arguments":{"path":"mcp://fixture/fixture://bump"}}]},{"text":"done"}]`, MCPServerConfig{})
	commands := s.Commands()
	if len(commands) != 2 || commands[1] != (Command{Name: "fixture:review", Description: "Review a file", Hint: "<path> [focus]"}) {
		t.Fatalf("commands = %+v", commands)
	}

	if _, err := s.Prompt(t.Context(), Prompt{Text: "/fixture:review"}); err == nil || !strings.Contains(err.Error(), "/fixture:review needs path") {
		t.Fatalf("missing argument error = %v", err)
	}
	if _, err := s.Prompt(t.Context(), Prompt{Text: "/fixture:review main.go naming"}); err != nil {
		t.Fatal(err)
	}
	var userTurns []string
	for _, message := range s.Messages() {
		user, isUser := normalizeMessage(message).(types.UserMessage)
		if !isUser {
			continue
		}
		encoded, err := json.Marshal(user.Content)
		if err != nil {
			t.Fatal(err)
		}
		userTurns = append(userTurns, string(encoded))
	}
	if len(userTurns) != 1 || !strings.Contains(userTurns[0], "Review main.go focusing on naming") || strings.Contains(userTurns[0], "/fixture:review") {
		t.Fatalf("user turns = %v", userTurns)
	}

	// The model reads an MCP resource; the server then changes its prompt
	// list, and watchers get the new commands.
	_, updates, stop := s.Watch(t.Context(), WatchOptions{Buffer: 256})
	defer stop()
	if _, err := s.Prompt(t.Context(), Prompt{Text: "refresh the fixture"}); err != nil {
		t.Fatal(err)
	}
	if read := toolResults(s.Messages())["r1"]; read.IsError || !strings.Contains(read.Content[0].Text, "bumped") {
		t.Fatalf("read mcp:// result = %#v", read)
	}
	deadline := time.After(10 * time.Second)
	for announced := false; !announced; {
		select {
		case u := <-updates:
			announced = u.Kind == "available_commands_update" && strings.Contains(string(u.Update), `"name":"fixture:added"`)
		case <-deadline:
			t.Fatal("watchers never saw the changed commands")
		}
	}
}
