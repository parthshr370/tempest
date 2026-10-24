package refs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// GitHubURL identifies the read-only issue and pull request schemes.
func GitHubURL(raw string) bool {
	return strings.HasPrefix(raw, "issue://") || strings.HasPrefix(raw, "pr://")
}

var githubRepoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var githubNumberPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

// ValidGitHubRepo rejects flags, URL authority changes and API path traversal.
func ValidGitHubRepo(repo string) bool {
	return githubRepoPattern.MatchString(repo) && !strings.Contains(repo, "/../") && !strings.HasSuffix(repo, "/..")
}

// ValidGitHubNumber accepts numeric issue, pull request and workflow identifiers.
func ValidGitHubNumber(number string) bool { return githubNumberPattern.MatchString(number) }

type processAdmissionKey struct{}

// WithProcessAdmission accounts for each CLI process, not idle polling time.
func WithProcessAdmission(ctx context.Context, admit func(context.Context) (func(), error)) context.Context {
	return context.WithValue(ctx, processAdmissionKey{}, admit)
}

// GitHubCommand invokes the authenticated CLI. Native API output is JSON; CLI
// subcommands use --json where supported. Diff and logs only support text.
func GitHubCommand(ctx context.Context, cwd string, args []string, jsonOutput bool) ([]byte, error) {
	host := os.Getenv("GH_HOST")
	hostLookup := len(args) == 4 && args[0] == "repo" && args[1] == "view" && args[2] == "--json" && args[3] == "url"
	if !hostLookup && !hasGitHubHost(args) {
		if out, err := GitHubCommand(ctx, cwd, []string{"repo", "view", "--json", "url"}, true); err == nil {
			var repo struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(out, &repo) == nil {
				if u, err := url.Parse(repo.URL); err == nil && u.Host != "" {
					host = u.Host
				}
			}
		}
		if len(args) > 0 && args[0] == "api" && host != "" {
			args = append(append([]string(nil), args...), "--hostname", host)
		}
	}
	if admit, _ := ctx.Value(processAdmissionKey{}).(func(context.Context) (func(), error)); admit != nil {
		release, err := admit(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	binary, err := exec.LookPath("gh")
	if err != nil {
		return nil, errors.New("GitHub access requires gh on PATH; install the GitHub CLI and run gh auth login")
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_PAGER=cat", "PAGER=cat", "NO_COLOR=1", "GIT_TERMINAL_PROMPT=0")
	if host != "" {
		cmd.Env = append(cmd.Env, "GH_HOST="+host)
	}
	var out, stderr githubOutput
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		hint := ""
		if errors.As(err, &exit) && exit.ExitCode() == 4 {
			hint = "; authenticate with gh auth login"
		}
		text := strings.TrimSpace(stderr.String())
		lower := strings.ToLower(text)
		if strings.Contains(lower, "authentication") || strings.Contains(lower, "gh auth login") ||
			strings.Contains(lower, "bad credentials") || strings.Contains(lower, "http 401") {
			hint = "; authenticate with gh auth login"
		}
		return nil, fmt.Errorf("gh %s: %w%s: %s", strings.Join(args[:min(2, len(args))], " "), err, hint, text)
	}
	if jsonOutput && !json.Valid(out.Bytes()) {
		return nil, errors.New("gh returned invalid JSON")
	}
	return out.Bytes(), nil
}

func hasGitHubHost(args []string) bool {
	for _, arg := range args {
		if arg == "--hostname" || strings.HasPrefix(arg, "--hostname=") || strings.HasPrefix(arg, "https://") || strings.HasPrefix(arg, "http://") {
			return true
		}
	}
	return false
}

type githubOutput struct{ bytes.Buffer }

func (b *githubOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 8<<20 {
		return 0, errors.New("GitHub output exceeds 8 MiB; narrow the request")
	}
	return b.Buffer.Write(p)
}

type githubTarget struct {
	kind, repo, number, diff string
	query                    url.Values
}

func parseGitHub(raw string) (githubTarget, error) {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok || (scheme != "issue" && scheme != "pr") {
		return githubTarget{}, fmt.Errorf("invalid GitHub URL %q", raw)
	}
	if strings.Contains(rest, "#") {
		return githubTarget{}, errors.New("GitHub URLs do not take fragments")
	}
	path, query, _ := strings.Cut(rest, "?")
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return githubTarget{}, err
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return githubTarget{}, err
	}
	target := githubTarget{kind: scheme, query: values}
	if decoded == "" {
		return target, validateGitHubQuery(target)
	}
	parts := strings.Split(decoded, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00") {
			return target, errors.New("invalid GitHub URL path")
		}
	}
	if ValidGitHubNumber(parts[0]) {
		target.number = parts[0]
		parts = parts[1:]
	} else {
		if len(parts) < 2 || !ValidGitHubRepo(strings.Join(parts[:2], "/")) {
			return target, errors.New("GitHub URL needs owner/repo and an optional positive number")
		}
		target.repo = strings.Join(parts[:2], "/")
		parts = parts[2:]
		if len(parts) > 0 {
			if !ValidGitHubNumber(parts[0]) {
				return target, errors.New("GitHub URL number must be a positive integer")
			}
			target.number = parts[0]
			parts = parts[1:]
		}
	}
	if len(parts) > 0 {
		if target.kind != "pr" || parts[0] != "diff" || len(parts) > 2 || target.number == "" {
			return target, errors.New("only pr://<number>/diff[/all|<file index>] has a subpath")
		}
		target.diff = "list"
		if len(parts) == 2 {
			target.diff = parts[1]
			if target.diff != "all" && !ValidGitHubNumber(target.diff) {
				return target, errors.New("PR diff page must be all or a positive file index")
			}
		}
	}
	return target, validateGitHubQuery(target)
}
func validateGitHubQuery(t githubTarget) error {
	for key, values := range t.query {
		if len(values) != 1 {
			return fmt.Errorf("GitHub query parameter %q must occur once", key)
		}
		switch key {
		case "state", "limit", "author", "label":
			if t.number != "" {
				return fmt.Errorf("%s is a listing parameter", key)
			}
		case "comments":
			if t.number == "" || t.diff != "" || (values[0] != "0" && values[0] != "1") {
				return errors.New("comments=0 or 1 applies to issue and PR views")
			}
		default:
			return fmt.Errorf("unknown GitHub query parameter %q", key)
		}
	}
	if state := t.query.Get("state"); state != "" && state != "open" && state != "closed" && state != "all" && (state != "merged" || t.kind != "pr") {
		return errors.New("invalid GitHub listing state")
	}
	if limit := t.query.Get("limit"); limit != "" {
		n, err := strconv.Atoi(limit)
		if err != nil || n < 1 || n > 100 {
			return errors.New("GitHub listing limit must be 1..100")
		}
	}
	return nil
}

// ReadGitHub resolves URLs against the calling session's cwd, not process cwd.
func ReadGitHub(ctx context.Context, cwd, raw string) (Resource, error) {
	t, err := parseGitHub(raw)
	if err != nil {
		return Resource{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{t.kind}
	isJSON := true
	fields := "number,title,state,body,url,author,labels,createdAt,updatedAt"
	if t.number == "" {
		args = append(args, "list", "--json", "number,title,state,url,author,labels,updatedAt", "--limit", "30")
		for _, key := range []string{"state", "author", "label"} {
			if value := t.query.Get(key); value != "" {
				args = append(args, "--"+key, value)
			}
		}
		if limit := t.query.Get("limit"); limit != "" {
			args[5] = limit
		}
	} else if t.diff != "" {
		args = append(args, "diff", t.number, "--color", "never")
		isJSON = false
	} else {
		if t.kind == "pr" {
			fields += ",baseRefName,headRefName,headRefOid,isDraft,mergeStateStatus,reviewDecision,files,reviews"
		}
		if t.query.Get("comments") != "0" {
			fields += ",comments"
		}
		args = append(args, "view", t.number, "--json", fields)
	}
	if t.repo != "" {
		args = append(args, "--repo", t.repo)
	}
	data, err := GitHubCommand(ctx, cwd, args, isJSON)
	if err != nil {
		return Resource{}, err
	}
	if t.diff != "" {
		data, err = githubDiffPage(data, t.diff, raw)
		if err != nil {
			return Resource{}, err
		}
	} else {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, data, "", "  "); err != nil {
			return Resource{}, err
		}
		data = pretty.Bytes()
	}
	return Resource{URI: raw, Data: data, Rendered: true}, nil
}

var githubDiffHeader = regexp.MustCompile(`(?m)^diff --git `)

func githubDiffPage(data []byte, page, raw string) ([]byte, error) {
	text := string(data)
	var files []string
	headers := githubDiffHeader.FindAllStringIndex(text, -1)
	for i, header := range headers {
		end := len(text)
		if i+1 < len(headers) {
			end = headers[i+1][0]
		}
		files = append(files, text[header[0]:end])
	}
	if page == "all" {
		return data, nil
	}
	if page != "list" {
		index, err := strconv.Atoi(page)
		if err != nil || index < 1 || index > len(files) {
			return nil, fmt.Errorf("PR diff file index %s outside 1..%d", page, len(files))
		}
		return []byte(files[index-1]), nil
	}
	var out strings.Builder
	for i, file := range files {
		first, _, _ := strings.Cut(file, "\n")
		fmt.Fprintf(&out, "%d: %s\n", i+1, first)
	}
	if len(files) == 0 {
		out.WriteString("No changed files.\n")
	} else {
		fmt.Fprintf(&out, "Read %s/<index> for one file, or %s/all for the full diff.\n", strings.Split(raw, "?")[0], strings.Split(raw, "?")[0])
	}
	return []byte(out.String()), nil
}
