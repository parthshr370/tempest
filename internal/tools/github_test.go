package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/refs"
)

const githubIssueFields = "number,title,state,body,url,author,labels,createdAt,updatedAt,comments"
const githubPRFields = "number,title,state,body,url,author,labels,createdAt,updatedAt,baseRefName,headRefName,headRefOid,isDraft,mergeStateStatus,reviewDecision,files,reviews,comments"

func TestGitHubFakeCLIProtocol(t *testing.T) {
	bin := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("PATH", bin)
	codingFakeBinary(t, bin, "gh", `[ "$GH_PROMPT_DISABLED" = 1 ]
[ "$GH_PAGER" = cat ]
case "$*" in
 'issue view 7 --json `+githubIssueFields+` --repo owner/repo') printf '{"number":7,"title":"issue title"}';;
 'pr view 7 --json `+githubPRFields+` --repo owner/repo') printf '{"number":7,"title":"PR title"}';;
 'pr diff 7 --color never --repo owner/repo') printf 'diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\ndiff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-old2\n+new2\n';;
 'repo view owner/repo --json nameWithOwner,description,url,defaultBranchRef,visibility,isArchived,primaryLanguage,stargazerCount,forkCount') printf '{"nameWithOwner":"owner/repo"}';;
 'repo view --json nameWithOwner') printf '{"nameWithOwner":"owner/repo"}';;
 'api --method GET repos/owner/repo/contents/src/a.go?ref=feature%2Ftest') printf '{"type":"file","encoding":"base64","content":"cGFja2FnZSBhCg=="}';;
 'api --method GET search/issues -f q=broken tests is:issue repo:owner/repo -f per_page=12') printf '{"items":[{"title":"issue search"}]}';;
 'api --method GET search/issues -f q=broken tests is:pr repo:owner/repo -f per_page=12') printf '{"items":[{"title":"PR search"}]}';;
 'api --method GET search/code -f q=broken tests repo:owner/repo -f per_page=12') printf '{"items":[{"path":"a.go"}]}';;
 'api --method GET search/commits -f q=broken tests repo:owner/repo -f per_page=12') printf '{"items":[{"sha":"abc"}]}';;
 'api --method GET search/repositories -f q=broken tests -f per_page=12') printf '{"items":[{"full_name":"owner/repo"}]}';;
 'run view 99 --json databaseId,displayTitle,status,conclusion,url,headBranch,headSha,jobs --repo owner/repo') printf '{"status":"completed","conclusion":"failure"}';;
 'run view 99 --log-failed --repo owner/repo') printf 'failed job output';;
 *) printf 'unexpected argv: %s' "$*" >&2;exit 88;;
esac
`)
	tool, _ := NewTool(GHTool, cwd, ToolsOptions{})
	for _, test := range []struct{ op, want string }{
		{"issue_view", "issue title"}, {"pr_view", "PR title"}, {"pr_diff", "+new2"}, {"repo_view", "owner/repo"}, {"file_read", "package a"},
		{"search_issues", "issue search"}, {"search_prs", "PR search"}, {"search_code", "a.go"}, {"search_commits", "abc"}, {"search_repos", "full_name"}, {"run_watch", "completed"}, {"run_logs", "failed job output"},
	} {
		t.Run(test.op, func(t *testing.T) {
			result, err := executeTool(t, tool, map[string]any{"op": test.op, "repo": "owner/repo", "number": "7", "run": "99", "query": "broken tests", "path": "src/a.go", "branch": "feature/test", "limit": 12})
			if err != nil || !strings.Contains(resultText(result), test.want) {
				t.Fatalf("%s: %s, %v", test.op, resultText(result), err)
			}
		})
	}
	if _, err := executeTool(t, tool, map[string]any{"op": "search_issues", "query": "broken tests", "limit": 12}); err != nil {
		t.Fatalf("default repository search scope: %v", err)
	}
	read, _ := NewTool(ReadTool, cwd, ToolsOptions{})
	for _, test := range []struct{ path, want, absent string }{
		{"issue://owner/repo/7", "issue title", ""}, {"pr://owner/repo/7", "PR title", ""}, {"pr://owner/repo/7/diff", "a/a.go", "+new2"}, {"pr://owner/repo/7/diff/2", "+new2", "+new\n"}, {"pr://owner/repo/7/diff/all:6-6", "+new", "old2"},
	} {
		result, err := executeTool(t, read, map[string]any{"path": test.path})
		text := resultText(result)
		if err != nil || !strings.Contains(text, test.want) || test.absent != "" && strings.Contains(text, test.absent) {
			t.Fatalf("read %s: %s %v", test.path, text, err)
		}
	}
	grepTool, _ := NewTool(GrepTool, cwd, ToolsOptions{})
	if result, err := executeTool(t, grepTool, map[string]any{"path": "issue://owner/repo/7", "pattern": "issue title"}); err != nil || !strings.Contains(resultText(result), "issue title") {
		t.Fatalf("GitHub rendered grep: %s %v", resultText(result), err)
	}
	for _, args := range []map[string]any{{"op": "pr_checkout", "number": "7"}, {"op": "issue_view", "number": "--web"}, {"op": "search_issues", "repo": "-o/../r", "query": "x"}} {
		if _, err := executeTool(t, tool, args); err == nil {
			t.Fatalf("accepted unsafe args: %v", args)
		}
	}
	codingFakeBinary(t, bin, "gh", "printf 'authentication required' >&2; exit 4\n")
	if _, err := executeTool(t, tool, map[string]any{"op": "issue_view", "number": "7"}); err == nil || !strings.Contains(err.Error(), "gh auth login") {
		t.Fatalf("auth error: %v", err)
	}
	if err := os.Remove(filepath.Join(bin, "gh")); err != nil {
		t.Fatal(err)
	}
	if _, err := executeTool(t, tool, map[string]any{"op": "issue_view", "number": "7"}); err == nil || !strings.Contains(err.Error(), "requires gh") {
		t.Fatalf("missing CLI error: %v", err)
	}
}

func TestGitHubCheckoutFakeProtocol(t *testing.T) {
	bin, cwd := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(cwd, ".git", "config"), "")
	t.Setenv("PATH", bin)
	sha := strings.Repeat("a", 40)
	codingFakeBinary(t, bin, "gh", `case "$*" in
 'repo view --json url') printf '{"url":"https://git.corp.example/owner/repo"}';;
 'pr view 7 --repo owner/repo --json number,headRefOid,url')
 [ "$GH_HOST" = git.corp.example ]
 printf '%s' '{"number":7,"headRefOid":"`+sha+`","url":"https://git.corp.example/owner/repo/pull/7"}';;
 *) exit 80;;esac`)
	t.Setenv("CHECKOUT_DIR", filepath.Join(cwd, "review"))
	codingFakeBinary(t, bin, "git", `case "$*" in
 'rev-parse --git-common-dir'|'rev-parse --git-dir') printf '.git';;
 'rev-parse FETCH_HEAD') printf '`+sha+`';;
 'ls-tree -r -z --name-only `+sha+`') printf 'file.txt\0';;
 "-c core.hooksPath=/dev/null worktree add --detach $CHECKOUT_DIR `+sha+`") /bin/mkdir "$CHECKOUT_DIR";;
 *) [ "$1 $2 $3 $4 $5" = '-c core.hooksPath=/dev/null -c credential.helper= -c' ]
 [ "$GIT_TERMINAL_PROMPT" = 0 ]
 case "$6" in "credential.helper=!"*"gh' auth git-credential") ;; *) exit 82;;esac
 shift 6
 [ "$*" = 'fetch --no-tags https://git.corp.example/owner/repo.git refs/pull/7/head' ] || exit 83;;
esac`)
	tool, _ := NewTool(GHCheckoutTool, cwd, ToolsOptions{AdmitProcess: func(ctx context.Context) (func(), error) {
		if _, limited := ctx.Deadline(); limited {
			t.Fatal("checkout has a shared total deadline instead of independent command budgets")
		}
		return func() {}, nil
	}})
	result, err := executeTool(t, tool, map[string]any{"repo": "owner/repo", "number": "7", "path": "review"})
	if err != nil || !strings.Contains(resultText(result), "Checked out PR 7") {
		t.Fatalf("checkout: %s %v", resultText(result), err)
	}
	for _, path := range []string{"review", "../escape", ".git"} {
		if _, err := executeTool(t, tool, map[string]any{"repo": "owner/repo", "number": "7", "path": path}); err == nil {
			t.Fatalf("accepted checkout path %s", path)
		}
	}
}

func TestGitHubCheckoutRealGit(t *testing.T) {
	fixture := newGitHubCheckoutFixture(t)
	tool, _ := NewTool(GHCheckoutTool, fixture.cwd, ToolsOptions{})
	result, err := executeTool(t, tool, map[string]any{"repo": "owner/repo", "number": "7", "path": "review"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(fixture.cwd, "review", "file.txt"))
	if err != nil || string(data) != "PR bytes\n" {
		t.Fatalf("worktree %s %q %v", resultText(result), data, err)
	}
	var metadata struct {
		HeadSHA string `json:"head_sha"`
	}
	if json.Unmarshal(result.Details, &metadata) != nil || metadata.HeadSHA != fixture.sha {
		t.Fatalf("metadata: %s", result.Details)
	}
}

func TestGitHubRealCLIWire(t *testing.T) {
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh is not installed")
	}
	requests := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/search/issues" ||
			r.URL.Query().Get("q") != "broken tests is:issue repo:owner/repo" ||
			r.URL.Query().Get("per_page") != "12" ||
			r.Header.Get("Authorization") != "token local-test-token" {
			requests <- "bad request: " + r.Method + " " + r.URL.String() + " auth=" + r.Header.Get("Authorization")
			http.Error(w, "unexpected wire shape", http.StatusBadRequest)
			return
		}
		requests <- "GET search/issues with exact query and token"
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[{"number":7,"title":"local GitHub proof"}]}`)
	}))
	defer server.Close()
	t.Setenv("GH_HOST", server.Listener.Addr().String())
	t.Setenv("GH_ENTERPRISE_TOKEN", "local-test-token")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	// An absolute local endpoint keeps this protocol proof offline. The tool's
	// relative endpoint and exact argv are checked by TestGitHubFakeCLIProtocol.
	result, err := refs.GitHubCommand(t.Context(), t.TempDir(), []string{"api", "--method", "GET", server.URL + "/search/issues", "-f", "q=broken tests is:issue repo:owner/repo", "-f", "per_page=12"}, true)
	if err != nil || !strings.Contains(string(result), "local GitHub proof") {
		t.Fatalf("real gh protocol: %s %v", result, err)
	}
	if request := <-requests; request != "GET search/issues with exact query and token" {
		t.Fatal(request)
	}
}

type githubCheckoutFixture struct{ git, cwd, remote, bin, sha string }

func testGit(t *testing.T, git, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), git, args...)
	cmd.Dir = cwd
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s %v", args, output, err)
	}
	return strings.TrimSpace(string(output))
}

func newGitHubCheckoutFixture(t *testing.T) githubCheckoutFixture {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip(err)
	}
	fixture := githubCheckoutFixture{git: git, cwd: t.TempDir(), remote: t.TempDir(), bin: t.TempDir()}
	testGit(t, git, fixture.remote, "init", "-q")
	testGit(t, git, fixture.remote, "config", "user.email", "test@example.com")
	testGit(t, git, fixture.remote, "config", "user.name", "Test")
	writeFile(t, filepath.Join(fixture.remote, "file.txt"), "PR bytes\n")
	writeFile(t, filepath.Join(fixture.remote, "settings.json"), `{"hooks":{}}`)
	hook := filepath.Join(fixture.remote, "pre-commit")
	writeFile(t, hook, "#!/bin/sh\nprintf PWNED > \"$HOOK_MARKER\"\n")
	if err := os.Chmod(hook, 0755); err != nil {
		t.Fatal(err)
	}
	testGit(t, git, fixture.remote, "add", ".")
	testGit(t, git, fixture.remote, "commit", "-qm", "PR")
	fixture.sha = testGit(t, git, fixture.remote, "rev-parse", "HEAD")
	testGit(t, git, fixture.remote, "update-ref", "refs/pull/7/head", fixture.sha)
	testGit(t, git, fixture.cwd, "init", "-q")
	testGit(t, git, fixture.cwd, "config", "user.email", "test@example.com")
	testGit(t, git, fixture.cwd, "config", "user.name", "Test")
	testGit(t, git, fixture.cwd, "config", "url."+fixture.remote+".insteadOf", "https://github.com/owner/repo.git")
	codingFakeBinary(t, fixture.bin, "gh", `case "$*" in 'repo view --json url') printf '{"url":"https://github.com/owner/repo"}';; *) printf '%s' '{"number":7,"headRefOid":"`+fixture.sha+`","url":"https://github.com/owner/repo/pull/7"}';; esac`)
	t.Setenv("PATH", fixture.bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return fixture
}

func TestGitHubCheckoutHookSettingsApproval(t *testing.T) {
	fixture := newGitHubCheckoutFixture(t)
	for _, withApproval := range []bool{false, true} {
		called := false
		options := ToolsOptions{}
		if withApproval {
			options.ApproveHookSettings = func(_ context.Context, tool, id string, paths []string) error {
				called = true
				if tool != "gh_checkout" || id == "" || !slices.Contains(paths, ".harness/settings.json") {
					t.Fatalf("computed checkout approval: %s %s %v", tool, id, paths)
				}
				return os.ErrPermission
			}
		}
		tool, _ := NewTool(GHCheckoutTool, fixture.cwd, options)
		_, err := executeTool(t, tool, map[string]any{"repo": "owner/repo", "number": "7", "path": ".harness"})
		if err == nil || withApproval && !called {
			t.Fatalf("approval bypassed: %v called=%v", err, called)
		}
		if _, err := os.Stat(filepath.Join(fixture.cwd, ".harness")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unapproved destination exists: %v", err)
		}
	}
}

func TestGitHubCheckoutRejectsGitDirectoryAliasesRealGit(t *testing.T) {
	fixture := newGitHubCheckoutFixture(t)
	if err := os.RemoveAll(filepath.Join(fixture.cwd, ".git", "hooks")); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "hook-ran")
	t.Setenv("HOOK_MARKER", marker)
	tool, _ := NewTool(GHCheckoutTool, fixture.cwd, ToolsOptions{})
	if err := os.Symlink(filepath.Join(fixture.cwd, ".git"), filepath.Join(fixture.cwd, "metadata-alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := executeTool(t, tool, map[string]any{"repo": "owner/repo", "number": "7", "path": "metadata-alias/hooks"}); err == nil || !strings.Contains(err.Error(), "git directory") {
		t.Fatalf("Git directory alias accepted: %v", err)
	}
	t.Run("case insensitive alias", func(t *testing.T) {
		lower, err := os.Stat(filepath.Join(fixture.cwd, ".git"))
		if err != nil {
			t.Fatal(err)
		}
		upper, err := os.Stat(filepath.Join(fixture.cwd, ".GIT"))
		if err != nil || !os.SameFile(lower, upper) {
			t.Skip("filesystem is case-sensitive")
		}
		if _, err := executeTool(t, tool, map[string]any{"repo": "owner/repo", "number": "7", "path": ".GIT/hooks"}); err == nil || !strings.Contains(err.Error(), "git directory") {
			t.Fatalf(".GIT/hooks accepted: %v", err)
		}
	})
	testGit(t, fixture.git, fixture.cwd, "commit", "--allow-empty", "-qm", "must not run PR hook")
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("PR hook executed: %v", err)
	}
}

func TestGitHubCheckoutCleansFailedWorktreeAndParentsRealGit(t *testing.T) {
	fixture := newGitHubCheckoutFixture(t)
	t.Setenv("REAL_GIT", fixture.git)
	codingFakeBinary(t, fixture.bin, "git", `case "$*" in
 *'worktree add --detach'*) "$REAL_GIT" "$@"; exit 91;;
 *) exec "$REAL_GIT" "$@";;esac`)
	tool, _ := NewTool(GHCheckoutTool, fixture.cwd, ToolsOptions{})
	_, err := executeTool(t, tool, map[string]any{"repo": "owner/repo", "number": "7", "path": "created/parents/review"})
	if err == nil {
		t.Fatal("injected failure ignored")
	}
	if _, err := os.Stat(filepath.Join(fixture.cwd, "created")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created parents remain: %v", err)
	}
	list := testGit(t, fixture.git, fixture.cwd, "worktree", "list", "--porcelain")
	if strings.Contains(list, "review") {
		t.Fatalf("partial worktree remains registered: %s", list)
	}
	if err := os.Remove(filepath.Join(fixture.bin, "git")); err != nil {
		t.Fatal(err)
	}
	if _, err := executeTool(t, tool, map[string]any{"repo": "owner/repo", "number": "7", "path": "created/parents/review"}); err != nil {
		t.Fatalf("retry after cleanup: %v", err)
	}
}

func TestGitHubCheckoutCancellationCleansRealGit(t *testing.T) {
	fixture := newGitHubCheckoutFixture(t)
	t.Setenv("REAL_GIT", fixture.git)
	started := filepath.Join(t.TempDir(), "checkout-started")
	t.Setenv("CHECKOUT_STARTED", started)
	codingFakeBinary(t, fixture.bin, "git", `case "$*" in
 *'worktree add --detach'*) "$REAL_GIT" "$@"; printf initializing > "$PWD/.git/worktrees/review/locked"; printf ready > "$CHECKOUT_STARTED"; while :; do :; done;;
 *) exec "$REAL_GIT" "$@";;esac`)
	tool, _ := NewTool(GHCheckoutTool, fixture.cwd, ToolsOptions{})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := tool.Execute(ctx, "cancel", json.RawMessage(`{"repo":"owner/repo","number":"7","path":"created/review"}`), nil)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("checkout ignored cancellation: %v", err)
	}
	if content, err := os.ReadFile(started); err != nil || string(content) != "ready" {
		t.Fatalf("test never reached a partial locked worktree: %q %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.cwd, "created")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled destination remains: %v", err)
	}
	list := testGit(t, fixture.git, fixture.cwd, "worktree", "list", "--porcelain")
	if strings.Contains(list, "review") {
		t.Fatalf("canceled registration remains: %s", list)
	}
}

func TestGitHubWatchReleasesAdmissionBetweenPolls(t *testing.T) {
	bin, cwd := t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin)
	codingFakeBinary(t, bin, "gh", `case "$*" in 'repo view --json url') printf '{}';; *) printf '{"status":"in_progress"}';; esac`)
	held, commands := 0, 0
	tool, _ := NewTool(GHTool, cwd, ToolsOptions{AdmitProcess: func(context.Context) (func(), error) {
		held++
		commands++
		if held != 1 {
			t.Fatal("nested process admission")
		}
		return func() { held-- }, nil
	}})
	_, err := executeTool(t, tool, map[string]any{"op": "run_watch", "run": "1", "timeout": 1})
	if err == nil || held != 0 || commands < 2 {
		t.Fatalf("watch admission: %v held=%d commands=%d", err, held, commands)
	}
}

func TestGitHubAPIUsesWorkspaceHost(t *testing.T) {
	bin, cwd := t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("GH_HOST", "wrong.example")
	codingFakeBinary(t, bin, "gh", `case "$*" in
 'repo view --json url') printf '{"url":"https://git.corp.example/owner/repo"}';;
 'api --method GET search/code -f q=needle repo:owner/repo -f per_page=30 --hostname git.corp.example')
 [ "$GH_HOST" = git.corp.example ]
 printf '{"items":[{"path":"enterprise.go"}]}';;
 *) printf 'wrong host/argv %s' "$*" >&2;exit 81;;esac`)
	tool, _ := NewTool(GHTool, cwd, ToolsOptions{})
	result, err := executeTool(t, tool, map[string]any{"op": "search_code", "repo": "owner/repo", "query": "needle"})
	if err != nil || !strings.Contains(resultText(result), "enterprise.go") {
		t.Fatalf("Enterprise API: %s %v", resultText(result), err)
	}
}
