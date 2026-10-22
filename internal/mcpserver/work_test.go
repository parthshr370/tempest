package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"go.harness.dev/harness/tempest"
)

func TestBackgroundProgressIsLifecycleOnly(t *testing.T) {
	for _, kind := range []string{"tempest/job", "tempest/job_output", "tempest/queue", "tempest/compaction"} {
		update := tempest.Update{Kind: kind, Update: json.RawMessage(`{"action":"finished","job":{"id":"job_one","kind":"task","state":"completed","resultText":"SECRET RESULT","cwd":"/private","value":{"secret":true}},"item":{"text":"SECRET QUEUE"},"summary":"SECRET SUMMARY"}`)}
		message := progressMessage(update)
		if message == "" || strings.Contains(message, "SECRET") || strings.Contains(message, "/private") {
			t.Fatalf("%s progress=%q", kind, message)
		}
	}
}
