// Pure-Go fallbacks for the grep and find tools, used when rg or fd is not on
// PATH. They walk the tool's os.Root, honour .gitignore, skip .git, and
// format results through the same helpers as the rg/fd paths so the model
// cannot tell which backend ran.
package tools

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// grepMatch is one content match, shared by the rg and pure-Go backends so
// formatting below the search is identical either way.
type grepMatch struct {
	filePath   string
	lineNumber int
	lineText   string
}

// binarySniffBytes is how much of a file the grep fallback reads before
// deciding it is binary. This mirrors the fast backend's behaviour closely
// enough that the same files are skipped.
const binarySniffBytes = 8 * 1024

// goGrep finds matches for pattern under the root-relative base directory
// and returns them with absolute file paths, matching the rg path's match
// shape. limitReached reports that the match limit was hit.
func goGrep(ctx context.Context, root *os.Root, base string, pattern string, glob string, ignoreCase bool, literal bool, limit int) ([]grepMatch, bool, error) {
	if literal {
		pattern = regexp.QuoteMeta(pattern)
	}
	if ignoreCase {
		pattern = "(?i)" + pattern
	}
	regex, err := regexp.Compile(pattern)
	if err != nil {
		return nil, false, err
	}
	// rg only honours .gitignore inside a Git worktree; mirror that.
	ignore := newIgnoreSet(ctx)
	if insideGitRepo(root.Name()) {
		ignore = rootGitignore(ctx, root)
	}
	matches := []grepMatch{}
	limitReached := false
	walkErr := fs.WalkDir(root.FS(), base, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if p != base && (entry.Name() == ".git" || ignore.excluded(p, true)) {
				return fs.SkipDir
			}
			return nil
		}
		if p != base && ignore.excluded(p, false) {
			return nil
		}
		name := searchRelative(base, p)
		if !matchGlobPattern(glob, name) {
			return nil
		}
		data, err := readFileInRoot(root, p)
		if err != nil {
			return nil // unreadable files are skipped, as rg skips them
		}
		sniff := data
		if len(sniff) > binarySniffBytes {
			sniff = sniff[:binarySniffBytes]
		}
		if bytes.IndexByte(sniff, 0) >= 0 {
			return nil
		}
		for lineNumber, line := range splitFileLines(string(data)) {
			if regex.MatchString(line) {
				matches = append(matches, grepMatch{
					filePath:   filepath.Join(root.Name(), p),
					lineNumber: lineNumber + 1,
					lineText:   line,
				})
				if len(matches) >= limit {
					limitReached = true
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	if errors.Is(walkErr, fs.SkipAll) {
		return matches, true, nil
	}
	return matches, limitReached, walkErr
}

// goFind walks the root looking for paths matching the find pattern and
// returns absolute paths in the same shape the fd fast path prints.
func goFind(ctx context.Context, root *os.Root, base string, pattern string, limit int) ([]string, bool, error) {
	effective := pattern
	fullPath := strings.Contains(pattern, "/")
	if fullPath && !strings.HasPrefix(pattern, "/") && !strings.HasPrefix(pattern, "**/") && pattern != "**" {
		effective = "**/" + pattern
	}
	// fd always honours .gitignore here: the fast path passes
	// --no-require-git outside git repos, so ignore files apply either way.
	ignore := rootGitignore(ctx, root)
	results := []string{}
	limitReached := false
	walkErr := fs.WalkDir(root.FS(), base, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" && p != base {
				return fs.SkipDir
			}
			if p == base {
				return nil
			}
		}
		name := searchRelative(base, p)
		if p != base && ignore.excluded(p, entry.IsDir()) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		matched := false
		if fullPath {
			matched = matchSegments(strings.Split(effective, "/"), strings.Split(name, "/"))
		} else if ok, err := path.Match(effective, path.Base(name)); err == nil {
			matched = ok
		}
		if !matched {
			return nil
		}
		results = append(results, filepath.Join(root.Name(), p))
		if len(results) >= limit {
			limitReached = true
			return fs.SkipAll
		}
		return nil
	})
	if errors.Is(walkErr, fs.SkipAll) {
		return results, true, nil
	}
	return results, limitReached, walkErr
}

// searchRelative converts a walk path (relative to the root) into a path
// relative to the search base, mirroring what rg and fd print.
func searchRelative(base, p string) string {
	if base == "." || base == "" {
		return p
	}
	if p == base {
		return "."
	}
	return strings.TrimPrefix(strings.TrimPrefix(p, base), "/")
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

// matchSegments matches a glob split on `/` against a path split on `/`,
// with `**` matching zero or more segments.
func matchSegments(pattern []string, name []string) bool {
	matcher := segmentMatcher{pattern: pattern, name: name}
	return matcher.match(0, 0)
}

// segmentMatcher is one matchSegments call. Each `**` tries every split of
// the remaining path, so repeated `**` would retry the same (pattern, path)
// offsets exponentially often; as in wildmatch, results are memoised once a
// match has recursed wildmatchMemoAfter times. Ordinary globs never get there.
type segmentMatcher struct {
	pattern []string
	name    []string
	calls   int
	memo    map[int]bool
}

// match matches pattern[p:] against name[n:].
func (m *segmentMatcher) match(p int, n int) bool {
	for p < len(m.pattern) {
		if m.pattern[p] == "**" {
			if p == len(m.pattern)-1 {
				return true
			}
			for split := n; split <= len(m.name); split++ {
				if m.recurse(p+1, split) {
					return true
				}
			}
			return false
		}
		if n == len(m.name) {
			return false
		}
		if ok, err := path.Match(m.pattern[p], m.name[n]); err != nil || !ok {
			return false
		}
		p, n = p+1, n+1
	}
	return n == len(m.name)
}

// recurse is a recursive match call, memoised past wildmatchMemoAfter.
func (m *segmentMatcher) recurse(p int, n int) bool {
	m.calls++
	if m.calls <= wildmatchMemoAfter {
		return m.match(p, n)
	}
	key := p*(len(m.name)+1) + n
	if result, ok := m.memo[key]; ok {
		return result
	}
	if m.memo == nil {
		m.memo = map[int]bool{}
	}
	result := m.match(p, n)
	m.memo[key] = result
	return result
}
