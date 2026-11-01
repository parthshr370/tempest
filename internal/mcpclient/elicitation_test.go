package mcpclient

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/internal/question"
)

func elicitText(t *testing.T, manager *Manager, mode string) string {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"value": mode})
	result, err := fixtureTool(t, manager, "elicit").Execute(context.Background(), "elicit-"+mode, args, nil)
	if err != nil || result.IsError || len(result.Content) == 0 {
		t.Fatalf("elicit %s: %#v %v", mode, result, err)
	}
	return result.Content[0].Text
}

func TestMCPServerElicitationIsForwarded(t *testing.T) {
	var mu sync.Mutex
	var asked []question.Elicit
	manager, _ := openFixture(t, "normal", func(_ *mcpconfig.Server, options *Options) {
		options.Elicit = func(_ context.Context, request question.Elicit) (question.ElicitResult, error) {
			mu.Lock()
			asked = append(asked, request)
			mu.Unlock()
			if request.Mode == "url" {
				return question.ElicitResult{Action: "accept"}, nil
			}
			return question.ElicitResult{Action: "accept", Content: json.RawMessage(`{"color":"red"}`)}, nil
		}
	})
	if reply := elicitText(t, manager, "form"); reply != `{"action":"accept","content":{"color":"red"}}` {
		t.Fatalf("form reply = %s", reply)
	}
	if reply := elicitText(t, manager, "url"); reply != `{"action":"accept"}` {
		t.Fatalf("url reply = %s", reply)
	}
	// A schema outside the flat form subset never reaches the user.
	if reply := elicitText(t, manager, "nested"); !strings.Contains(reply, "elicitation error") || !strings.Contains(reply, "unsupported type") {
		t.Fatalf("nested reply = %s", reply)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 2 {
		t.Fatalf("asked %d times", len(asked))
	}
	form, link := asked[0], asked[1]
	if form.Server != "fixture" || form.Mode != "form" || form.Message != "Pick a color" || !strings.Contains(string(form.Schema), `"enum":["red","blue"]`) || form.Display() != "MCP server fixture: Pick a color" {
		t.Fatalf("form request = %+v", form)
	}
	if link.Mode != "url" || link.URL != "https://fixture.example/connect" || link.ElicitationID != "fixture-1" {
		t.Fatalf("url request = %+v", link)
	}
}

func TestMCPServerElicitationDeclinedWithoutUser(t *testing.T) {
	headless, _ := openFixture(t, "normal")
	for _, mode := range []string{"form", "url"} {
		if reply := elicitText(t, headless, mode); reply != `{"action":"decline"}` {
			t.Fatalf("headless %s reply = %s", mode, reply)
		}
	}
	unavailable, _ := openFixture(t, "normal", func(_ *mcpconfig.Server, options *Options) {
		options.Elicit = func(context.Context, question.Elicit) (question.ElicitResult, error) {
			return question.ElicitResult{}, question.Unavailable
		}
	})
	if reply := elicitText(t, unavailable, "form"); reply != `{"action":"decline"}` {
		t.Fatalf("unavailable reply = %s", reply)
	}
}
