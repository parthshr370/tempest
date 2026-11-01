package discovery

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/config"
)

// contextRepo makes root/.git, root/pkg (the cwd) and an empty agent dir.
func contextRepo(t *testing.T) (root, cwd, agentDir string) {
	t.Helper()
	root = t.TempDir()
	cwd = filepath.Join(root, "pkg")
	agentDir = filepath.Join(root, "agent")
	for _, dir := range []string{filepath.Join(root, ".git"), cwd, agentDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, cwd, agentDir
}

func discoverContents(t *testing.T, options Options) []string {
	t.Helper()
	inputs, err := Discover(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	contents := make([]string, 0, len(inputs.ContextFiles))
	for _, file := range inputs.ContextFiles {
		contents = append(contents, file.Content)
	}
	return contents
}

func TestDiscoverOverrideBeatsAgentsAndFallback(t *testing.T) {
	root, cwd, agentDir := contextRepo(t)
	write(t, filepath.Join(root, "AGENTS.override.md"), "root override")
	write(t, filepath.Join(root, "AGENTS.md"), "root agents")
	write(t, filepath.Join(root, "CLAUDE.md"), "root claude")
	write(t, filepath.Join(cwd, "AGENTS.md"), "pkg agents")
	write(t, filepath.Join(cwd, "CLAUDE.md"), "pkg claude")
	write(t, filepath.Join(agentDir, "AGENTS.override.md"), "user override")
	write(t, filepath.Join(agentDir, "AGENTS.md"), "user agents")
	got := discoverContents(t, Options{Cwd: cwd, AgentDir: agentDir})
	want := []string{"user override", "root override", "pkg agents"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("contexts = %q, want %q", got, want)
	}
}

func TestDiscoverFallbackOnlyWhenNoAgentsFile(t *testing.T) {
	root, cwd, agentDir := contextRepo(t)
	write(t, filepath.Join(root, "CLAUDE.md"), "root claude")
	write(t, filepath.Join(cwd, "AGENTS.md"), "pkg agents")
	write(t, filepath.Join(cwd, "CLAUDE.md"), "pkg claude")
	got := discoverContents(t, Options{Cwd: cwd, AgentDir: agentDir})
	if strings.Join(got, "|") != "root claude|pkg agents" {
		t.Fatalf("default fallback contexts = %q", got)
	}
	none := discoverContents(t, Options{Cwd: cwd, AgentDir: agentDir, Files: config.ContextFileSettings{Fallback: []string{}}})
	if strings.Join(none, "|") != "pkg agents" {
		t.Fatalf("empty fallback contexts = %q", none)
	}
	write(t, filepath.Join(root, "GEMINI.md"), "root gemini")
	custom := discoverContents(t, Options{Cwd: cwd, AgentDir: agentDir, Files: config.ContextFileSettings{Fallback: []string{"GEMINI.md", "CLAUDE.md"}}})
	if strings.Join(custom, "|") != "root gemini|pkg agents" {
		t.Fatalf("custom fallback contexts = %q", custom)
	}
}

func TestDiscoverExpandsImportsInClaudeFile(t *testing.T) {
	root, cwd, agentDir := contextRepo(t)
	write(t, filepath.Join(root, "style.md"), "use tabs")
	write(t, filepath.Join(root, "CLAUDE.md"), "rules: @./style.md")
	got := discoverContents(t, Options{Cwd: cwd, AgentDir: agentDir})
	if len(got) != 1 || got[0] != "rules: use tabs" {
		t.Fatalf("contexts = %q", got)
	}
}

func TestDiscoverCapsTotalBytesWithNotice(t *testing.T) {
	const (
		firstBody  = "0123456789"
		secondBody = "abcdefghij"
		// The cap keeps all of the first file and half of the second.
		capBytes  = len(firstBody) + len(secondBody)/2
		keptBytes = capBytes
	)
	root, cwd, agentDir := contextRepo(t)
	write(t, filepath.Join(agentDir, "AGENTS.md"), firstBody)
	write(t, filepath.Join(root, "AGENTS.md"), secondBody)
	write(t, filepath.Join(cwd, "AGENTS.md"), "never loaded")
	inputs, err := Discover(context.Background(), Options{Cwd: cwd, AgentDir: agentDir, Files: config.ContextFileSettings{MaxBytes: capBytes}})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs.ContextFiles) != 2 || inputs.ContextFiles[0].Content != firstBody {
		t.Fatalf("contexts = %#v", inputs.ContextFiles)
	}
	cut := inputs.ContextFiles[1].Content
	wantNotice := "context_files.max_bytes (" + strconv.Itoa(capBytes) + " bytes)"
	if !strings.HasPrefix(cut, secondBody[:len(secondBody)/2]+"\n\n[harness: ") || !strings.Contains(cut, wantNotice) {
		t.Fatalf("truncated file = %q", cut)
	}
	omittedPath := filepath.ToSlash(filepath.Join(canonical(t, cwd), "AGENTS.md"))
	if !strings.Contains(cut, "omitted: "+omittedPath) {
		t.Fatalf("notice does not name omitted %s: %q", omittedPath, cut)
	}
	if inputs.ContextBytes != keptBytes {
		t.Fatalf("ContextBytes = %d, want %d", inputs.ContextBytes, keptBytes)
	}
	if len(inputs.Diagnostics) == 0 || inputs.Diagnostics[len(inputs.Diagnostics)-1].Code != DiagnosticTruncated {
		t.Fatalf("diagnostics = %#v", inputs.Diagnostics)
	}
}

func TestTruncateUTF8KeepsWholeRunes(t *testing.T) {
	const word = "héllo" // é is two bytes at offsets 1 and 2.
	const splitsRune = 2
	if got := TruncateUTF8(word, splitsRune); got != "h" {
		t.Fatalf("TruncateUTF8 = %q", got)
	}
}

func canonical(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
