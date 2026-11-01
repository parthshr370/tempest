// Package discovery collects the fixed V1 sources of session context, rules, skills and agent files.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"go.harness.dev/harness/internal/adapter"
	"go.harness.dev/harness/internal/agentfiles"
	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/contextfile"
	"go.harness.dev/harness/internal/prompt"
	"go.harness.dev/harness/internal/skills"
)

// Options configures startup discovery.
type Options struct {
	Cwd                  string
	RepoRoot             string
	AgentDir             string
	HomeDir              string
	SkillPaths           []string
	IncludeDefaultSkills bool
	// Files names context files and bounds their bytes; zero values take the
	// built-in defaults.
	Files config.ContextFileSettings
	// Providers are the configured model routes an agent file's model spec
	// may name.
	Providers map[string]adapter.ProviderConfig
}

// DiagnosticCode classifies non-fatal discovery diagnostics.
type DiagnosticCode string

const (
	DiagnosticSkill     DiagnosticCode = "skill"
	DiagnosticCollision DiagnosticCode = "collision"
	DiagnosticImport    DiagnosticCode = "import"
	DiagnosticTruncated DiagnosticCode = "truncated"
	// DiagnosticAgent is an agent file skipped as invalid or shadowed.
	DiagnosticAgent DiagnosticCode = "agent"
)

// Diagnostic is a non-fatal source warning.
type Diagnostic struct {
	Code      DiagnosticCode
	Path      string
	Message   string
	Collision *skills.Collision
}

// SessionInputs are the concrete session values produced at startup.
type SessionInputs struct {
	RepoRoot     string
	ContextFiles []prompt.ContextFile
	Skills       []skills.Skill
	GenericRules []string
	Diagnostics  []Diagnostic
	// ContextBytes counts the context file bytes kept, without a cut
	// notice, for doctor's report. Nested discovery spends the cap from the
	// files' whole content instead, notice included, so a cut startup file
	// leaves it nothing.
	ContextBytes int
	// Agents are the agent files of the workspace and the agent dir, first
	// definition of a name first.
	Agents []agentfiles.Agent
}

// UserContextNames are the user-level context files, first hit wins.
var UserContextNames = []string{"AGENTS.override.md", "AGENTS.md"}

// ContextNames is the per-directory lookup order: AGENTS.override.md, then
// AGENTS.md, then the configured fallback names. Only the first hit loads.
func ContextNames(fallback []string) []string {
	names := slices.Clone(UserContextNames)
	for _, name := range fallback {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// ErrorCode classifies fatal discovery failures.
type ErrorCode string

const (
	InvalidOptions  ErrorCode = "invalid_options"
	ResolveCwd      ErrorCode = "resolve_cwd"
	ResolveAgentDir ErrorCode = "resolve_agent_dir"
	ResolveRepoRoot ErrorCode = "resolve_repo_root"
	ReadContext     ErrorCode = "read_context"
	ReadRule        ErrorCode = "read_rule"
	ExpandContext   ErrorCode = "expand_context"
	ExpandRule      ErrorCode = "expand_rule"
	LoadSkills      ErrorCode = "load_skills"
	LoadAgents      ErrorCode = "load_agents"
)

// Error is a typed discovery failure.
type Error struct {
	Code ErrorCode
	Op   string
	Path string
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("discovery %s %s: %v", e.Code, e.Path, e.Err) }
func (e *Error) Unwrap() error { return e.Err }
func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && e.Code == other.Code
}

// source is where startup files come from: the root they and their imports
// are read through, and the home dir their ~ imports resolve against. A
// source whose directory does not exist has no root and no files.
type source struct {
	root *os.Root
	home string
}

func openSource(dir, home string, code ErrorCode) (source, error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, os.ErrNotExist) {
		return source{}, nil
	}
	if err != nil {
		return source{}, &Error{Code: code, Op: "open root", Path: dir, Err: err}
	}
	return source{root: root, home: home}, nil
}

func (s source) close() {
	if s.root != nil {
		s.root.Close()
	}
}

// candidate keeps a source path with its bytes so expansion errors can name
// the file that supplied them. whole is false when the read stopped at its
// limit before the end of the file.
type candidate struct {
	path    string
	content []byte
	whole   bool
	from    source
}

// loadedContext is a context file after import expansion; complete is
// false when its read or its imports were cut at the byte limit.
type loadedContext struct {
	prompt.ContextFile
	complete bool
}

// Discover collects context files, sticky rules, and canonical skills once at startup.
func Discover(ctx context.Context, options Options) (SessionInputs, error) {
	if strings.TrimSpace(options.Cwd) == "" || strings.TrimSpace(options.AgentDir) == "" {
		return SessionInputs{}, &Error{Code: InvalidOptions, Op: "options", Err: errors.New("cwd and agent dir are required")}
	}
	cwd, err := canonicalDir(options.Cwd)
	if err != nil {
		return SessionInputs{}, &Error{Code: ResolveCwd, Op: "canonicalize", Path: options.Cwd, Err: err}
	}
	agentDir, err := canonicalDir(options.AgentDir)
	if err != nil {
		return SessionInputs{}, &Error{Code: ResolveAgentDir, Op: "canonicalize", Path: options.AgentDir, Err: err}
	}
	home := options.HomeDir
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home != "" {
		home, _ = canonicalDir(home)
	}
	repoRoot, ancestors, err := resolveRoot(cwd, options.RepoRoot, home)
	if err != nil {
		code := ResolveRepoRoot
		if errors.Is(err, errOutsideRoot) {
			code = InvalidOptions
		}
		return SessionInputs{}, &Error{Code: code, Op: "resolve root", Path: options.RepoRoot, Err: err}
	}
	inputs := SessionInputs{RepoRoot: repoRoot}
	files := options.Files.WithDefaults()
	names := ContextNames(files.Fallback)
	// Every startup file and its imports are read through the root of the
	// place it comes from. Repository files: the repository root, with ~
	// imports left literal, so a repository file never pulls in a file from
	// outside the repository. The user's own files in the agent dir: the
	// home directory, or the agent dir itself when it is outside home, so
	// they may import ~/ files.
	repo, err := openSource(repoRoot, "", ResolveRepoRoot)
	if err != nil {
		return SessionInputs{}, err
	}
	defer repo.close()
	userRootDir := agentDir
	if home != "" && within(home, agentDir) {
		userRootDir = home
	}
	user, err := openSource(userRootDir, home, ResolveAgentDir)
	if err != nil {
		return SessionInputs{}, err
	}
	defer user.close()

	userContext, err := firstCandidate(user, agentDir, UserContextNames, files.MaxBytes)
	if err != nil {
		return SessionInputs{}, err
	}
	userRule, err := readCandidate(user, filepath.Join(agentDir, "RULES.md"), ReadRule, -1)
	if err != nil {
		return SessionInputs{}, err
	}
	var projectContexts []candidate
	var nativeRule *candidate
	nativeContextSelected := false
	for _, ancestor := range ancestors {
		if err := ctx.Err(); err != nil {
			return SessionInputs{}, err
		}
		nativeContext, err := readCandidate(repo, filepath.Join(ancestor, config.ProjectConfigDirName, "AGENTS.md"), ReadContext, files.MaxBytes)
		if err != nil {
			return SessionInputs{}, err
		}
		var standalone *candidate
		if !strings.HasPrefix(filepath.Base(ancestor), ".") {
			standalone, err = firstCandidate(repo, ancestor, names, files.MaxBytes)
		}
		if err != nil {
			return SessionInputs{}, err
		}
		selectsNative := nativeContext != nil && !nativeContextSelected
		if selectsNative {
			projectContexts = append(projectContexts, *nativeContext)
			nativeContextSelected = true
		} else if standalone != nil {
			projectContexts = append(projectContexts, *standalone)
		}
		if nativeRule == nil {
			rule, err := readCandidate(repo, filepath.Join(ancestor, config.ProjectConfigDirName, "RULES.md"), ReadRule, -1)
			if err != nil {
				return SessionInputs{}, err
			}
			if rule != nil {
				nativeRule = rule
			}
		}
	}
	var contexts []loadedContext
	if userContext != nil {
		item, err := expandContext(ctx, *userContext, files.MaxBytes)
		if err != nil {
			return SessionInputs{}, err
		}
		contexts = append(contexts, item)
	}
	// ancestors are cwd-first. Reversing yields the required root-first order.
	seenPaths := map[string]struct{}{}
	project := make([]loadedContext, 0, len(projectContexts))
	for index := len(projectContexts) - 1; index >= 0; index-- {
		real := canonicalPath(projectContexts[index].path)
		if _, exists := seenPaths[real]; exists {
			continue
		}
		seenPaths[real] = struct{}{}
		item, err := expandContext(ctx, projectContexts[index], files.MaxBytes)
		if err != nil {
			return SessionInputs{}, err
		}
		project = append(project, item)
	}
	// Identical files collapse to the nearest one. Only files read whole
	// compare: two cut files may share the bytes read and differ after them.
	lastContent := make(map[string]int, len(project))
	for index, item := range project {
		if item.complete {
			lastContent[item.Content] = index
		}
	}
	for index, item := range project {
		if !item.complete || lastContent[item.Content] == index {
			contexts = append(contexts, item)
		}
	}
	var notice *Diagnostic
	inputs.ContextFiles, inputs.ContextBytes, notice = capContextFiles(contexts, files.MaxBytes)
	if notice != nil {
		inputs.Diagnostics = append(inputs.Diagnostics, *notice)
	}
	for _, rule := range []*candidate{userRule, nativeRule} {
		if rule == nil {
			continue
		}
		body, err := expandRule(ctx, *rule)
		if err != nil {
			return SessionInputs{}, err
		}
		if strings.TrimSpace(body) != "" {
			inputs.GenericRules = append(inputs.GenericRules, body)
		}
	}
	loaded, err := skills.LoadSkills(ctx, skills.LoadOptions{Cwd: cwd, AgentDir: agentDir, SkillPaths: append([]string(nil), options.SkillPaths...), IncludeDefaults: options.IncludeDefaultSkills})
	if err != nil {
		return SessionInputs{}, &Error{Code: LoadSkills, Op: "load skills", Err: err}
	}
	inputs.Skills = loaded.Skills
	for _, diagnostic := range loaded.Diagnostics {
		code := DiagnosticSkill
		if diagnostic.Collision != nil || diagnostic.Type == "collision" {
			code = DiagnosticCollision
		}
		inputs.Diagnostics = append(inputs.Diagnostics, Diagnostic{Code: code, Path: diagnostic.Path, Message: diagnostic.Message, Collision: diagnostic.Collision})
	}
	projectDir := repoRoot
	if options.RepoRoot == "" {
		projectDir = projectDirOf(cwd)
	}
	agents, agentDiagnostics, err := agentfiles.Load(ctx, agentfiles.Options{ProjectDir: projectDir, AgentDir: agentDir, Providers: options.Providers})
	if err != nil {
		return SessionInputs{}, &Error{Code: LoadAgents, Op: "load agents", Err: err}
	}
	inputs.Agents = agents
	for _, diagnostic := range agentDiagnostics {
		inputs.Diagnostics = append(inputs.Diagnostics, Diagnostic{Code: DiagnosticAgent, Path: diagnostic.Path, Message: diagnostic.Message})
	}
	return inputs, nil
}

// readCandidate treats missing and blank optional sources as absent, keeping
// discovery fallback selection simple. The file is opened through its
// source's root, and one that leaves it through a symlink is skipped: a
// repository file may not pull in a file from outside the repository. A
// file that is not regular fails without being opened, so a FIFO cannot
// block startup. At most limit bytes are read, plus one to tell a cut file
// (negative reads the whole file); a cut file counts as nonblank.
func readCandidate(from source, path string, code ErrorCode, limit int) (*candidate, error) {
	if from.root == nil {
		return nil, nil
	}
	name, err := filepath.Rel(from.root.Name(), path)
	if err != nil || !filepath.IsLocal(name) {
		return nil, &Error{Code: code, Op: "read", Path: path, Err: errors.New("outside its root " + from.root.Name())}
	}
	info, err := from.root.Stat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		if escapesRoot(from.root.Name(), path) {
			slog.Warn("context file skipped: it links outside its root", "path", path, "root", from.root.Name())
			return nil, nil
		}
		return nil, &Error{Code: code, Op: "read", Path: path, Err: err}
	}
	if !info.Mode().IsRegular() {
		return nil, &Error{Code: code, Op: "read", Path: path, Err: errors.New("not a regular file")}
	}
	file, err := from.root.Open(name)
	if err != nil {
		return nil, &Error{Code: code, Op: "read", Path: path, Err: err}
	}
	defer file.Close()
	var reader io.Reader = file
	if limit >= 0 {
		reader = io.LimitReader(file, int64(limit)+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, &Error{Code: code, Op: "read", Path: path, Err: err}
	}
	whole := limit < 0 || int64(len(data)) == info.Size()
	if whole && strings.TrimSpace(string(data)) == "" {
		return nil, nil
	}
	return &candidate{path: path, content: data, whole: whole, from: from}, nil
}

// escapesRoot reports whether path resolves outside dir.
func escapesRoot(dir, path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && !within(canonicalPath(dir), resolved)
}

// firstCandidate returns the first nonblank file of names in dir, so
// AGENTS.override.md shadows AGENTS.md and both shadow fallback names.
func firstCandidate(from source, dir string, names []string, limit int) (*candidate, error) {
	for _, name := range names {
		found, err := readCandidate(from, filepath.Join(dir, name), ReadContext, limit)
		if err != nil || found != nil {
			return found, err
		}
	}
	return nil, nil
}

// capContextFiles keeps files in prompt order until maxBytes is spent. The
// file that crosses the cap, or whose read or imports were already cut at
// it, is cut at a UTF-8 boundary and carries a notice naming the cap and
// every file left out, so the model knows the context is incomplete. It
// returns the kept files, the bytes they spend (without the notice) and a
// diagnostic when anything was cut.
func capContextFiles(files []loadedContext, maxBytes int) ([]prompt.ContextFile, int, *Diagnostic) {
	used := 0
	kept := make([]prompt.ContextFile, 0, len(files))
	for index, file := range files {
		if file.complete && used+len(file.Content) <= maxBytes {
			used += len(file.Content)
			kept = append(kept, file.ContextFile)
			continue
		}
		content := TruncateUTF8(file.Content, maxBytes-used)
		omitted := make([]string, 0, len(files)-index-1)
		for _, later := range files[index+1:] {
			omitted = append(omitted, later.Path)
		}
		message := fmt.Sprintf("context files exceed context_files.max_bytes (%d bytes): %s was cut after %d bytes", maxBytes, file.Path, len(content))
		if len(omitted) > 0 {
			message += "; omitted: " + strings.Join(omitted, ", ")
		}
		file.Content = content + "\n\n[harness: " + message + "]"
		return append(kept, file.ContextFile), used + len(content), &Diagnostic{Code: DiagnosticTruncated, Path: file.Path, Message: message}
	}
	return kept, used, nil
}

// TruncateUTF8 returns the longest prefix of s within limit bytes that does
// not split a UTF-8 sequence.
func TruncateUTF8(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// expandContext expands a context file's imports through its source's
// root, reading at most maxBytes of imports.
func expandContext(ctx context.Context, item candidate, maxBytes int) (loadedContext, error) {
	content, complete, err := contextfile.Expand(ctx, item.from.root, item.path, item.content, contextfile.ExpandOptions{HomeDir: item.from.home, MaxBytes: maxBytes})
	if err != nil {
		return loadedContext{}, &Error{Code: ExpandContext, Op: "expand", Path: item.path, Err: err}
	}
	return loadedContext{ContextFile: prompt.ContextFile{Path: filepath.ToSlash(item.path), Content: content}, complete: complete && item.whole}, nil
}

// expandRule expands a RULES.md file's imports through its source's root.
// Rules have no byte cap.
func expandRule(ctx context.Context, item candidate) (string, error) {
	content, _, err := contextfile.Expand(ctx, item.from.root, item.path, item.content, contextfile.ExpandOptions{HomeDir: item.from.home, MaxBytes: -1})
	if err != nil {
		return "", &Error{Code: ExpandRule, Op: "expand", Path: item.path, Err: err}
	}
	return content, nil
}

var errOutsideRoot = errors.New("cwd is outside repo root")

// resolveRoot picks the boundary for ancestor discovery: an explicit root wins, then Git, home, or the filesystem root.
func resolveRoot(cwd, suppliedRoot, home string) (string, []string, error) {
	boundary := ""
	if suppliedRoot != "" {
		root, err := canonicalDir(suppliedRoot)
		if err != nil {
			return "", nil, err
		}
		if !within(root, cwd) {
			return "", nil, errOutsideRoot
		}
		boundary = root
	} else {
		boundary = gitRoot(cwd)
		if boundary == "" && home != "" && within(home, cwd) {
			boundary = home
		}
		if boundary == "" {
			for boundary = cwd; filepath.Dir(boundary) != boundary; boundary = filepath.Dir(boundary) {
			}
		}
	}
	ancestors := make([]string, 0)
	for dir := cwd; ; dir = filepath.Dir(dir) {
		ancestors = append(ancestors, dir)
		if dir == boundary {
			break
		}
	}
	return boundary, ancestors, nil
}

// gitRoot is the nearest directory at or above dir that holds .git, or "".
func gitRoot(dir string) string {
	for ; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		if filepath.Dir(dir) == dir {
			return ""
		}
	}
}

// projectDirOf is where a session in cwd finds its repository's agent
// files: the git work tree's root, or cwd outside git.
func projectDirOf(cwd string) string {
	if root := gitRoot(cwd); root != "" {
		return root
	}
	return cwd
}

// ProjectDir is projectDirOf for a working directory given as the user
// wrote it.
func ProjectDir(cwd string) (string, error) {
	dir, err := canonicalDir(cwd)
	if err != nil {
		return "", err
	}
	return projectDirOf(dir), nil
}

// canonicalDir resolves symlinks when possible but preserves a clean absolute path for an absent directory.
func canonicalDir(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

// canonicalPath gives duplicate checks a stable identity while tolerating paths that can't be resolved.
func canonicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
