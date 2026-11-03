package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeGitHubFlagsMustBeSetTogether(t *testing.T) {
	for _, args := range [][]string{{"-github", "acme/widgets"}, {"-github-config", "/etc/tempest/github.json"}} {
		var stderr bytes.Buffer
		if code := runServe(append(args, "-session-dir", t.TempDir()), &bytes.Buffer{}, &stderr); code != exitUsage {
			t.Fatalf("%v: exit %d, stderr %q", args, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "-github and -github-config must be set together") {
			t.Fatalf("%v: stderr %q", args, stderr.String())
		}
	}
}

func TestServeGitHubRejectsInvalidConfigBeforeServing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "github.json")
	config := `{"version":1,"repository":"acme/widgets","account":{"login":"bot","id":100,"ghPath":"/usr/bin/gh","ghConfigDir":"/var/gh"},` +
		`"clone":"/srv/widgets","poll":{},"observe":{},"admission":{"mode":"observe","actors":[{"login":"parth","id":1111}],"bots":[]},` +
		`"workers":{"max":2,"prepareTimeout":"0s"},"budgets":{"run":{"maxTurns":10,"maxCostUsd":1,"maxWallTime":"1h"},"ticketUsd":10,"repoUsd":100,"repoPeriod":"720h","maxAutoWakesPerDay":12},` +
		`"limits":{},"prompts":{},"merge":"auto"}`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code := runServe([]string{"-github", "acme/widgets", "-github-config", path, "-session-dir", dir}, &bytes.Buffer{}, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "merge:") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	stderr.Reset()
	code = runServe([]string{"-github", "acme/gadgets", "-github-config", path, "-session-dir", dir}, &bytes.Buffer{}, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "repository:") {
		t.Fatalf("mismatched -github: exit %d, stderr %q", code, stderr.String())
	}
}

func TestServeGitHubFlagsOnlyOnServe(t *testing.T) {
	var stderr bytes.Buffer
	if code := runServerCommand("mcp", []string{"-github", "acme/widgets"}, &bytes.Buffer{}, &stderr); code == 0 || !strings.Contains(stderr.String(), "-github") {
		t.Fatalf("mcp accepted -github: exit %d, stderr %q", code, stderr.String())
	}
}
