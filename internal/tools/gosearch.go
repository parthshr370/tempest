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
	var ignore *gitignoreMatcher
	if insideGitRepo(root.Name()) {
		ignore = loadGitignore(root)
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
			if entry.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		name := searchRelative(base, p)
		if ignore.Ignore(name, false) {
			return nil
		}
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
	ignore := loadGitignore(root)
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
		if ignore.Ignore(name, entry.IsDir()) {
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

// loadGitignore reads the root .gitignore when present.
func loadGitignore(root *os.Root) *gitignoreMatcher {
	data, err := root.ReadFile(".gitignore")
	if err != nil {
		return nil
	}
	return parseGitignore(string(data))
}
