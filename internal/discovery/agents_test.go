package discovery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Startup discovery loads agent files from the working directory and the agent
// dir and reports a skipped file as an agent diagnostic naming it.
func TestDiscoverLoadsAgentFilesWithDiagnostics(t *testing.T) {
	cwd, agentDir := t.TempDir(), t.TempDir()
	files := map[string]string{
		filepath.Join(cwd, ".harness", "agents", "reviewer.md"): "---\nname: reviewer\ndescription: Reviews\n---\nReview.\n",
		filepath.Join(agentDir, "agents", "broken.md"):          "---\nname: broken\n---\nBody.\n",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inputs, err := Discover(context.Background(), Options{Cwd: cwd, AgentDir: agentDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs.Agents) != 1 || inputs.Agents[0].Preset.ID != "reviewer" {
		t.Fatalf("agents %+v, want reviewer", inputs.Agents)
	}
	var agentDiagnostics []Diagnostic
	for _, diagnostic := range inputs.Diagnostics {
		if diagnostic.Code == DiagnosticAgent {
			agentDiagnostics = append(agentDiagnostics, diagnostic)
		}
	}
	if len(agentDiagnostics) != 1 || filepath.Base(agentDiagnostics[0].Path) != "broken.md" || !strings.Contains(agentDiagnostics[0].Message, "description") {
		t.Fatalf("agent diagnostics %+v", agentDiagnostics)
	}
}
