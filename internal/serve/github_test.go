package serve

import (
	"strings"
	"testing"

	"go.harness.dev/harness/internal/githubsource"
	"go.harness.dev/harness/tempest/extension"
)

func TestServeGitHubRefusesExtensionProcesses(t *testing.T) {
	_, err := New(Options{
		SessionDir:         t.TempDir(),
		ExtensionProcesses: []extension.ProcessOptions{{ID: "linter", Argv: []string{"/usr/bin/true"}}},
		GitHub:             &githubsource.Options{},
	})
	if err == nil || !strings.Contains(err.Error(), "-extension-config cannot be combined with -github") {
		t.Fatalf("New = %v", err)
	}
}

func TestServeWithoutGitHubHasNoRunner(t *testing.T) {
	server, err := New(Options{SessionDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close(t.Context())
	if server.GitHub() != nil || server.registry != nil {
		t.Fatal("a runner without -github")
	}
}
