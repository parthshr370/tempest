package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/truncate"
)

func codingFakeBinary(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nset -eu\n"+body), 0755); err != nil {
		t.Fatal(err)
	}
}
func astPreviewToken(t *testing.T, result json.RawMessage) string {
	t.Helper()
	var detail struct {
		Preview string `json:"preview"`
	}
	if err := json.Unmarshal(result, &detail); err != nil || len(detail.Preview) != 64 {
		t.Fatalf("preview details: %s, %v", result, err)
	}
	return detail.Preview
}

func TestASTToolsFakeProtocol(t *testing.T) {
	bin := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("AST_WORKSPACE", cwd)
	codingFakeBinary(t, bin, "ast-grep", `[ "$PWD" != "$AST_WORKSPACE" ] || exit 81
case "$*" in
 'run --stdin --lang=javascript --pattern=old($A) --json=compact --color never --threads 1') ;;
 'run --stdin --lang=javascript --pattern=old($A) --json=compact --color never --threads 1 --rewrite=new($A)') ;;
 *) printf 'bad argv: %s' "$*" >&2; exit 82;;
esac
body=$(/bin/cat)
case "$body" in old*) printf '%s\n' '[{"text":"old(1)","range":{"byteOffset":{"start":0,"end":6},"start":{"line":0,"column":0}},"replacement":"new(1)","replacementOffsets":{"start":0,"end":6}}]';; *) printf '[]';; esac
`)
	writeFile(t, filepath.Join(cwd, "a.js"), "old(1);\n")
	changed := []string{}
	opts := ToolsOptions{SessionDataDir: t.TempDir(), OnFileChanged: func(path string) { changed = append(changed, path) }}
	all := AllTools(cwd, opts)
	result, err := executeTool(t, all[ASTGrepTool], map[string]any{"pat": "old($A)", "path": "*.js"})
	if err != nil || !strings.Contains(resultText(result), "a.js:1:1: old(1)") {
		t.Fatalf("search %s: %v", resultText(result), err)
	}
	args := map[string]any{"ops": []map[string]string{{"pat": "old($A)", "out": "new($A)"}}, "paths": []string{"*.js"}}
	preview, err := executeTool(t, all[ASTEditTool], args)
	if err != nil {
		t.Fatal(err)
	}
	token := astPreviewToken(t, preview.Details)
	before, _ := os.ReadFile(filepath.Join(cwd, "a.js"))
	if string(before) != "old(1);\n" || len(changed) != 0 || !strings.Contains(resultText(preview), "-old(1)") {
		t.Fatalf("preview changed files or omitted diff: %s", resultText(preview))
	}
	args["apply"], args["preview"] = true, token
	applied, err := executeTool(t, all[ASTEditTool], args)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(cwd, "a.js"))
	if string(after) != "new(1);\n" || len(changed) != 1 || changed[0] != "a.js" || !strings.Contains(resultText(applied), "Applied") {
		t.Fatalf("apply = %s, %q, %v", resultText(applied), after, changed)
	}
	if _, err := executeTool(t, all[ASTEditTool], args); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale preview accepted: %v", err)
	}
	for _, raw := range []string{`{"ops":[{"pat":"x"}],"paths":["a.js"]}`, `{"ops":[{"pat":"x","out":"y"}],"paths":["a.js"],"apply":true}`} {
		if _, err := all[ASTEditTool].Execute(t.Context(), "invalid", json.RawMessage(raw), nil); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := executeTool(t, all[ASTGrepTool], map[string]any{"pat": "old($A)", "path": "../outside.js"}); err == nil {
		t.Fatal("accepted outside workspace")
	}
	outside := filepath.Join(t.TempDir(), "secret.js")
	writeFile(t, outside, "old(1);\n")
	if err := os.Symlink(outside, filepath.Join(cwd, "link.js")); err != nil {
		t.Fatal(err)
	}
	result, err = executeTool(t, all[ASTGrepTool], map[string]any{"pat": "old($A)", "path": "link.js"})
	if err == nil || !strings.Contains(err.Error(), "outside") || strings.Contains(resultText(result), "old(1)") {
		t.Fatalf("followed symlink outside: %s %v", resultText(result), err)
	}
	codingFakeBinary(t, bin, "ast-grep", "printf 'not json'\n")
	if _, err := executeTool(t, all[ASTGrepTool], map[string]any{"pat": "old($A)", "path": "a.js"}); err == nil || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("invalid JSON accepted: %v", err)
	}
}

func TestASTUnavailableRegistration(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	if _, err := ASTBinary(); err == nil {
		t.Fatal("found missing ast-grep")
	}
	if _, ok := AllTools(t.TempDir(), ToolsOptions{})[ASTEditTool]; ok {
		t.Fatal("registered absent ast-grep")
	}
	codingFakeBinary(t, bin, "sg", "printf 'unrelated sg 1.0\\n'\n")
	if _, err := ASTBinary(); err == nil {
		t.Fatal("accepted unrelated sg")
	}
	codingFakeBinary(t, bin, "sg", "printf 'ast-grep 0.42.0\\n'\n")
	if _, err := ASTBinary(); err != nil {
		t.Fatal(err)
	}
}

func TestASTRealBinaryPreviewApply(t *testing.T) {
	if _, err := ASTBinary(); err != nil {
		t.Skip(err)
	}
	cwd := t.TempDir()
	for _, name := range []string{"a.js", "b.js"} {
		writeFile(t, filepath.Join(cwd, name), "// é\noldApi(1, 2);\n")
	}
	if err := os.Chmod(filepath.Join(cwd, "a.js"), 0750); err != nil {
		t.Fatal(err)
	}
	all := AllTools(cwd, ToolsOptions{SessionDataDir: t.TempDir()})
	search, err := executeTool(t, all[ASTGrepTool], map[string]any{"pat": "oldApi($$$ARGS)", "path": "."})
	if err != nil || !strings.Contains(resultText(search), "a.js:2:1") {
		t.Fatalf("real search: %s %v", resultText(search), err)
	}
	args := map[string]any{"ops": []map[string]string{{"pat": "oldApi($$$ARGS)", "out": "newApi($$$ARGS)"}}, "paths": []string{"*.js"}}
	preview, err := executeTool(t, all[ASTEditTool], args)
	if err != nil {
		t.Fatal(err)
	}
	args["apply"], args["preview"] = true, astPreviewToken(t, preview.Details)
	if _, err := executeTool(t, all[ASTEditTool], args); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.js", "b.js"} {
		data, _ := os.ReadFile(filepath.Join(cwd, name))
		if string(data) != "// é\nnewApi(1, 2);\n" {
			t.Fatalf("rewrite %s = %q", name, data)
		}
	}
	info, _ := os.Stat(filepath.Join(cwd, "a.js"))
	if info.Mode().Perm() != 0750 {
		t.Fatal("lost mode")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := all[ASTEditTool].Execute(ctx, "cancel", json.RawMessage(`{"ops":[{"pat":"x","out":"y"}],"paths":["a.js"]}`), nil); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestASTHookSettingsComputedApproval(t *testing.T) {
	bin, cwd := t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin)
	codingFakeBinary(t, bin, "ast-grep", `printf '%s' '[{"text":"1","range":{"byteOffset":{"start":5,"end":6}},"replacement":"2"}]'`)
	name := filepath.Join(cwd, ".harness", "settings.json")
	writeFile(t, name, "{\"x\":1}\n")
	args := map[string]any{"ops": []map[string]string{{"pat": "1", "out": "2"}}, "paths": []string{".harness/settings.json"}}
	tool, _ := NewTool(ASTEditTool, cwd, ToolsOptions{})
	preview, err := executeTool(t, tool, args)
	if err != nil {
		t.Fatal(err)
	}
	args["apply"], args["preview"] = true, astPreviewToken(t, preview.Details)
	if _, err := executeTool(t, tool, args); err == nil || !strings.Contains(err.Error(), "hook settings") {
		t.Fatalf("settings rewrite bypassed approval: %v", err)
	}
	called := false
	tool, _ = NewTool(ASTEditTool, cwd, ToolsOptions{ApproveHookSettings: func(_ context.Context, tool, callID string, paths []string) error {
		called = true
		if tool != "ast_edit" || len(paths) != 1 || paths[0] != ".harness/settings.json" {
			t.Errorf("wrong computed approval identity: %s %s %v", tool, callID, paths)
		}
		return os.ErrPermission
	}})
	if _, err := executeTool(t, tool, args); err == nil || !called {
		t.Fatalf("ignored approval denial: called=%t err=%v", called, err)
	}
	data, _ := os.ReadFile(name)
	if string(data) != "{\"x\":1}\n" {
		t.Fatalf("unapproved rewrite changed settings: %s", data)
	}
}

func TestASTSearchRepositoryScopeAndPaging(t *testing.T) {
	bin, cwd := t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin)
	codingFakeBinary(t, bin, "ast-grep", `printf '%s' '[{"text":"hit","range":{"start":{"line":0,"column":0}}}]'`)
	for i := range 300 {
		writeFile(t, filepath.Join(cwd, fmt.Sprintf("f%03d.js", i)), "hit();\n")
	}
	writeFile(t, filepath.Join(cwd, ".gitignore"), "node_modules/\n")
	// More entries than the old walker cap, all under one ignored directory.
	for i := range 8300 {
		writeFile(t, filepath.Join(cwd, "node_modules", fmt.Sprintf("f%d.js", i)), "ignored();")
	}
	tool, err := NewTool(ASTGrepTool, cwd, ToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executeTool(t, tool, map[string]any{"pat": "hit()", "skip": 250})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(result)
	if !strings.Contains(text, "f250.js:1:1") || !strings.Contains(text, "f299.js:1:1") || strings.Contains(text, "f249.js") || strings.Contains(text, "node_modules") || strings.Count(text, ":1:1:") != 50 {
		t.Fatalf("wrong search page: %s", text)
	}
	var details codingToolDetails
	if err := json.Unmarshal(result.Details, &details); err != nil || details.Matches != 300 {
		t.Fatalf("count: %s %v", result.Details, err)
	}
	root, err := os.OpenRoot(cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if targets, err := astTargets(t.Context(), root, cwd, []string{"node_modules"}, "", true); err != nil || len(targets) != 0 {
		t.Fatalf("ignored directory was walked: %d %v", len(targets), err)
	}
}

func TestASTDashPatternAndRewrite(t *testing.T) {
	if _, err := ASTBinary(); err != nil {
		t.Skip(err)
	}
	cwd := t.TempDir()
	writeFile(t, filepath.Join(cwd, "a.js"), "-value;\n")
	tool, err := NewTool(ASTEditTool, cwd, ToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executeTool(t, tool, map[string]any{"ops": []map[string]string{{"pat": "-$A", "out": "-other"}}, "paths": []string{"a.js"}})
	if err != nil || !strings.Contains(resultText(result), "+-other;") {
		t.Fatalf("leading dash: %s %v", resultText(result), err)
	}
}

func TestASTApprovalDoesNotHoldAdmissionOrFileLocks(t *testing.T) {
	bin, cwd := t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin)
	codingFakeBinary(t, bin, "ast-grep", `printf '%s' '[{"range":{"byteOffset":{"start":5,"end":6}},"replacement":"2"}]'`)
	writeFile(t, filepath.Join(cwd, ".harness", "settings.json"), "{\"x\":1}\n")
	held := false
	opts := ToolsOptions{AdmitProcess: func(context.Context) (func(), error) {
		if held {
			return nil, errors.New("process slot already held")
		}
		held = true
		return func() {
			if !held {
				t.Error("released process slot twice")
			}
			held = false
		}, nil
	}}
	called := false
	opts.ApproveHookSettings = func(ctx context.Context, _, _ string, _ []string) error {
		called = true
		if held {
			t.Fatal("approval holds a process slot")
		}
		release, err := opts.AdmitProcess(ctx)
		if err != nil {
			return err
		}
		release()
		// The same queue key can be acquired during approval.
		return mutationQueue(opts).Do(filepath.Join(cwd, ".harness", "settings.json"), func() error { return os.ErrPermission })
	}
	tool, err := NewTool(ASTEditTool, cwd, opts)
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"ops": []map[string]string{{"pat": "1", "out": "2"}}, "paths": []string{".harness/settings.json"}}
	preview, err := executeTool(t, tool, args)
	if err != nil {
		t.Fatal(err)
	}
	args["apply"], args["preview"] = true, astPreviewToken(t, preview.Details)
	if _, err := executeTool(t, tool, args); !errors.Is(err, os.ErrPermission) || !called || held {
		t.Fatalf("approval: %v called=%v held=%v", err, called, held)
	}
}

func TestCodingLargeOutputsSpill(t *testing.T) {
	bin, cwd, session := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin)
	full := strings.Repeat("large output line\n", 4000)
	output := filepath.Join(t.TempDir(), "large.txt")
	writeFile(t, output, full)
	t.Setenv("LARGE_OUTPUT", output)
	codingFakeBinary(t, bin, "gh", `case "$*" in 'repo view --json url') printf '{}';; *) /bin/cat "$LARGE_OUTPUT";; esac`)
	tool, _ := NewTool(GHTool, cwd, ToolsOptions{SessionDataDir: session})
	result, err := executeTool(t, tool, map[string]any{"op": "run_logs", "run": "1"})
	if err != nil {
		t.Fatal(err)
	}
	assertSpill := func(result agent.AgentToolResult, want string) {
		t.Helper()
		if len(resultText(result)) > truncate.DefaultMaxBytes+1024 {
			t.Fatal("output exceeds context cap")
		}
		var detail codingToolDetails
		if err := json.Unmarshal(result.Details, &detail); err != nil || detail.FullOutputPath == "" {
			t.Fatalf("no spill: %s %v", result.Details, err)
		}
		if strings.Contains(string(result.Details), `"diff"`) {
			t.Fatal("duplicated diff in Details")
		}
		content, err := os.ReadFile(detail.FullOutputPath)
		if err != nil || !strings.Contains(string(content), want) {
			t.Fatalf("full output missing: %v", err)
		}
		read, _ := NewTool(ReadTool, cwd, ToolsOptions{SessionDataDir: session})
		if _, err := executeTool(t, read, map[string]any{"path": detail.FullOutputPath + ":1-2"}); err != nil {
			t.Fatalf("unreadable spill: %v", err)
		}
	}
	assertSpill(result, "large output line")
	writeFile(t, filepath.Join(cwd, "a.js"), "old();\n")
	largeRewrite := strings.Repeat("new();\n", 4000)
	match := astMatch{Replacement: &largeRewrite}
	match.Range.ByteOffset.End = 6
	protocol, err := json.Marshal([]astMatch{match})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, output, string(protocol))
	codingFakeBinary(t, bin, "ast-grep", `/bin/cat "$LARGE_OUTPUT"`)
	tool, err = NewTool(ASTEditTool, cwd, ToolsOptions{SessionDataDir: session})
	if err != nil {
		t.Fatal(err)
	}
	result, err = executeTool(t, tool, map[string]any{"ops": []map[string]string{{"pat": "old()", "out": "new()"}}, "paths": []string{"a.js"}})
	if err != nil {
		t.Fatal(err)
	}
	assertSpill(result, "+new();")
	if astPreviewToken(t, result.Details) == "" {
		t.Fatal("spill lost preview token")
	}
}

func TestCodingLargeErrorsSpill(t *testing.T) {
	bin, cwd, session := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin)
	output := filepath.Join(t.TempDir(), "diagnostic.txt")
	writeFile(t, output, strings.Repeat("failed log line\n", 4000))
	t.Setenv("LARGE_DIAGNOSTIC", output)
	codingFakeBinary(t, bin, "gh", `case "$*" in 'repo view --json url') printf '{}';; *) /bin/cat "$LARGE_DIAGNOSTIC" >&2;exit 1;;esac`)
	tool, _ := NewTool(GHTool, cwd, ToolsOptions{SessionDataDir: session})
	_, err := executeTool(t, tool, map[string]any{"op": "run_logs", "run": "1"})
	if err == nil || len(err.Error()) > truncate.DefaultMaxBytes+1024 {
		t.Fatalf("unbounded diagnostic: %v", err)
	}
	_, path, ok := strings.Cut(err.Error(), "Full output: ")
	if !ok {
		t.Fatalf("missing diagnostic artifact: %v", err)
	}
	path = strings.TrimSuffix(path, "]")
	content, readErr := os.ReadFile(path)
	if readErr != nil || strings.Count(string(content), "failed log line") != 4000 {
		t.Fatalf("incomplete diagnostic artifact: %v", readErr)
	}
}
