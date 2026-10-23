package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/manifest"
)

// copyTree copies testdata into a fresh directory the test may modify.
func copyTree(t *testing.T, source string) string {
	t.Helper()
	target := t.TempDir()
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(target, relative), 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(target, relative), content, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func runManifestDiff(t *testing.T, cwd, args string) (manifest.Result, agent.AgentToolResult, error) {
	t.Helper()
	tool, err := NewTool(ManifestDiffTool, cwd, ToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	output, err := tool.Execute(context.Background(), "call", json.RawMessage(args), nil)
	if err != nil {
		return manifest.Result{}, output, err
	}
	var result manifest.Result
	if err := json.Unmarshal([]byte(resultText(output)), &result); err != nil {
		t.Fatalf("text content is not the result JSON: %v\n%s", err, resultText(output))
	}
	return result, output, nil
}

func TestManifestDiffComparesRenderedFixtures(t *testing.T) {
	cwd := copyTree(t, filepath.Join("testdata", "manifest_diff"))
	result, output, err := runManifestDiff(t, cwd, `{"before":"before","after":"after/all.yaml"}`)
	if err != nil {
		t.Fatal(err)
	}
	if string(output.Details) != resultText(output) {
		t.Fatalf("details must equal the text JSON:\n%s\n%s", output.Details, resultText(output))
	}
	want := manifest.Summary{Before: 3, After: 4, Added: 1, Removed: 0, Changed: 2, Unchanged: 1}
	if result.Summary != want {
		t.Fatalf("summary = %+v, want %+v", result.Summary, want)
	}
	byName := map[string]manifest.Resource{}
	for _, entry := range result.Resources {
		byName[entry.Identity.Kind+"/"+entry.Identity.Name] = entry
	}
	deployment := byName["Deployment/api"]
	if deployment.Change != manifest.ChangeChanged || deployment.BeforeSource.Path != "before/deployment.yaml" || deployment.AfterSource.Path != "after/all.yaml" || deployment.AfterSource.Document != 3 {
		t.Fatalf("deployment entry: %+v", deployment)
	}
	for _, line := range []string{"-  replicas: 2", "+  replicas: 3", "-          image: registry.example.com/api:1.0.0", "+          image: registry.example.com/api:1.1.0"} {
		if !strings.Contains(deployment.Patch, line+"\n") {
			t.Fatalf("deployment patch lacks %q:\n%s", line, deployment.Patch)
		}
	}
	secret := byName["Secret/api-credentials"]
	if secret.Change != manifest.ChangeChanged || !secret.SecretDataChanged {
		t.Fatalf("secret-only change not detected: %+v", secret)
	}
	if strings.Contains(resultText(output), "b2xkLXBhc3N3b3Jk") || strings.Contains(resultText(output), "bmV3LXBhc3N3b3Jk") {
		t.Fatal("redacted output contains Secret data")
	}
	if byName["ConfigMap/api-flags"].Change != manifest.ChangeAdded {
		t.Fatalf("configmap: %+v", byName["ConfigMap/api-flags"])
	}

	unredacted, _, err := runManifestDiff(t, cwd, `{"before":"before/secret.yaml","after":"after","redact_secrets":false,"context_lines":0}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range unredacted.Resources {
		if entry.Identity.Kind == "Secret" && !strings.Contains(entry.Patch, "+  DATABASE_PASSWORD: bmV3LXBhc3N3b3Jk\n") {
			t.Fatalf("explicit redact_secrets=false must show the values:\n%s", entry.Patch)
		}
	}
}

func TestManifestDiffStaysInsideTheWorkingDirectory(t *testing.T) {
	cwd := copyTree(t, filepath.Join("testdata", "manifest_diff"))
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "leak.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: leak}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cwd, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(cwd, "before"), filepath.Join(cwd, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "leak.yaml"), filepath.Join(cwd, "after", "leak.yaml")); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		args            string
		wantOutsideRoot bool
		wantText        string
	}{
		"parent traversal":             {args: `{"before":"../x","after":"after/all.yaml"}`, wantOutsideRoot: true},
		"absolute outside":             {args: `{"before":"` + filepath.Join(outside, "leak.yaml") + `","after":"before"}`, wantOutsideRoot: true},
		"symlink out of cwd":           {args: `{"before":"escape/leak.yaml","after":"before"}`, wantOutsideRoot: true},
		"symlink inside cwd":           {args: `{"before":"alias","after":"before"}`, wantText: "symlinked input"},
		"symlink inside walked folder": {args: `{"before":"before","after":"after"}`, wantText: "symlinked entry"},
		"missing input":                {args: `{"before":"nope","after":"before"}`, wantText: "no such file"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := runManifestDiff(t, cwd, test.args)
			if err == nil {
				t.Fatal("comparison outside the contained inputs succeeded")
			}
			if test.wantOutsideRoot != errors.Is(err, ErrOutsideRoot) {
				t.Fatalf("ErrOutsideRoot = %v, want %v: %v", errors.Is(err, ErrOutsideRoot), test.wantOutsideRoot, err)
			}
			if !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("error %q lacks %q", err, test.wantText)
			}
		})
	}
}

// TestManifestDiffRunsNoProcess poisons PATH with renderer and VCS stand-ins
// that record any invocation; manifest_diff must call none of them.
func TestManifestDiffRunsNoProcess(t *testing.T) {
	cwd := copyTree(t, filepath.Join("testdata", "manifest_diff"))
	bin := t.TempDir()
	ledger := filepath.Join(t.TempDir(), "ledger")
	for _, name := range []string{"helm", "kustomize", "kubectl", "git", "sh", "bash"} {
		script := "#!/bin/sh\necho " + name + " >> " + ledger + "\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	if _, _, err := runManifestDiff(t, cwd, `{"before":"before","after":"after"}`); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(ledger); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest_diff launched a process: %q %v", content, err)
	}
}

func TestManifestDiffArgumentsAreStrict(t *testing.T) {
	cwd := copyTree(t, filepath.Join("testdata", "manifest_diff"))
	for name, args := range map[string]string{
		"unknown argument":        `{"before":"before","after":"after","context":3}`,
		"camelCase alias":         `{"before":"before","after":"after","maxResources":3}`,
		"fractional integer":      `{"before":"before","after":"after","context_lines":1.5}`,
		"context above maximum":   `{"before":"before","after":"after","context_lines":11}`,
		"resources below minimum": `{"before":"before","after":"after","max_resources":0}`,
		"resources above maximum": `{"before":"before","after":"after","max_resources":201}`,
		"string boolean":          `{"before":"before","after":"after","redact_secrets":"no"}`,
		"missing after":           `{"before":"before"}`,
		"URL":                     `{"before":"https://example.com/a.yaml","after":"after"}`,
		"NUL":                     `{"before":"before\u0000x","after":"after"}`,
		"trailing data":           `{"before":"before","after":"after"} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := runManifestDiff(t, cwd, args); err == nil {
				t.Fatalf("accepted %s", args)
			}
		})
	}
	limited, _, err := runManifestDiff(t, cwd, `{"before":"before","after":"after","max_resources":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited.Resources) != 1 || limited.OmittedResources != 2 || !limited.Truncated {
		t.Fatalf("max_resources not applied: %+v", limited)
	}
}

func TestManifestDiffHonoursCancellation(t *testing.T) {
	cwd := copyTree(t, filepath.Join("testdata", "manifest_diff"))
	tool, err := NewTool(ManifestDiffTool, cwd, ToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Execute(ctx, "call", json.RawMessage(`{"before":"before","after":"after"}`), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}
