// Gitignore matching for the pure-Go grep and find fallbacks. This is a
// small matcher, not a full gitignore engine: blank lines and `#` comments
// are skipped, trailing `/` marks directory-only rules, a leading `/` anchors
// the pattern to the repository root, and `*` plus `**` match through
// path.Match on path segments. Negation rules (`!`) are ignored, so a file
// re-included after a broader ignore stays ignored, matching the fast
// backends' common case.
package tools

import (
	"path"
	"strings"
)

// gitignoreRule is one parsed .gitignore line.
type gitignoreRule struct {
	segments []string
	dirOnly  bool
	anchored bool
}

// gitignoreMatcher answers whether a root-relative path is ignored.
type gitignoreMatcher struct {
	rules []gitignoreRule
}

// parseGitignore builds a matcher from .gitignore content. Rules that cannot
// be represented (negation) are dropped, matching the documented limits.
func parseGitignore(content string) *gitignoreMatcher {
	matcher := &gitignoreMatcher{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimRight(line, "\r")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "!") {
			// Negation is ignored and documented in the package comment.
			continue
		}
		rule := gitignoreRule{}
		if strings.HasSuffix(line, "/") {
			rule.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		if strings.HasPrefix(line, "/") {
			rule.anchored = true
			line = strings.TrimPrefix(line, "/")
		}
		if line == "" {
			continue
		}
		rule.segments = strings.Split(line, "/")
		matcher.rules = append(matcher.rules, rule)
	}
	return matcher
}

// Ignore reports whether the root-relative slash path is ignored. A single
// unanchored segment matches basenames at any depth, as git does.
func (m *gitignoreMatcher) Ignore(name string, isDir bool) bool {
	if m == nil || name == "" || name == "." {
		return false
	}
	segments := strings.Split(name, "/")
	base := segments[len(segments)-1]
	for _, rule := range m.rules {
		if rule.dirOnly {
			// A dir-only rule matches the directory itself or anything
			// underneath it, at any depth.
			end := len(segments)
			if !isDir {
				end--
			}
			for i := 0; i < end; i++ {
				for k := i + 1; k <= end; k++ {
					if matchSegments(rule.segments, segments[i:k]) {
						return true
					}
				}
			}
			continue
		}
		if len(rule.segments) == 1 && !rule.anchored {
			if ok, err := path.Match(rule.segments[0], base); err == nil && ok {
				return true
			}
			continue
		}
		if !rule.anchored {
			// An unanchored multi-segment pattern matches at any depth.
			for i := range segments {
				if matchSegments(rule.segments, segments[i:]) {
					return true
				}
			}
			continue
		}
		if matchSegments(rule.segments, segments) {
			return true
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
