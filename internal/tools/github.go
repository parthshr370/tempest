package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/hooks"
	"go.harness.dev/harness/internal/pathutil"
	"go.harness.dev/harness/internal/refs"
	"go.harness.dev/harness/internal/schema"
)

type githubInput struct {
	Op      string `json:"op"`
	Repo    string `json:"repo"`
	Number  string `json:"number"`
	Query   string `json:"query"`
	Path    string `json:"path"`
	Branch  string `json:"branch"`
	Run     string `json:"run"`
	Limit   int    `json:"limit"`
	Timeout int    `json:"timeout"`
}

var githubReadOps = []string{"issue_view", "pr_view", "pr_diff", "repo_view", "file_read", "search_issues", "search_prs", "search_code", "search_commits", "search_repos", "run_watch", "run_logs"}
var githubSearchScope = regexp.MustCompile(`(?:^|\s)-?(?:repo|org|user|owner):\S`)

func newGitHubTool(name ToolName, cwd string, options ToolsOptions) agent.AgentTool {
	props := schema.JSON{"op": schema.JSON{"type": "string", "enum": githubReadOps}, "repo": schema.String(), "number": schema.String(), "query": schema.String(), "path": schema.String(), "branch": schema.String(), "run": schema.String(), "limit": schema.JSON{"type": "integer", "minimum": 1, "maximum": 100}, "timeout": schema.JSON{"type": "integer", "minimum": 1, "maximum": 300}}
	required := []string{"op"}
	description := "Read GitHub issues, PRs, full PR diffs, repositories, files, search results and Actions logs through the gh CLI. repo is owner/repo (default current workspace repository). number and run are positive decimal strings. Search uses the GitHub API JSON protocol. run_watch polls until completed, with a timeout of 1..300 seconds (default 30). run_logs reads failed job logs. For paged PR diffs use read pr://<number>/diff. Requires authenticated gh; never changes GitHub or workspace state."
	if name == GHCheckoutTool {
		props = schema.JSON{"repo": schema.String(), "number": schema.String(), "path": schema.String()}
		required = []string{"number", "path"}
		description = "Check out a GitHub PR in a new detached Git worktree inside the workspace, without switching or overwriting the current checkout. number is a positive decimal string, repo is owner/repo (default current repository), and path is a new workspace directory outside Git metadata. Fetches the numbered PR head from its actual host with gh credentials (separate five-minute fetch budget), verifies its SHA, and approves would-be hook-settings paths before checkout. Needs exec permission; unavailable in children. Git metadata cannot be rewound: successful checkout invalidates older checkpoint targets. Remove it with git worktree remove."
	}
	return agent.AgentTool{Tool: types.Tool{Name: string(name), Description: description, Parameters: strictObjectSchema(props, required...)}, Label: string(name), Execute: func(ctx context.Context, id string, raw json.RawMessage, _ agent.AgentToolUpdateCallback) (toolResult agent.AgentToolResult, toolErr error) {
		defer func() { toolErr = codingToolError(toolErr, options) }()
		var input githubInput
		if err := decodeParams(raw, &input); err != nil {
			return agent.AgentToolResult{}, err
		}
		if input.Repo != "" && !refs.ValidGitHubRepo(input.Repo) {
			return agent.AgentToolResult{}, errors.New("repo must be owner/repo")
		}
		if input.Limit == 0 {
			input.Limit = 30
		}
		if input.Timeout == 0 {
			input.Timeout = 30
		}
		if input.Limit < 1 || input.Limit > 100 || input.Timeout < 1 || input.Timeout > 300 {
			return agent.AgentToolResult{}, errors.New("limit must be 1..100 and timeout 1..300")
		}
		ctx = refs.WithProcessAdmission(ctx, options.AdmitProcess)
		if name == GHCheckoutTool {
			return githubCheckout(ctx, cwd, id, input, options)
		}
		ctx, cancel := context.WithTimeout(ctx, time.Duration(input.Timeout)*time.Second)
		defer cancel()
		text, err := githubRead(ctx, cwd, input)
		if err != nil {
			return agent.AgentToolResult{}, err
		}
		return codingTextResult(text, codingToolDetails{Op: input.Op, Repo: input.Repo}, options)
	}}
}
func githubRead(ctx context.Context, cwd string, input githubInput) (string, error) {
	var args []string
	jsonOutput := true
	addRepo := func(args []string) []string {
		if input.Repo != "" {
			return append(args, "--repo", input.Repo)
		}
		return args
	}
	switch input.Op {
	case "issue_view", "pr_view", "pr_diff":
		if !refs.ValidGitHubNumber(input.Number) {
			return "", errors.New("number must be a positive decimal string")
		}
		kind := "pr"
		if input.Op == "issue_view" {
			kind = "issue"
		}
		target := kind + "://"
		if input.Repo != "" {
			target += input.Repo + "/"
		}
		target += input.Number
		if input.Op == "pr_diff" {
			target += "/diff/all"
		}
		resource, err := refs.ReadGitHub(ctx, cwd, target)
		return string(resource.Data), err
	case "repo_view":
		args = []string{"repo", "view"}
		if input.Repo != "" {
			args = append(args, input.Repo)
		}
		args = append(args, "--json", "nameWithOwner,description,url,defaultBranchRef,visibility,isArchived,primaryLanguage,stargazerCount,forkCount")
	case "file_read":
		repo, err := githubRepo(ctx, cwd, input.Repo)
		if err != nil {
			return "", err
		}
		if input.Path == "" || strings.HasPrefix(input.Path, "/") {
			return "", errors.New("path must be repository-relative")
		}
		var parts []string
		for _, part := range strings.Split(input.Path, "/") {
			if part == "" || part == "." || part == ".." {
				return "", errors.New("invalid repository path")
			}
			parts = append(parts, url.PathEscape(part))
		}
		endpoint := "repos/" + repo + "/contents/" + strings.Join(parts, "/")
		if input.Branch != "" {
			endpoint += "?ref=" + url.QueryEscape(input.Branch)
		}
		out, err := refs.GitHubCommand(ctx, cwd, []string{"api", "--method", "GET", endpoint}, true)
		if err != nil {
			return "", err
		}
		var content struct {
			Type     string `json:"type"`
			Encoding string `json:"encoding"`
			Content  string `json:"content"`
		}
		if err := json.Unmarshal(out, &content); err != nil {
			return "", errors.New("file_read requires a file, not a directory")
		}
		if content.Type != "file" || content.Encoding != "base64" {
			return "", errors.New("GitHub did not return base64 file contents; file may be too large")
		}
		data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(content.Content, "\n", ""))
		if err != nil {
			return "", err
		}
		if strings.IndexByte(string(data), 0) >= 0 {
			return "", errors.New("GitHub file is binary")
		}
		return string(data), nil
	case "search_issues", "search_prs", "search_code", "search_commits", "search_repos":
		if strings.TrimSpace(input.Query) == "" {
			return "", errors.New("query must not be empty")
		}
		endpoint := strings.TrimPrefix(input.Op, "search_")
		query := input.Query
		if endpoint == "prs" {
			endpoint = "issues"
			query += " is:pr"
		} else if endpoint == "issues" {
			query += " is:issue"
		} else if endpoint == "repos" {
			endpoint = "repositories"
		}
		repo := input.Repo
		if endpoint != "repositories" && repo == "" && !githubSearchScope.MatchString(input.Query) {
			repo, _ = githubRepo(ctx, cwd, "")
		}
		if endpoint != "repositories" && repo != "" {
			query += " repo:" + repo
		}
		args = []string{"api", "--method", "GET", "search/" + endpoint, "-f", "q=" + query, "-f", "per_page=" + strconv.Itoa(input.Limit)}
	case "run_watch", "run_logs":
		if !refs.ValidGitHubNumber(input.Run) {
			return "", errors.New("run must be a positive decimal string")
		}
		if input.Op == "run_logs" {
			args = addRepo([]string{"run", "view", input.Run, "--log-failed"})
			jsonOutput = false
			break
		}
		args = addRepo([]string{"run", "view", input.Run, "--json", "databaseId,displayTitle,status,conclusion,url,headBranch,headSha,jobs"})
		for {
			out, err := refs.GitHubCommand(ctx, cwd, args, true)
			if err != nil {
				return "", err
			}
			var run struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(out, &run); err != nil {
				return "", err
			}
			if run.Status == "completed" {
				return string(out), nil
			}
			if run.Status == "" {
				return "", errors.New("gh run view omitted status")
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", fmt.Errorf("run watch stopped before completion: %w; last snapshot: %s", ctx.Err(), out)
			case <-timer.C:
			}
		}
	default:
		return "", fmt.Errorf("unsupported GitHub read operation %q", input.Op)
	}
	out, err := refs.GitHubCommand(ctx, cwd, args, jsonOutput)
	return string(out), err
}
func githubRepo(ctx context.Context, cwd, repo string) (string, error) {
	if repo != "" {
		return repo, nil
	}
	out, err := refs.GitHubCommand(ctx, cwd, []string{"repo", "view", "--json", "nameWithOwner"}, true)
	if err != nil {
		return "", err
	}
	var value struct {
		Name string `json:"nameWithOwner"`
	}
	if json.Unmarshal(out, &value) != nil || !refs.ValidGitHubRepo(value.Name) {
		return "", errors.New("gh could not resolve owner/repo from the workspace; pass repo explicitly")
	}
	return value.Name, nil
}

var githubSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$|^[0-9a-fA-F]{64}$`)

func githubCheckout(ctx context.Context, cwd, id string, input githubInput, options ToolsOptions) (agent.AgentToolResult, error) {
	if !refs.ValidGitHubNumber(input.Number) || input.Path == "" {
		return agent.AgentToolResult{}, errors.New("checkout requires a positive number and new workspace path")
	}
	abs := pathutil.ResolveToCwd(input.Path, cwd)
	_, canonical, err := rootRelPaths(cwd, abs)
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	if canonical == "." {
		return agent.AgentToolResult{}, errors.New("worktree path must be a new workspace directory outside the git directory")
	}
	root, err := os.OpenRoot(cwd)
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	defer root.Close()
	git, err := exec.LookPath("git")
	if err != nil {
		return agent.AgentToolResult{}, errors.New("PR checkout requires git on PATH")
	}
	run := func(ctx context.Context, budget time.Duration, args ...string) ([]byte, error) {
		release, err := admitCodingProcess(ctx, options)
		if err != nil {
			return nil, err
		}
		defer release()
		return runCodingBinaryTimeout(ctx, git, cwd, args, nil, budget)
	}
	// Compare filesystem identities, not spelling: .GIT aliases .git on macOS,
	// and linked worktrees keep their common Git directory elsewhere.
	for _, flag := range []string{"--git-common-dir", "--git-dir"} {
		out, err := run(ctx, 30*time.Second, "rev-parse", flag)
		if err != nil {
			return agent.AgentToolResult{}, err
		}
		info, err := os.Stat(pathutil.ResolveToCwd(strings.TrimSpace(string(out)), cwd))
		if err != nil {
			return agent.AgentToolResult{}, err
		}
		for parent := abs; ; parent = filepath.Dir(parent) {
			if ancestor, err := os.Stat(parent); err == nil && os.SameFile(info, ancestor) {
				return agent.AgentToolResult{}, errors.New("worktree path must be outside the git directory")
			}
			if parent == filepath.Dir(parent) {
				break
			}
		}
	}
	repo, err := githubRepo(ctx, cwd, input.Repo)
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	out, err := refs.GitHubCommand(ctx, cwd, []string{"pr", "view", input.Number, "--repo", repo, "--json", "number,headRefOid,url"}, true)
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	var pr struct {
		Number int    `json:"number"`
		SHA    string `json:"headRefOid"`
		URL    string `json:"url"`
	}
	if json.Unmarshal(out, &pr) != nil || strconv.Itoa(pr.Number) != input.Number || !githubSHA.MatchString(pr.SHA) {
		return agent.AgentToolResult{}, errors.New("gh returned invalid PR checkout metadata")
	}
	u, err := url.Parse(pr.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.EqualFold(u.Path, "/"+repo+"/pull/"+input.Number) {
		return agent.AgentToolResult{}, errors.New("gh returned an invalid PR repository URL")
	}
	gh, err := exec.LookPath("gh")
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	credential := "!" + "'" + strings.ReplaceAll(gh, "'", "'\\''") + "' auth git-credential"
	var result agent.AgentToolResult
	// FETCH_HEAD is shared across destinations. Serialize the entire fetch and
	// verification against the repository rather than just the destination.
	err = mutationQueue(options).Do(cwd, func() (err error) {
		if _, err := root.Lstat(canonical); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("worktree path must not exist: %s", input.Path)
		}
		if _, err := run(ctx, 5*time.Minute, "-c", "core.hooksPath="+os.DevNull, "-c", "credential.helper=", "-c", "credential.helper="+credential, "fetch", "--no-tags", "https://"+u.Host+"/"+repo+".git", "refs/pull/"+input.Number+"/head"); err != nil {
			return err
		}
		head, err := run(ctx, 30*time.Second, "rev-parse", "FETCH_HEAD")
		if err != nil {
			return err
		}
		if !strings.EqualFold(strings.TrimSpace(string(head)), pr.SHA) {
			return errors.New("PR head changed during fetch; checkout refused")
		}
		tree, err := run(ctx, 30*time.Second, "ls-tree", "-r", "-z", "--name-only", pr.SHA)
		if err != nil {
			return err
		}
		var paths []string
		for _, name := range strings.Split(strings.TrimSuffix(string(tree), "\x00"), "\x00") {
			if name != "" {
				paths = append(paths, filepath.ToSlash(filepath.Join(canonical, name)))
			}
		}
		if options.ApproveHookSettings != nil {
			if err := options.ApproveHookSettings(ctx, string(GHCheckoutTool), id, paths); err != nil {
				return err
			}
		} else {
			for _, path := range paths {
				if hooks.HookSettingsPath(path, cwd) {
					return fmt.Errorf("Changing hook settings requires per-call approval: %s", path)
				}
			}
		}
		var parents []string
		started := false
		defer func() {
			if err == nil {
				return
			}
			if started {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				// --force twice also removes an interrupted, locked initialization.
				_, removeErr := run(cleanup, 30*time.Second, "-c", "core.hooksPath="+os.DevNull, "worktree", "remove", "--force", "--force", abs)
				diskErr := root.RemoveAll(canonical)
				_, pruneErr := run(cleanup, 30*time.Second, "worktree", "prune", "--expire", "now")
				if removeErr != nil || diskErr != nil || pruneErr != nil {
					err = errors.Join(err, fmt.Errorf("worktree cleanup: %w", errors.Join(removeErr, diskErr, pruneErr)))
				}
			}
			for i := len(parents) - 1; i >= 0; i-- {
				if removeErr := root.Remove(parents[i]); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					err = errors.Join(err, fmt.Errorf("remove created parent %s: %w", parents[i], removeErr))
				}
			}
		}()
		var missing []string
		for p := filepath.Dir(canonical); p != "."; p = filepath.Dir(p) {
			if _, err := root.Stat(p); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			missing = append(missing, p)
		}
		for i := len(missing) - 1; i >= 0; i-- {
			if err := root.Mkdir(missing[i], 0755); err != nil {
				return err
			}
			parents = append(parents, missing[i])
		}
		started = true
		if _, err := run(ctx, 30*time.Second, "-c", "core.hooksPath="+os.DevNull, "worktree", "add", "--detach", abs, pr.SHA); err != nil {
			return err
		}
		result = textResult("Checked out PR "+input.Number+" in "+input.Path+" at "+pr.SHA, map[string]any{"worktree_path": input.Path, "head_sha": pr.SHA, "url": pr.URL})
		return nil
	})
	return result, err
}
