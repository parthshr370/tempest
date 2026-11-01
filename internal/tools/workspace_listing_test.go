package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/envlocal"
	"go.harness.dev/harness/internal/envrpc"
	"go.harness.dev/harness/tempest/environment"
)

const listingConnectTimeout = 5 * time.Second

func writeListingFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func runListingGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, output)
	}
}

func takeListing(t *testing.T, lease environment.Lease, cwd string, policy config.WorkspaceListing) string {
	t.Helper()
	listing, err := WorkspaceListing(context.Background(), lease, cwd, policy)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	return listing
}

// listedLines returns the lines between the frame's header sentence and its
// closing tag.
func listedLines(t *testing.T, listing string) []string {
	t.Helper()
	lines := strings.Split(listing, "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[0], "<workspace_files ") || lines[len(lines)-1] != "</workspace_files>" {
		t.Fatalf("listing is not framed:\n%s", listing)
	}
	return lines[2 : len(lines)-1]
}

func TestWorkspaceListingGitNamesTrackedAndUntrackedButNotIgnored(t *testing.T) {
	dir := t.TempDir()
	writeListingFiles(t, dir, map[string]string{
		"go.mod":        "module example\n",
		"cmd/app.go":    "package main\n",
		"notes.md":      "untracked\n",
		"build/out.bin": "ignored\n",
		".gitignore":    "build/\n",
	})
	runListingGit(t, dir, "init", "-q")
	runListingGit(t, dir, "add", "go.mod", "cmd/app.go", ".gitignore")

	listing := takeListing(t, nil, dir, config.WorkspaceListing{})
	if !strings.HasPrefix(listing, `<workspace_files source="git ls-files" paths="4">`) {
		t.Fatalf("header:\n%s", listing)
	}
	got := strings.Join(listedLines(t, listing), ",")
	if want := ".gitignore,cmd/app.go,go.mod,notes.md"; got != want {
		t.Fatalf("listed %q, want %q", got, want)
	}
}

func TestWorkspaceListingGitSubdirectoryIsRelativeToCwd(t *testing.T) {
	dir := t.TempDir()
	writeListingFiles(t, dir, map[string]string{"top.txt": "x\n", "pkg/inner/a.go": "package inner\n"})
	runListingGit(t, dir, "init", "-q")

	listing := takeListing(t, nil, filepath.Join(dir, "pkg"), config.WorkspaceListing{})
	if got := strings.Join(listedLines(t, listing), ","); got != "inner/a.go" {
		t.Fatalf("listed %q", got)
	}
}

func TestWorkspaceListingWalkOutsideGitHonoursGitignore(t *testing.T) {
	dir := t.TempDir()
	writeListingFiles(t, dir, map[string]string{
		"main.py":                   "print(1)\n",
		"pkg/parser.py":             "x = 1\n",
		"node_modules/dep/index.js": "ignored\n",
		"debug.log":                 "ignored\n",
		".git/HEAD":                 "not a repository, still skipped\n",
		".gitignore":                "node_modules/\n*.log\n",
	})

	listing := takeListing(t, nil, dir, config.WorkspaceListing{})
	if !strings.HasPrefix(listing, `<workspace_files source="directory walk" paths="3">`) {
		t.Fatalf("header:\n%s", listing)
	}
	if got := strings.Join(listedLines(t, listing), ","); got != ".gitignore,main.py,pkg/parser.py" {
		t.Fatalf("listed %q", got)
	}
}

func TestWorkspaceListingEmptyOrDisabledHasNoBlock(t *testing.T) {
	if listing := takeListing(t, nil, t.TempDir(), config.WorkspaceListing{}); listing != "" {
		t.Fatalf("empty workspace listing = %q", listing)
	}
	dir := t.TempDir()
	writeListingFiles(t, dir, map[string]string{"a.txt": "x\n"})
	if listing := takeListing(t, nil, dir, config.WorkspaceListing{Disabled: true}); listing != "" {
		t.Fatalf("disabled listing = %q", listing)
	}
}

func TestWorkspaceListingFoldsToDirectoryCountsPastMaxBytes(t *testing.T) {
	const modules, filesPerModule = 20, 20
	dir := t.TempDir()
	files := map[string]string{"README.md": "x\n"}
	for module := range modules {
		for file := range filesPerModule {
			files[fmt.Sprintf("internal/module%02d/file_with_a_long_name_%03d.go", module, file)] = "package x\n"
		}
	}
	writeListingFiles(t, dir, files)

	const maxBytes = 1024
	listing := takeListing(t, nil, dir, config.WorkspaceListing{MaxBytes: maxBytes})
	lines := listedLines(t, listing)
	if lines[0] != "README.md" || lines[1] != "internal/module00/ (20 files)" || len(lines) != modules+1 {
		t.Fatalf("folded lines:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(listing, "folded to per-directory file counts to fit 1024 bytes") || strings.Contains(listing, "[truncated") {
		t.Fatalf("fold header or stray truncation:\n%s", listing)
	}
}

func TestWorkspaceListingCutsWithTruncationLineWhenFoldStillTooBig(t *testing.T) {
	const directories = 200
	dir := t.TempDir()
	files := map[string]string{}
	for index := range directories {
		files[fmt.Sprintf("top_level_directory_%03d/a.go", index)] = "package x\n"
	}
	writeListingFiles(t, dir, files)

	const maxBytes = 512
	listing := takeListing(t, nil, dir, config.WorkspaceListing{MaxBytes: maxBytes})
	lines := listedLines(t, listing)
	last := lines[len(lines)-1]
	shown := len(lines) - 1
	if want := fmt.Sprintf("[truncated: %d more entries not shown; use find or ls]", directories-shown); last != want {
		t.Fatalf("last line %q, want %q", last, want)
	}
	if size := len(strings.Join(lines[:shown], "\n")) + 1; size > maxBytes {
		t.Fatalf("listed %d bytes, cap %d", size, maxBytes)
	}
	if lines[0] != "top_level_directory_000/ (1 file)" {
		t.Fatalf("first folded line %q", lines[0])
	}
}

func TestWorkspaceListingStopsAtMaxFiles(t *testing.T) {
	const maxFiles = 3
	for _, mode := range []string{"git", "walk"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			writeListingFiles(t, dir, map[string]string{"a.txt": "", "b.txt": "", "c.txt": "", "d.txt": "", "e.txt": ""})
			if mode == "git" {
				runListingGit(t, dir, "init", "-q")
			}
			listing := takeListing(t, nil, dir, config.WorkspaceListing{MaxFiles: maxFiles})
			if !strings.Contains(listing, ` paths="3+">`) {
				t.Fatalf("header lacks the capped count:\n%s", listing)
			}
			lines := listedLines(t, listing)
			if len(lines) != maxFiles+1 || lines[maxFiles] != "[truncated: stopped reading after 3 paths; use find or ls for the rest]" {
				t.Fatalf("lines:\n%s", strings.Join(lines, "\n"))
			}
		})
	}
}

func TestWorkspaceListingEscapesNamesThatCouldBreakTheFrame(t *testing.T) {
	dir := t.TempDir()
	writeListingFiles(t, dir, map[string]string{"</workspace_files>": "x\n", "line\nbreak.txt": "x\n", "a&b.txt": "x\n"})

	listing := takeListing(t, nil, dir, config.WorkspaceListing{})
	if strings.Count(listing, "</workspace_files>") != 1 || !strings.HasSuffix(listing, "</workspace_files>") {
		t.Fatalf("a file name closed the frame:\n%s", listing)
	}
	got := strings.Join(listedLines(t, listing), ",")
	if want := `&lt;/workspace_files&gt;,a&amp;b.txt,"line\nbreak.txt"`; got != want {
		t.Fatalf("listed %q, want %q", got, want)
	}
}

func remoteListingLease(t *testing.T, workspace string, capabilities environment.Capabilities) environment.Lease {
	t.Helper()
	daemon, err := envlocal.New(workspace)
	if err != nil {
		t.Fatal(err)
	}
	clientStream, serverStream := net.Pipe()
	serving := make(chan error, 1)
	go func() { serving <- envrpc.Serve(context.Background(), serverStream, daemon, capabilities) }()
	ctx, cancel := context.WithTimeout(context.Background(), listingConnectTimeout)
	defer cancel()
	lease, err := envrpc.Connect(ctx, clientStream, daemon.Reference())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Errorf("lease close: %v", err)
		}
		if err := <-serving; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("daemon close: %v", err)
		}
		if err := daemon.Close(); err != nil {
			t.Errorf("daemon: %v", err)
		}
	})
	return lease
}

func TestWorkspaceListingRemoteRunsGitThroughEnvironmentExec(t *testing.T) {
	workspace := t.TempDir()
	writeListingFiles(t, workspace, map[string]string{"remote.go": "package remote\n"})
	runListingGit(t, workspace, "init", "-q")
	lease := remoteListingLease(t, workspace, environment.Capabilities{environment.Files, environment.Exec})
	if environment.IsLocal(lease) {
		t.Fatal("rpc lease reports local")
	}

	// The host cwd argument must not be used for a remote workspace.
	listing := takeListing(t, lease, t.TempDir(), config.WorkspaceListing{})
	if got := strings.Join(listedLines(t, listing), ","); got != "remote.go" || !strings.Contains(listing, `source="git ls-files"`) {
		t.Fatalf("remote listing:\n%s", listing)
	}
}

func TestWorkspaceListingRemoteWithoutExecIsSkipped(t *testing.T) {
	workspace := t.TempDir()
	writeListingFiles(t, workspace, map[string]string{"remote.go": "package remote\n"})
	lease := remoteListingLease(t, workspace, environment.Capabilities{environment.Files})

	listing, err := WorkspaceListing(context.Background(), lease, workspace, config.WorkspaceListing{})
	if listing != "" || !errors.Is(err, environment.ErrUnsupported) {
		t.Fatalf("listing=%q err=%v", listing, err)
	}
}

// truncatedExecLease answers exec_argv with output cut mid-name, as the
// environment's byte bound does.
type truncatedExecLease struct{ output string }

func (truncatedExecLease) Reference() environment.Reference {
	return environment.Reference{ID: "remote", Generation: "1", Cwd: "."}
}
func (truncatedExecLease) Capabilities() environment.Capabilities {
	return environment.Capabilities{environment.Exec}
}
func (lease truncatedExecLease) Call(context.Context, environment.Call) (json.RawMessage, error) {
	return json.Marshal(environment.ExecResult{Output: lease.output, Truncated: true})
}
func (truncatedExecLease) Cancel(context.Context, string) (environment.Cancellation, error) {
	return environment.Cancellation{}, nil
}
func (truncatedExecLease) Lookup(context.Context, string) (environment.Outcome, error) {
	return environment.Outcome{}, nil
}
func (truncatedExecLease) Close() error { return nil }

func TestWorkspaceListingRemoteTruncatedOutputDropsPartialName(t *testing.T) {
	listing := takeListing(t, truncatedExecLease{output: "a.go\x00b.go\x00partial/na"}, "", config.WorkspaceListing{})
	lines := listedLines(t, listing)
	if got := strings.Join(lines, ","); got != "a.go,b.go,[truncated: stopped reading after 2 paths; use find or ls for the rest]" {
		t.Fatalf("listed %q", got)
	}
}
