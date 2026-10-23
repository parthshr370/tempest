package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.harness.dev/harness/internal/refs"
)

// maxGrepPaths bounds one grep's search paths; its match limit bounds the output.
const maxGrepPaths = 20

// grepSearchPaths resolves grep's path or paths to root-relative search
// paths, each of which must exist. With neither, it searches the working
// directory. Internal URLs are searched one per call through path.
func grepSearchPaths(cwd string, root *os.Root, input grepToolInput) ([]string, error) {
	requested := input.Paths
	switch {
	case input.Path != "" && len(requested) > 0:
		return nil, errors.New("pass path or paths, not both")
	case len(requested) > maxGrepPaths:
		return nil, fmt.Errorf("paths has %d entries; search at most %d per call", len(requested), maxGrepPaths)
	case len(requested) == 0:
		requested = []string{defaultString(input.Path, ".")}
	}
	relSearches := make([]string, 0, len(requested))
	for _, path := range requested {
		if refs.IsURL(path) {
			return nil, fmt.Errorf("%s is an internal URL; search it with path, one URL per call", path)
		}
		relSearch, err := resolveRootPath(cwd, path)
		if err != nil {
			return nil, err
		}
		if _, err := statInRoot(root, relSearch); err != nil {
			return nil, fmt.Errorf("Path not found: %s", filepath.Join(cwd, relSearch))
		}
		relSearches = append(relSearches, relSearch)
	}
	return relSearches, nil
}

// uniqueGrepMatches drops repeats that overlapping search paths (a directory
// and a file inside it) report twice, keeping first-seen order.
func uniqueGrepMatches(matches []grepMatch) []grepMatch {
	type location struct {
		file string
		line int
	}
	seen := make(map[location]bool, len(matches))
	unique := matches[:0]
	for _, match := range matches {
		key := location{match.filePath, match.lineNumber}
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, match)
	}
	return unique
}
