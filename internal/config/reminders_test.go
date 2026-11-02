package config

import (
	"errors"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/agent"
)

// The operator's reminders resolve as written; a higher layer's array
// replaces a lower one's; an invalid reminder fails resolution naming it.
func TestRemindersSettingResolvesAndValidates(t *testing.T) {
	w := newLayerWorkspace(t)
	w.write(w.user, `{"reminders":[{"name":"todo","text":"Update your todo list.","when":{"tool_calls":8,"without_tool":"todo_write"},"max_fires":3},{"name":"final","text":"Answer now.","when":{"final_turn":true}}]}`)
	got := w.mustResolve().Reminders()
	if len(got) != 2 || got[0].Name != "todo" || got[0].When.ToolCalls != 8 || got[0].When.WithoutTool != "todo_write" || got[0].MaxFires != 3 || !got[1].When.FinalTurn || got[1].Scope != "" {
		t.Fatalf("reminders = %+v", got)
	}

	w.write(w.localPath(), `{"reminders":[{"name":"rule","text":"Keep tests green.","scope":"conversation"}]}`)
	w.flags.TrustProjectHooks = BoolInput{Value: true, Set: true}
	got = w.mustResolve().Reminders()
	if len(got) != 1 || got[0].Name != "rule" || got[0].Scope != agent.ReminderConversation {
		t.Fatalf("trusted project-local reminders = %+v, want them to replace the user's", got)
	}

	w = newLayerWorkspace(t)
	w.write(w.user, `{"reminders":[{"name":"bad","text":"x","max_fires":-2}]}`)
	_, err := w.resolve()
	var configErr *Error
	if !errors.As(err, &configErr) || configErr.Field != "reminders" || !strings.Contains(err.Error(), `reminder "bad": max_fires`) {
		t.Fatalf("resolve = %v, want a reminders error naming bad", err)
	}
}
