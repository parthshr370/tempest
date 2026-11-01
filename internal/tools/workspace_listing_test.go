package tools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

// withoutRg leaves only git on PATH, so a non-git workspace is listed by the
// walk even where rg is installed.
func withoutRg(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	if git, err := exec.LookPath("git"); err == nil {
		if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
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
	withoutRg(t)
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

// The walk applies every .gitignore at its own level, as git does. The same
// tree is listed by the walk and then, once it is a repository, by git; the
// two must agree, and the cases the walk must get right are named.
func TestWorkspaceListingWalkMatchesGitForNestedIgnoreFiles(t *testing.T) {
	withoutRg(t)
	// Isolate git from the machine's global excludes file.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	writeListingFiles(t, dir, map[string]string{
		".gitignore":              "*.log\n!keep.log\n/docs/build\ndocs/out\nvendor/\n*.gen\ncache/\n!cache/keep.txt\n\\#hash.txt\ntrail.txt   \n**/tmp\n",
		"a.log":                   "",
		"keep.log":                "",
		"sub/b.log":               "",
		"sub/keep.log":            "",
		"docs/build/x.txt":        "",
		"docs/out/y.txt":          "",
		"nested/docs/out/z.txt":   "",
		"nested/docs/build/w.txt": "",
		"vendor/v.go":             "",
		"nested/vendor/w.go":      "",
		"top.gen":                 "",
		"cache/keep.txt":          "",
		"#hash.txt":               "",
		"trail.txt":               "",
		"a/b/tmp/t.txt":           "",
		"pkg/.gitignore":          "secret.key\n!*.gen\n/local.txt\n",
		"pkg/secret.key":          "",
		"pkg/deep/secret.key":     "",
		"pkg/x.gen":               "",
		"pkg/local.txt":           "",
		"pkg/deep/local.txt":      "",
		"pkg/public.go":           "",
	})

	walked := listedLines(t, takeListing(t, nil, dir, config.WorkspaceListing{}))
	want := []string{".gitignore", "keep.log", "nested/docs/build/w.txt", "nested/docs/out/z.txt", "pkg/.gitignore", "pkg/deep/local.txt", "pkg/public.go", "pkg/x.gen", "sub/keep.log"}
	if got := strings.Join(walked, ","); got != strings.Join(want, ",") {
		t.Fatalf("walk listed %q, want %q", got, strings.Join(want, ","))
	}
	runListingGit(t, dir, "init", "-q")
	if got := listedLines(t, takeListing(t, nil, dir, config.WorkspaceListing{})); strings.Join(got, ",") != strings.Join(walked, ",") {
		t.Fatalf("walk %q differs from git ls-files %q", walked, got)
	}
}

// A .gitignore that is not a regular file is never opened: a FIFO would
// block the read forever. The listing still returns well inside its deadline
// and, like git, names only regular files and symbolic links.
func TestWorkspaceListingWalkSkipsNonRegularIgnoreFile(t *testing.T) {
	withoutRg(t)
	dir := t.TempDir()
	writeListingFiles(t, dir, map[string]string{"a.go": "package a\n", "pkg/b.go": "package pkg\n"})
	for _, name := range []string{".gitignore", "pkg/.gitignore"} {
		if err := syscall.Mkfifo(filepath.Join(dir, name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	type outcome struct {
		listing string
		err     error
	}
	listed := make(chan outcome, 1)
	go func() {
		listing, err := WorkspaceListing(context.Background(), nil, dir, config.WorkspaceListing{})
		listed <- outcome{listing, err}
	}()
	select {
	case result := <-listed:
		if result.err != nil {
			t.Fatalf("listing: %v", result.err)
		}
		if got := strings.Join(listedLines(t, result.listing), ","); got != "a.go,pkg/b.go" {
			t.Fatalf("listed %q", got)
		}
	case <-time.After(workspaceListingTimeout + time.Second):
		// Release the blocked reader so the test binary can exit.
		for _, name := range []string{".gitignore", "pkg/.gitignore"} {
			if writer, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				writer.Close()
			}
		}
		t.Fatal("listing blocked on a FIFO .gitignore past its deadline")
	}
}

// rg opens every .ignore and .gitignore it meets, so a FIFO among them
// blocks it before it prints a name. With no name by half the budget the
// rg run is dropped and the walk, which never opens a FIFO, lists the
// workspace in the time left.
func TestWorkspaceListingWalksWhenRgListsNothingByHalfTheBudget(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	for _, fifo := range []string{".ignore", "pkg/.gitignore"} {
		t.Run(fifo, func(t *testing.T) {
			dir := t.TempDir()
			writeListingFiles(t, dir, map[string]string{"a.go": "package a\n", "pkg/b.go": "package pkg\n"})
			if err := syscall.Mkfifo(filepath.Join(dir, fifo), 0o600); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			listing, err := WorkspaceListing(context.Background(), nil, dir, config.WorkspaceListing{})
			if err != nil {
				t.Fatalf("listing after %s: %v", time.Since(start), err)
			}
			if !strings.HasPrefix(listing, `<workspace_files source="directory walk" paths="2">`) {
				t.Fatalf("header:\n%s", listing)
			}
			if got := strings.Join(listedLines(t, listing), ","); got != "a.go,pkg/b.go" {
				t.Fatalf("listed %q", got)
			}
		})
	}
}

// An ignore file larger than the read bound is skipped whole, as git skips
// one past its own limit, instead of being read into memory.
func TestWorkspaceListingWalkSkipsOversizedIgnoreFile(t *testing.T) {
	withoutRg(t)
	dir := t.TempDir()
	padding := strings.Repeat("#\n", ignoreFileMaxBytes/len("#\n"))
	writeListingFiles(t, dir, map[string]string{"a.go": "package a\n", "big.txt": "x\n", ".gitignore": "big.txt\n" + padding})

	if got := strings.Join(listedLines(t, takeListing(t, nil, dir, config.WorkspaceListing{})), ","); got != ".gitignore,a.go,big.txt" {
		t.Fatalf("listed %q", got)
	}
}

// After the deadline the listing returns without starting the walk.
func TestWorkspaceListingExpiredContextDoesNotWalk(t *testing.T) {
	dir := t.TempDir()
	writeListingFiles(t, dir, map[string]string{"a.go": "package a\n"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	listing, err := WorkspaceListing(ctx, nil, dir, config.WorkspaceListing{})
	if listing != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("listing=%q err=%v", listing, err)
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
	for _, source := range []string{gitListingSource, rgListingSource, walkListingSource} {
		t.Run(source, func(t *testing.T) {
			dir := t.TempDir()
			writeListingFiles(t, dir, map[string]string{"a.txt": "", "b.txt": "", "c.txt": "", "d.txt": "", "e.txt": ""})
			switch source {
			case gitListingSource:
				runListingGit(t, dir, "init", "-q")
			case rgListingSource:
				if _, err := exec.LookPath("rg"); err != nil {
					t.Skip("rg is not installed")
				}
			case walkListingSource:
				withoutRg(t)
			}
			listing := takeListing(t, nil, dir, config.WorkspaceListing{MaxFiles: maxFiles})
			if !strings.Contains(listing, ` paths="3+">`) || !strings.Contains(listing, `source="`+source+`"`) {
				t.Fatalf("header lacks the source or capped count:\n%s", listing)
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

// remoteListingLease serves workspace through the environment RPC with the
// operations a daemon binds, and returns the host's remote lease.
func remoteListingLease(t *testing.T, workspace string, capabilities environment.Capabilities) environment.Lease {
	t.Helper()
	daemon, err := envlocal.New(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := BindEnvironment(daemon, ToolsOptions{SessionDataDir: t.TempDir()}); err != nil {
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

func TestWorkspaceListingRemoteRunsGitInTheEnvironment(t *testing.T) {
	workspace := t.TempDir()
	writeListingFiles(t, workspace, map[string]string{"remote.go": "package remote\n"})
	runListingGit(t, workspace, "init", "-q")
	lease := remoteListingLease(t, workspace, environment.Capabilities{environment.Files, environment.Search})
	if environment.IsLocal(lease) {
		t.Fatal("rpc lease reports local")
	}

	// The host cwd argument must not be used for a remote workspace.
	listing := takeListing(t, lease, t.TempDir(), config.WorkspaceListing{})
	if got := strings.Join(listedLines(t, listing), ","); got != "remote.go" || !strings.Contains(listing, `source="git ls-files"`) {
		t.Fatalf("remote listing:\n%s", listing)
	}
}

// The environment's git must ignore GIT_* routing in the daemon's own
// environment, exactly as the local listing does: here GIT_INDEX_FILE names
// another repository's index, whose file names must not reach the model.
func TestWorkspaceListingRemoteIgnoresInheritedGitRouting(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	writeListingFiles(t, workspace, map[string]string{"remote.go": "package remote\n"})
	writeListingFiles(t, outside, map[string]string{"outside-secret-name.txt": "x\n"})
	runListingGit(t, workspace, "init", "-q")
	runListingGit(t, outside, "init", "-q")
	runListingGit(t, outside, "add", ".")
	t.Setenv("GIT_INDEX_FILE", filepath.Join(outside, ".git", "index"))
	t.Setenv("GIT_WORK_TREE", outside)

	lease := remoteListingLease(t, workspace, environment.Capabilities{environment.Files, environment.Search})
	for name, listing := range map[string]string{
		"local":  takeListing(t, nil, workspace, config.WorkspaceListing{}),
		"remote": takeListing(t, lease, workspace, config.WorkspaceListing{}),
	} {
		if got := strings.Join(listedLines(t, listing), ","); got != "remote.go" {
			t.Errorf("%s listing followed inherited GIT_* routing: %q", name, got)
		}
	}
}

// A remote workspace that is not a git work tree gets the same ignore-aware
// walk as a local one, run inside the environment.
func TestWorkspaceListingRemoteWalksNonGitWorkspace(t *testing.T) {
	withoutRg(t)
	workspace := t.TempDir()
	writeListingFiles(t, workspace, map[string]string{"main.py": "print(1)\n", "pkg/.gitignore": "secret.key\n", "pkg/secret.key": "private\n", "pkg/public.py": "x = 1\n"})
	lease := remoteListingLease(t, workspace, environment.Capabilities{environment.Files, environment.Search})

	listing := takeListing(t, lease, t.TempDir(), config.WorkspaceListing{})
	if !strings.Contains(listing, `source="directory walk"`) {
		t.Fatalf("remote non-git listing:\n%s", listing)
	}
	if got, want := strings.Join(listedLines(t, listing), ","), "main.py,pkg/.gitignore,pkg/public.py"; got != want {
		t.Fatalf("listed %q, want %q", got, want)
	}
}

func TestWorkspaceListingRemoteWithoutSearchIsSkipped(t *testing.T) {
	workspace := t.TempDir()
	writeListingFiles(t, workspace, map[string]string{"remote.go": "package remote\n"})
	lease := remoteListingLease(t, workspace, environment.Capabilities{environment.Files, environment.Exec})

	listing, err := WorkspaceListing(context.Background(), lease, workspace, config.WorkspaceListing{})
	if listing != "" || !errors.Is(err, environment.ErrUnsupported) {
		t.Fatalf("listing=%q err=%v", listing, err)
	}
}

// The environment returns whole names up to its reply bound and says there
// are more, so a huge remote workspace still yields a usable listing.
func TestWorkspaceListingRemoteReplyStopsAtByteBound(t *testing.T) {
	const nameLength = 100
	fileCount := remoteListingMaxBytes/nameLength + 1
	workspace := t.TempDir()
	files := make(map[string]string, fileCount)
	for index := range fileCount {
		name := fmt.Sprintf("%05d", index)
		files[name+strings.Repeat("x", nameLength-1-len(name))] = ""
	}
	writeListingFiles(t, workspace, files)
	lease := remoteListingLease(t, workspace, environment.Capabilities{environment.Files, environment.Search})

	listing := takeListing(t, lease, workspace, config.WorkspaceListing{MaxBytes: remoteListingMaxBytes * 2})
	lines := listedLines(t, listing)
	read := len(lines) - 1
	if want := fmt.Sprintf("[truncated: stopped reading after %d paths; use find or ls for the rest]", read); lines[read] != want {
		t.Fatalf("last line %q, want %q", lines[read], want)
	}
	if read == 0 || read*nameLength > remoteListingMaxBytes {
		t.Fatalf("remote reply held %d names of %d bytes, bound %d", read, nameLength, remoteListingMaxBytes)
	}
}
