package serve

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
)

// buildMCPFixture builds the hermetic MCP server; the parent test command
// already holds the machine-wide heavy lock.
func buildMCPFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp-fixture")
	if output, err := exec.Command("go", "build", "-o", path, "../mcpclient/testdata/server").CombinedOutput(); err != nil {
		t.Fatalf("build MCP fixture: %v\n%s", err, output)
	}
	return path
}

func TestACPAnnouncesAndExpandsMCPPromptCommands(t *testing.T) {
	fixture := buildMCPFixture(t)
	turns := make(chan string, 1)
	provider := faux.New(faux.Options{})
	provider.SetResponses(func(c types.Context, opts *types.StreamOptions, state faux.State, model types.Model) (types.AssistantMessage, error) {
		encoded, err := json.Marshal(c.Messages[len(c.Messages)-1])
		turns <- string(encoded)
		if err != nil {
			return types.AssistantMessage{}, err
		}
		return faux.Respond(faux.Text("reviewed"))(c, opts, state, model)
	})
	server := newServer(t, provider, nil, permission.ModeDefault, nil)
	client := connect(t, server)
	var created struct {
		SessionID string `json:"sessionId"`
	}
	call(t, client, "session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []map[string]any{{"name": "fixture", "command": fixture, "args": []string{}, "env": []string{}}}}, &created)

	deadline := time.After(10 * time.Second)
	for announced := false; !announced; {
		select {
		case event := <-client.events:
			var update struct {
				Update struct {
					SessionUpdate     string `json:"sessionUpdate"`
					AvailableCommands []struct {
						Name        string `json:"name"`
						Description string `json:"description"`
						Input       struct {
							Hint string `json:"hint"`
						} `json:"input"`
					} `json:"availableCommands"`
				} `json:"update"`
			}
			if event.method != "session/update" || json.Unmarshal(event.params, &update) != nil || update.Update.SessionUpdate != "available_commands_update" {
				continue
			}
			commands := update.Update.AvailableCommands
			if len(commands) != 2 || commands[1].Name != "fixture:review" || commands[1].Description != "Review a file" || commands[1].Input.Hint != "<path> [focus]" {
				t.Fatalf("available commands = %s", event.params)
			}
			announced = true
		case <-deadline:
			t.Fatal("no available_commands_update after session/new")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var reply json.RawMessage
	if err := client.conn.Call(ctx, "session/prompt", map[string]any{"sessionId": created.SessionID, "prompt": []map[string]string{{"type": "text", "text": "/fixture:review main.go naming"}}}, &reply); err != nil {
		t.Fatal(err)
	}
	select {
	case turn := <-turns:
		if !strings.Contains(turn, "Review main.go focusing on naming") || strings.Contains(turn, "/fixture:review") {
			t.Fatalf("model saw %s", turn)
		}
	default:
		t.Fatal("the model was not called")
	}
}
