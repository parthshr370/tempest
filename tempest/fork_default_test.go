package tempest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.harness.dev/harness/tempest/durable"
)

func TestForkDefaultStackResumeKeepsPrefix(t *testing.T) {
	script := filepath.Join(t.TempDir(), "faux.json")
	if err := os.WriteFile(script, []byte(`[{"text":"answer"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	host, err := OpenHost(t.Context(), HostOptions{SessionDir: t.TempDir(), FauxScript: script})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	parent, err := host.Create(t.Context(), AgentOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parent.Prompt(t.Context(), Prompt{Text: "inherited"}); err != nil {
		t.Fatal(err)
	}
	page, err := parent.Read(t.Context(), durable.Query{Kind: durable.EntryKind, Namespace: "_tempest/journal", Reverse: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	cut := ""
	for _, record := range page.Records {
		var entry struct{ Type, ID string }
		if err := json.Unmarshal(record.Value, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Type == "message" {
			cut = entry.ID
			break
		}
	}
	child, err := host.Fork(t.Context(), parent.ID(), cut, ForkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := child.ID()
	if err := host.CloseSession(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	reopened, err := host.Open(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.core.store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.core.agent.State().Messages) != 2 {
		t.Fatalf("loaded=%d agent=%d", len(loaded.Messages), len(reopened.core.agent.State().Messages))
	}
}
