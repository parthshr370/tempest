// Gitignore matching for the pure-Go walks: the grep, find and AST fallbacks
// and the session-start listing outside a git work tree. It follows git's
// rules for the common cases: blank lines and `#` comments are skipped, `\#`
// and `\!` escape a leading character, unescaped trailing spaces are
// trimmed, a trailing `/` matches directories only, a `/` at the start or in
// the middle anchors the pattern to the directory of its .gitignore, `*`,
// `?` and `[...]` match within one path segment, `**` matches any number of
// segments, and `!` re-includes a path an earlier pattern excluded. The last
// matching pattern decides, a deeper .gitignore outranks the ones above it,
// and nothing under an excluded directory is re-included. Global excludes
// (core.excludesFile) and .git/info/exclude are not read.
package tools

import (
	"io"
	"os"
	"path"
	"strings"
	"syscall"
)

// gitignoreMaxBytes bounds one ignore file read.
const gitignoreMaxBytes = 1 << 20

// loadGitignore reads the root .gitignore when present.
func loadGitignore(root *os.Root) *gitignoreMatcher {
	return readGitignore(root, ".gitignore")
}

// readGitignore parses the ignore file at the root-relative name, or returns
// nil when there is none to apply. Only a regular file is opened, and without
// blocking, so a FIFO or device named .gitignore can't stall a walk; a
// symbolic link is not followed, as git doesn't follow one in the work tree.
// A file past gitignoreMaxBytes is skipped whole, as git skips one past its
// own limit. An unreadable file is likewise absent: the walk lists what it
// can rather than fail the search or listing over an ignore file.
func readGitignore(root *os.Root, name string) *gitignoreMatcher {
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > gitignoreMaxBytes {
		return nil
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	defer file.Close()
	// The name may have been replaced after Lstat.
	if info, err = file.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(file, gitignoreMaxBytes+1))
	if err != nil || len(data) > gitignoreMaxBytes {
		return nil
	}
	return parseGitignore(string(data))
}

// gitignoreRule is one parsed .gitignore line.
type gitignoreRule struct {
	segments []string
	dirOnly  bool
	anchored bool
	negated  bool
}

// gitignoreMatcher holds the rules of one .gitignore file. Paths given to it
// are slash-separated and relative to the directory holding that file.
type gitignoreMatcher struct {
	rules []gitignoreRule
}

// parseGitignore builds a matcher from .gitignore content.
func parseGitignore(content string) *gitignoreMatcher {
	matcher := &gitignoreMatcher{}
	for _, line := range strings.Split(content, "\n") {
		line = trimGitignoreSpaces(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rule := gitignoreRule{}
		if strings.HasPrefix(line, "!") {
			rule.negated = true
			line = line[1:]
		} else if strings.HasPrefix(line, `\!`) || strings.HasPrefix(line, `\#`) {
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			rule.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		rule.anchored = strings.Contains(line, "/")
		line = strings.TrimPrefix(line, "/")
		if line == "" {
			continue
		}
		rule.segments = strings.Split(line, "/")
		matcher.rules = append(matcher.rules, rule)
	}
	return matcher
}

// trimGitignoreSpaces drops trailing spaces unless a backslash escapes them.
func trimGitignoreSpaces(line string) string {
	trimmed := strings.TrimRight(line, " ")
	if len(trimmed) < len(line) && strings.HasSuffix(trimmed, `\`) {
		return trimmed[:len(trimmed)-1] + " "
	}
	return trimmed
}

// decide reports whether the last rule matching the relative path excludes
// it, and whether any rule matched at all.
func (m *gitignoreMatcher) decide(segments []string, isDir bool) (ignored bool, matched bool) {
	for index := len(m.rules) - 1; index >= 0; index-- {
		rule := m.rules[index]
		if rule.dirOnly && !isDir {
			continue
		}
		if rule.matches(segments) {
			return !rule.negated, true
		}
	}
	return false, false
}

// matches applies the rule to a path relative to its .gitignore directory. An
// unanchored pattern is one segment and matches the basename at any depth.
func (rule gitignoreRule) matches(segments []string) bool {
	if !rule.anchored {
		ok, err := path.Match(rule.segments[0], segments[len(segments)-1])
		return err == nil && ok
	}
	return matchSegments(rule.segments, segments)
}

// Ignore reports whether the slash path relative to the matcher's directory
// is ignored, itself or through an excluded parent directory.
func (m *gitignoreMatcher) Ignore(name string, isDir bool) bool {
	if m == nil || name == "" || name == "." {
		return false
	}
	segments := strings.Split(name, "/")
	for end := 1; end <= len(segments); end++ {
		if ignored, _ := m.decide(segments[:end], end < len(segments) || isDir); ignored {
			return true
		}
	}
	return false
}

// gitignoreTree holds the .gitignore files of a walked tree by the
// root-relative directory that contains each, "." for the root.
type gitignoreTree map[string]*gitignoreMatcher

// excluded reports whether the root-relative path is ignored by the deepest
// .gitignore with a matching rule. Callers walk top-down and never descend
// into an excluded directory, so parents are not checked again.
func (tree gitignoreTree) excluded(name string, isDir bool) bool {
	segments := strings.Split(name, "/")
	for depth := len(segments) - 1; depth >= 0; depth-- {
		directory := "."
		if depth > 0 {
			directory = strings.Join(segments[:depth], "/")
		}
		matcher := tree[directory]
		if matcher == nil {
			continue
		}
		if ignored, matched := matcher.decide(segments[depth:], isDir); matched {
			return ignored
		}
	}
	return false
}

// matchSegments matches a pattern split on `/` against a path split on `/`,
// with `**` matching zero or more segments.
func matchSegments(pattern []string, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			if len(pattern) == 1 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegments(pattern[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pattern[0], name[0]); err != nil || !ok {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}

// matchGlobPattern applies a user-supplied glob to a search-relative path the
// way rg and fd do: a glob without `/` matches basenames at any depth, a glob
// with `/` matches the whole relative path.
func matchGlobPattern(glob string, name string) bool {
	if glob == "" {
		return true
	}
	if !strings.Contains(glob, "/") {
		ok, err := path.Match(glob, path.Base(name))
		return err == nil && ok
	}
	return matchSegments(strings.Split(glob, "/"), strings.Split(name, "/"))
}
