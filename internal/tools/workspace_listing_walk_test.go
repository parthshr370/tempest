package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/config"
)

// gitIgnoreScenarioNames are the files every conformance scenario creates
// next to its root .gitignore: escapes, one-byte and non-ASCII names for the
// wildcards and classes, and nested directories for anchoring and `**`.
var gitIgnoreScenarioNames = []string{
	"top.txt", "a.txt", "b.txt", "!.txt", "^.txt", "1.txt", "é.txt", "keep.log", "other.log",
	"#hash", "!bang", "space ", "slash\\", "slash\\ ", "foo ", "trail",
	"dir/keep.txt", "dir/drop.txt", "dir/deep/secret.txt", "nested/dir/x.txt", "nested/top.txt",
	"a/b.txt", "a/keep.txt", "a/b/keep.txt", "a/c/b.txt", "a/c/deep/b.txt", "a/c/secret.txt", "abcab.txt",
	"nested/a.log", "nested/local.txt", "nested/deep/local.txt",
}

// gitIgnoreScenarios are root .gitignore contents. Every scenario also has
// nested/.gitignore re-including *.log and anchoring /local.txt.
var gitIgnoreScenarios = []string{
	"*.log\n!keep.log\n", "/top.txt\n", "dir/\n", "dir/**\n!dir/keep.txt\n", "a/**/b.txt\n",
	"**/secret.txt\n", "**\n!top.txt\n", "a/**\n!a/\n!a/keep.txt\n", "*/\n!dir/\n", "[!a].txt\n",
	"[^a].txt\n", "[[:digit:]].txt\n", "[[:alpha:]].txt\n", "?.txt\n", "\\#hash\n\\!bang\n",
	"space\\ \n", "slash\\\\ \n", "slash\\\\\\ \n", "foo\\ \n", "trail   \n", "dir\\/\n", "**/\n",
	"a**b.txt\n", "/**/keep.txt\n", "dir/*\n!dir/keep.txt\n", "dir/\n!dir/keep.txt\n", "dir/\n!dir/\n",
	"a/**/\n!a/b/\n!a/b/keep.txt\n", "*.log\n", "!\n", "a/***\n", "a/***/b.txt\n",
	"\ufeff*.log\r\n!keep.log\r\n", "[a-c].txt\n", "[]a].txt\n", "[[:bogus:]].txt\n", "a/*/b.txt\n",
	"*\n!*/\n!*.txt\n", "nested/\n!nested/top.txt\n", "/a/c/\n", "**/c/**\n!**/b.txt\n", "*.TXT\n",
}

// The walk lists exactly what `git ls-files --others --exclude-standard`
// lists for the same untracked tree, scenario by scenario.
func TestWorkspaceListingWalkMatchesGitLsFilesOthers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	// Isolate git from the machine's excludes file and configuration.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for index, rules := range gitIgnoreScenarios {
		t.Run(fmt.Sprintf("%02d", index), func(t *testing.T) {
			dir := t.TempDir()
			files := map[string]string{".gitignore": rules, "nested/.gitignore": "!*.log\n/local.txt\n"}
			for _, name := range gitIgnoreScenarioNames {
				files[name] = ""
			}
			writeListingFiles(t, dir, files)
			walked, err := walkWorkspaceFiles(t.Context(), dir, len(files)*2)
			if err != nil {
				t.Fatal(err)
			}
			runListingGit(t, dir, "init", "-q")
			// git init turns case folding on for a case-insensitive file
			// system; the port matches case-sensitively.
			runListingGit(t, dir, "config", "core.ignorecase", "false")
			output, err := exec.Command("git", "-C", dir, "ls-files", "-z", "--others", "--exclude-standard").Output()
			if err != nil {
				t.Fatal(err)
			}
			var listed []string
			if len(output) > 0 {
				listed = strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
			}
			slices.Sort(listed)
			if !slices.Equal(walked.paths, listed) {
				t.Errorf(".gitignore %q:\nwalk %q\ngit  %q", rules, walked.paths, listed)
			}
		})
	}
}

// Outside git, .ignore and .gitignore files apply as rg applies them: a
// matching .ignore pattern outranks every .gitignore, and within each name
// the deepest file with a match decides. The walk gives the expected listing,
// and so does rg when it is installed.
func TestWorkspaceListingWalkHonoursIgnoreFilesLikeRg(t *testing.T) {
	rg, rgErr := exec.LookPath("rg")
	for _, scenario := range []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			name:  "ignore re-includes what gitignore excludes",
			files: map[string]string{".gitignore": "*.log\n", ".ignore": "!keep.log\n", "keep.log": "", "drop.log": ""},
			want:  []string{".gitignore", ".ignore", "keep.log"},
		},
		{
			name:  "root ignore outranks a deeper gitignore",
			files: map[string]string{".ignore": "!b.txt\n", "sub/.gitignore": "b.txt\n", "sub/b.txt": "", "c.txt": ""},
			want:  []string{".ignore", "c.txt", "sub/.gitignore", "sub/b.txt"},
		},
		{
			name:  "ignore exclusion beats a deeper gitignore re-inclusion",
			files: map[string]string{".ignore": "*.key\n", "sub/.gitignore": "!*.key\n", "a.key": "", "sub/b.key": "", "sub/c.txt": ""},
			want:  []string{".ignore", "sub/.gitignore", "sub/c.txt"},
		},
		{
			name:  "an excluded directory is not entered",
			files: map[string]string{".ignore": "build/\n", ".gitignore": "!build/keep.txt\n", "build/keep.txt": "", "src/a.go": ""},
			want:  []string{".gitignore", ".ignore", "src/a.go"},
		},
		{
			name:  "a nested ignore applies below its directory",
			files: map[string]string{"pkg/.ignore": "*.tmp\n", "pkg/a.tmp": "", "b.tmp": "", "pkg/c.go": ""},
			want:  []string{"b.tmp", "pkg/.ignore", "pkg/c.go"},
		},
		{
			name:  "hidden files listed and .git skipped",
			files: map[string]string{".env.example": "", ".git/HEAD": "not a repository\n", "main.go": ""},
			want:  []string{".env.example", "main.go"},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			dir := t.TempDir()
			writeListingFiles(t, dir, scenario.files)
			walked, err := walkWorkspaceFiles(t.Context(), dir, 100)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(walked.paths, scenario.want) {
				t.Errorf("walk %q, want %q", walked.paths, scenario.want)
			}
			if rgErr != nil {
				return
			}
			listed, err := localRgFiles(t.Context(), rg, dir, 100)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(listed.paths, scenario.want) {
				t.Errorf("rg %q, want %q", listed.paths, scenario.want)
			}
		})
	}
}

// With rg installed, a non-git workspace is listed by `rg --files`.
func TestWorkspaceListingRgListsNonGitWorkspace(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg is not installed")
	}
	dir := t.TempDir()
	writeListingFiles(t, dir, map[string]string{".ignore": "*.key\n", "private.key": "", "public.go": "", ".git/HEAD": "x\n"})
	listing := takeListing(t, nil, dir, config.WorkspaceListing{})
	if !strings.HasPrefix(listing, `<workspace_files source="rg --files" paths="2">`) {
		t.Fatalf("header:\n%s", listing)
	}
	if got := strings.Join(listedLines(t, listing), ","); got != ".ignore,public.go" {
		t.Fatalf("listed %q", got)
	}
}

// Once its deadline passes the walking goroutine stops within a small bound,
// even while ignore matching is what takes the time: the walk checks its
// context at every entry and matching checks it as it goes.
func TestWorkspaceListingWalkStopsPromptlyAfterDeadline(t *testing.T) {
	const files, patterns = 2000, 50000
	tree := map[string]string{}
	var rules strings.Builder
	for index := range patterns {
		fmt.Fprintf(&rules, "*q%05d*z\n", index)
	}
	tree[".gitignore"] = rules.String()
	for index := range files {
		tree[fmt.Sprintf("d%02d/f%04d.go", index%50, index)] = ""
	}
	dir := t.TempDir()
	writeListingFiles(t, dir, tree)

	const deadline, bound = 100 * time.Millisecond, 250 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), deadline)
	defer cancel()
	walk := startWorkspaceWalk(ctx, dir, files*2)
	<-ctx.Done()
	select {
	case <-walk.done:
		t.Fatal("the walk finished before its deadline; the workload no longer tests cancellation")
	default:
	}
	select {
	case <-walk.done:
	case <-time.After(bound):
		t.Fatalf("the walk was still running %v after its deadline", bound)
	}
	if !errors.Is(walk.err, context.DeadlineExceeded) {
		t.Fatalf("walk ended with %v", walk.err)
	}
}

// The review's pathological .gitignore, 5000 consecutive `**/` before a name
// that never matches, over a 30-level tree: the walk finishes the whole
// listing well inside its deadline.
func TestWorkspaceListingWalkFinishesPathologicalIgnorePattern(t *testing.T) {
	deep := strings.Repeat("d/", 30) + "file.go"
	dir := t.TempDir()
	writeListingFiles(t, dir, map[string]string{".gitignore": strings.Repeat("**/", 5000) + "nomatch\n", deep: ""})
	ctx, cancel := context.WithTimeout(t.Context(), workspaceListingTimeout)
	defer cancel()
	walked, err := walkWorkspaceFiles(ctx, dir, 100)
	if err != nil || walked.more || !slices.Equal(walked.paths, []string{".gitignore", deep}) {
		t.Fatalf("walk %q more=%v err=%v", walked.paths, walked.more, err)
	}
}
