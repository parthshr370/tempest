package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/pathutil"
	"go.harness.dev/harness/internal/refs"
)

// grepToolInput is the grep tool's arguments.
type grepToolInput struct {
	Pattern    string   `json:"pattern"`
	Path       string   `json:"path"`
	Paths      []string `json:"paths"`
	Glob       string   `json:"glob"`
	IgnoreCase bool     `json:"ignore_case"`
	Literal    bool     `json:"literal"`
	Context    int      `json:"context"`
	Limit      int      `json:"limit"`
}

// defaultGrepMatchLimit matches the grep tool's default for workspace
// searches.
const defaultGrepMatchLimit = 100

// Tool description fragments for the internal URL support.
const (
	readSelectorsHelp = "Also reads directories (entries), archives (bundle.zip, .tar, .tar.gz, .tgz list members; bundle.zip:dir/file reads one), .docx/.xlsx/.pptx (extracted text), " +
		"JSON queries (file.json?q=.items[0].name; a jq subset: .a.b, .[n], .[], ? after a step, keys, length, |) and internal URLs: skill://<name>[/<file>] (skill://<name>/ lists it), " +
		"job:// (jobs), job://<id> (record), job://<id>/<artifact> (output.log, result.txt, changes.patch), history:// and history://<id> (child transcripts), local://<path> (scratch shared with child sessions). " +
		"PDF and SQLite are not rendered; use bash. Line ranges apply to every text output."
	localWriteHelp = "A local://<path> URL targets the session tree's private scratch directory instead of the workspace, for handing large payloads between agents; other internal URLs are read-only."
	bashURLHelp    = "skill://<name>/<file> and local://<path> words expand to quoted absolute paths before the command is checked and run (percent-encode spaces in names)."
)

// grepRefs searches an internal URL: a scheme directory or file through an
// os.Root with the pure-Go matcher, or rendered text (job listings,
// transcripts) line by line. Results name files by URL and are not
// recorded in the edit snapshot store.
func grepRefs(ctx context.Context, options ToolsOptions, input grepToolInput) (agent.AgentToolResult, error) {
	target, err := options.Refs.Search(ctx, input.Path)
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	limit := input.Limit
	if limit <= 0 {
		limit = defaultGrepMatchLimit
	}
	var matches []grepMatch
	var limitReached bool
	source := grepSource{canonical: func(string) string { return "" }}
	if target.Location == nil {
		matches, limitReached, err = grepText(target.URI, target.Text, input, limit)
		source.display = func(string) string { return target.URI }
		source.read = func(string) ([]byte, error) { return []byte(target.Text), nil }
	} else {
		root, openErr := os.OpenRoot(target.Location.Root)
		if openErr != nil {
			return agent.AgentToolResult{}, fmt.Errorf("%s: not found", target.URI)
		}
		defer root.Close()
		if _, statErr := statInRoot(root, target.Location.Name); statErr != nil {
			return agent.AgentToolResult{}, fmt.Errorf("%s: not found", target.URI)
		}
		matches, limitReached, err = goGrep(ctx, root, filepath.ToSlash(target.Location.Name), input.Pattern, input.Glob, input.IgnoreCase, input.Literal, limit)
		rootRel := func(filePath string) (string, error) {
			rel, relErr := filepath.Rel(root.Name(), filePath)
			return filepath.ToSlash(rel), relErr
		}
		source.display = func(filePath string) string {
			rel, relErr := rootRel(filePath)
			if relErr != nil {
				return target.URI
			}
			return refs.Child(target.RootURI, rel)
		}
		source.read = func(filePath string) ([]byte, error) {
			rel, relErr := rootRel(filePath)
			if relErr != nil {
				return nil, relErr
			}
			return readFileInRoot(root, rel)
		}
	}
	if err != nil {
		return agent.AgentToolResult{}, err
	}
	return formatGrepOutput(matches, source, nil, max(input.Context, 0), limit, limitReached, "go"), nil
}

// grepText matches rendered text line by line under grep's pattern rules.
func grepText(uri, text string, input grepToolInput, limit int) ([]grepMatch, bool, error) {
	pattern := input.Pattern
	if input.Literal {
		pattern = regexp.QuoteMeta(pattern)
	}
	if input.IgnoreCase {
		pattern = "(?i)" + pattern
	}
	regex, err := regexp.Compile(pattern)
	if err != nil {
		return nil, false, err
	}
	var matches []grepMatch
	for index, line := range splitFileLines(text) {
		if !regex.MatchString(line) {
			continue
		}
		if len(matches) >= limit {
			return matches, true, nil
		}
		matches = append(matches, grepMatch{filePath: uri, lineNumber: index + 1, lineText: line})
	}
	return matches, false, nil
}

// mutationTarget is where write and edit change a file: relPath inside
// root, serialized on queueKey. canonicalPath is the workspace-relative
// name language servers key on, and "" for local:// files, which live
// outside the workspace.
type mutationTarget struct {
	root          *os.Root
	relPath       string
	canonicalPath string
	queueKey      string
	release       func() error
}

// diskName is the name a write replaces inside root: the symlink-resolved
// canonical path when there is one, so an atomic rename lands on the real
// file instead of replacing a link to it.
func (t mutationTarget) diskName() string {
	if t.canonicalPath != "" {
		return filepath.FromSlash(t.canonicalPath)
	}
	return t.relPath
}

// resolveMutationTarget maps a write or edit path to its root: the
// workspace for paths, the root session's local:// directory for local://
// URLs. Other internal URLs are read-only.
func resolveMutationTarget(cwd string, options ToolsOptions, requested string) (mutationTarget, error) {
	if refs.IsURL(requested) {
		location, err := options.Refs.LocalTarget(requested)
		if err != nil {
			return mutationTarget{}, err
		}
		root, err := os.OpenRoot(location.Root)
		if err != nil {
			return mutationTarget{}, fmt.Errorf("%s: %w", location.URI, err)
		}
		return mutationTarget{root: root, relPath: filepath.ToSlash(location.Name), queueKey: location.Abs(), release: root.Close}, nil
	}
	root, err := rootOnce(cwd)()
	if err != nil {
		return mutationTarget{}, err
	}
	absolutePath := pathutil.ResolveToCwd(requested, cwd)
	relPath, canonicalPath, err := rootRelPaths(cwd, absolutePath)
	if err != nil {
		return mutationTarget{}, err
	}
	return mutationTarget{root: root, relPath: relPath, canonicalPath: canonicalPath, queueKey: absolutePath, release: func() error { return nil }}, nil
}

// prepareBashArguments rewrites skill:// and local:// words in the command
// before validation and the permission gate. A word that does not resolve
// is left as written; Execute reports it.
func prepareBashArguments(resolver *refs.Resolver) func(json.RawMessage) json.RawMessage {
	return func(args json.RawMessage) json.RawMessage {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(args, &raw); err != nil {
			return args
		}
		var command string
		if err := json.Unmarshal(raw["command"], &command); err != nil {
			return args
		}
		expanded, _ := resolver.ExpandCommand(command)
		if expanded == command {
			return args
		}
		encoded, err := json.Marshal(expanded)
		if err != nil {
			return args
		}
		raw["command"] = encoded
		out, err := json.Marshal(raw)
		if err != nil {
			return args
		}
		return out
	}
}

// checkCommandURLs runs at execution. The agent loop prepared the command,
// so every resolvable URL word is already a path; a word left over could
// not resolve and fails with why. A command that would still change is
// refused, so it never runs as different text than the permission gate
// judged.
func checkCommandURLs(resolver *refs.Resolver, command string) error {
	expanded, err := resolver.ExpandCommand(command)
	if err != nil {
		var refErr *refs.Error
		if errors.As(err, &refErr) {
			return fmt.Errorf("cannot expand %s in the bash command: %s", refErr.URI, refErr.Message)
		}
		return err
	}
	if expanded != command {
		return errors.New("the bash command has internal URLs that were not expanded before the permission check; send the call again")
	}
	return nil
}
