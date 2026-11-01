// Ignore-file matching for the pure-Go walks: the grep, find and AST
// fallbacks and the session-start listing outside a git work tree.
//
// This is a port of git v2.54.0, not a reimplementation of its rules:
// wildmatch is dowild() from wildmatch.c, and the pattern parsing and
// matching are add_patterns_from_buffer(), trim_trailing_spaces(),
// parse_path_pattern(), match_basename(), match_pathname() and
// last_matching_pattern_from_list() from dir.c. Matching is case-sensitive,
// as git's is with core.ignorecase false (WM_CASEFOLD is not ported).
//
// One addition: dowild results are memoised once a match has recursed
// wildmatchMemoAfter times. The function is pure, so results do not change,
// but git's own matcher is exponential in patterns such as many consecutive
// `**/` (each zero-length `**/` attempt ignores the abort codes), and an
// ignore file is untrusted repository content. Matching also stops when the
// walk's context ends (cancelPoll).
package tools

import (
	"context"
	"io"
	"os"
	"strings"
	"syscall"
)

// ignoreFileMaxBytes bounds one ignore file read. git skips a file past 100
// MiB (PATTERN_MAX_FILE_SIZE); a walk that must finish within a startup
// deadline skips one past 1 MiB.
const ignoreFileMaxBytes = 1 << 20

// readIgnoreFile parses the ignore file at the root-relative name, whose
// patterns apply below dir ("" for the root), or returns nil when there is
// none to apply. Only a regular file is opened, and without blocking, so a
// FIFO or device can't stall a walk; a symbolic link is not followed, as git
// opens it with O_NOFOLLOW. A file past ignoreFileMaxBytes is skipped whole.
// An unreadable file is likewise absent: the walk lists what it can rather
// than fail the search or listing over an ignore file.
func readIgnoreFile(root *os.Root, name string, dir string) *ignoreList {
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > ignoreFileMaxBytes {
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
	data, err := io.ReadAll(io.LimitReader(file, ignoreFileMaxBytes+1))
	if err != nil || len(data) > ignoreFileMaxBytes {
		return nil
	}
	return parseIgnoreList(string(data), dir)
}

// Pattern flags, git's PATTERN_FLAG_*.
const (
	patternNoDir     = 1 << iota // no slash: match the basename at any depth
	patternEndsWith              // `*literal`: a suffix compare
	patternMustBeDir             // trailing slash: directories only
	patternNegative              // leading `!`: re-include
)

// ignorePattern is git's struct path_pattern.
type ignorePattern struct {
	pattern string
	// nowildcardlen is the length of the literal prefix before the first
	// glob-special byte.
	nowildcardlen int
	flags         int
}

// ignoreList is the patterns of one ignore file, git's struct pattern_list.
type ignoreList struct {
	// base is the root-relative directory holding the file, "" for the root.
	base     string
	patterns []ignorePattern
}

// parseIgnoreList is add_patterns_from_buffer: a leading UTF-8 BOM is
// skipped, the content ends with a newline as add_patterns() appends one, and
// each line that is not empty or a `#` comment loses one `\r` before its
// newline and its unescaped trailing spaces. A NUL ends a pattern, as it does
// git's C string. Patterns left empty never match and are dropped.
func parseIgnoreList(content string, base string) *ignoreList {
	list := &ignoreList{base: base}
	content = strings.TrimPrefix(content, "\ufeff")
	for {
		line, rest, found := strings.Cut(content, "\n")
		if !found && line == "" {
			break
		}
		content = rest
		if line == "" || line[0] == '#' {
			continue
		}
		line = strings.TrimSuffix(line, "\r")
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			line = line[:nul]
		}
		if pattern, ok := parsePathPattern(trimTrailingSpaces(line)); ok {
			list.patterns = append(list.patterns, pattern)
		}
		if !found {
			break
		}
	}
	return list
}

// trimTrailingSpaces is trim_trailing_spaces: it cuts the run of trailing
// spaces that no backslash escapes. A backslash escapes the byte after it,
// so `\\ ` ends in an escaped backslash and an unescaped space.
func trimTrailingSpaces(line string) string {
	lastSpace := -1
	for index := 0; index < len(line); index++ {
		switch line[index] {
		case ' ':
			if lastSpace < 0 {
				lastSpace = index
			}
		case '\\':
			index++
			if index == len(line) {
				return line
			}
			lastSpace = -1
		default:
			lastSpace = -1
		}
	}
	if lastSpace >= 0 {
		return line[:lastSpace]
	}
	return line
}

// parsePathPattern is parse_path_pattern. ok is false for a pattern that is
// empty once its `!` and trailing `/` are removed.
func parsePathPattern(line string) (ignorePattern, bool) {
	var parsed ignorePattern
	if strings.HasPrefix(line, "!") {
		parsed.flags |= patternNegative
		line = line[1:]
	}
	length := len(line)
	if length > 0 && line[length-1] == '/' {
		length--
		parsed.flags |= patternMustBeDir
	}
	if strings.IndexByte(line[:length], '/') < 0 {
		parsed.flags |= patternNoDir
	}
	parsed.nowildcardlen = min(simpleLength(line), length)
	if line != "" && line[0] == '*' && simpleLength(line[1:]) == len(line)-1 {
		parsed.flags |= patternEndsWith
	}
	parsed.pattern = line[:length]
	return parsed, length > 0
}

// simpleLength is simple_length: the length of the prefix free of
// glob-special bytes.
func simpleLength(pattern string) int {
	for index := range len(pattern) {
		if isGlobSpecial(pattern[index]) {
			return index
		}
	}
	return len(pattern)
}

// isGlobSpecial is git's is_glob_special (sane_ctype GIT_GLOB_SPECIAL).
func isGlobSpecial(c byte) bool {
	return c == '*' || c == '?' || c == '[' || c == '\\'
}

// lastMatching is last_matching_pattern_from_list: the last pattern matching
// the root-relative pathname, or nil. It returns nil as soon as poll reports
// the walk's context ended.
func (list *ignoreList) lastMatching(poll *cancelPoll, pathname string, isDir bool) *ignorePattern {
	basename := pathname[strings.LastIndexByte(pathname, '/')+1:]
	for index := len(list.patterns) - 1; index >= 0; index-- {
		if poll.stopped() {
			return nil
		}
		pattern := &list.patterns[index]
		if pattern.flags&patternMustBeDir != 0 && !isDir {
			continue
		}
		if pattern.flags&patternNoDir != 0 {
			if matchBasename(poll, basename, pattern) {
				return pattern
			}
			continue
		}
		if matchPathname(poll, pathname, list.base, pattern) {
			return pattern
		}
	}
	return nil
}

// matchBasename is match_basename.
func matchBasename(poll *cancelPoll, basename string, pattern *ignorePattern) bool {
	switch {
	case pattern.nowildcardlen == len(pattern.pattern):
		return basename == pattern.pattern
	case pattern.flags&patternEndsWith != 0:
		return strings.HasSuffix(basename, pattern.pattern[1:])
	default:
		return wildmatch(poll, pattern.pattern, basename, false)
	}
}

// matchPathname is match_pathname: the pattern is anchored at base, the
// directory of its ignore file, and matched against the rest of the path
// with WM_PATHNAME.
func matchPathname(poll *cancelPoll, pathname string, base string, pattern *ignorePattern) bool {
	text, prefix := pattern.pattern, pattern.nowildcardlen
	if text[0] == '/' {
		text = text[1:]
		prefix--
	}
	name := pathname
	if base != "" {
		if len(pathname) < len(base)+1 || pathname[len(base)] != '/' || pathname[:len(base)] != base {
			return false
		}
		name = pathname[len(base)+1:]
	}
	if prefix > 0 {
		if prefix > len(name) || text[:prefix] != name[:prefix] {
			return false
		}
		if len(text) == prefix && len(name) == prefix {
			return true
		}
		// Keep one byte of the prefix so wildmatch sees where a path
		// component starts.
		prefix--
		text, name = text[prefix:], name[prefix:]
	}
	return wildmatch(poll, text, name, true)
}

// cancelPoll lets matching notice that the walk's context ended without
// reading the context on every step.
type cancelPoll struct {
	ctx   context.Context
	steps uint
	done  bool
}

// cancelPollInterval is how many matching steps pass between context reads.
const cancelPollInterval = 1 << 10

// stopped counts one step and reports whether the context has ended. Once
// it has, it stays stopped.
func (poll *cancelPoll) stopped() bool {
	if poll.done {
		return true
	}
	poll.steps++
	if poll.steps%cancelPollInterval == 0 && poll.ctx.Err() != nil {
		poll.done = true
	}
	return poll.done
}

// dowild results, from wildmatch.h and wildmatch.c.
const (
	wmMatch           = 0
	wmNoMatch         = 1
	wmAbortAll        = -1
	wmAbortToStarStar = -2
)

// wildmatchMemoAfter is how many recursive dowild calls one match makes
// before it memoises them. Ordinary patterns never reach it.
const wildmatchMemoAfter = 256

// wildmatcher is one wildmatch() call. pattern and text stand in for C
// strings: index len is the terminating NUL (see at).
type wildmatcher struct {
	pattern  string
	text     string
	pathname bool // WM_PATHNAME
	poll     *cancelPoll
	calls    int
	memo     map[int]int
}

// wildmatch reports whether text matches the pattern, git's wildmatch()
// with flags WM_PATHNAME when pathname is set.
func wildmatch(poll *cancelPoll, pattern string, text string, pathname bool) bool {
	matcher := wildmatcher{pattern: pattern, text: text, pathname: pathname, poll: poll}
	return matcher.dowild(0, 0) == wmMatch
}

// at returns s[index], or the C string's terminating 0 past the end.
func at(s string, index int) byte {
	if index < len(s) {
		return s[index]
	}
	return 0
}

// recurse is a recursive dowild call, memoised past wildmatchMemoAfter.
func (m *wildmatcher) recurse(p int, t int) int {
	m.calls++
	if m.calls <= wildmatchMemoAfter {
		return m.dowild(p, t)
	}
	key := p*(len(m.text)+1) + t
	if result, ok := m.memo[key]; ok {
		return result
	}
	if m.memo == nil {
		m.memo = map[int]int{}
	}
	result := m.dowild(p, t)
	m.memo[key] = result
	return result
}

// dowild matches the pattern from byte p against the text from byte t. The
// statements follow wildmatch.c's dowild() one for one, minus WM_CASEFOLD.
func (m *wildmatcher) dowild(p int, t int) int {
	pattern, text := m.pattern, m.text
	for ; p < len(pattern); t, p = t+1, p+1 {
		if m.poll.stopped() {
			return wmAbortAll
		}
		pCh := pattern[p]
		tCh := at(text, t)
		if tCh == 0 && pCh != '*' {
			return wmAbortAll
		}
		switch pCh {
		case '\\':
			// Literal match with the following byte; a trailing backslash
			// is compared as the terminating 0 and fails.
			p++
			if tCh != at(pattern, p) {
				return wmNoMatch
			}
			continue
		default:
			if tCh != pCh {
				return wmNoMatch
			}
			continue
		case '?':
			if m.pathname && tCh == '/' {
				return wmNoMatch
			}
			continue
		case '*':
			var matchSlash bool
			p++
			if at(pattern, p) == '*' {
				prevP := p
				for p++; at(pattern, p) == '*'; p++ {
				}
				if !m.pathname {
					matchSlash = true
				} else if (prevP < 2 || pattern[prevP-2] == '/') &&
					(at(pattern, p) == 0 || pattern[p] == '/' || (pattern[p] == '\\' && at(pattern, p+1) == '/')) {
					// Having matched "foo/", assume "**/" matches nothing and
					// match the rest, so foo/**/bar matches foo/bar too.
					if at(pattern, p) == '/' && m.recurse(p+1, t) == wmMatch {
						return wmMatch
					}
					matchSlash = true
				} else {
					matchSlash = false
				}
			} else {
				matchSlash = !m.pathname
			}
			if p == len(pattern) {
				// A trailing "**" matches everything, a trailing "*" only
				// text without another slash.
				if !matchSlash && strings.IndexByte(text[t:], '/') >= 0 {
					return wmAbortToStarStar
				}
				return wmMatch
			} else if !matchSlash && pattern[p] == '/' {
				// One asterisk then a slash matches up to the next slash,
				// which the loop then consumes.
				slash := strings.IndexByte(text[t:], '/')
				if slash < 0 {
					return wmAbortAll
				}
				t += slash
				continue
			}
			for tCh != 0 {
				// Before a literal, skip straight to its next occurrence;
				// without matchSlash, not past a slash.
				if !isGlobSpecial(pattern[p]) {
					pCh = pattern[p]
					for tCh = at(text, t); tCh != 0 && (matchSlash || tCh != '/'); tCh = at(text, t) {
						if tCh == pCh {
							break
						}
						t++
					}
					if tCh != pCh {
						if matchSlash {
							return wmAbortAll
						}
						return wmAbortToStarStar
					}
				}
				if matched := m.recurse(p, t); matched != wmNoMatch {
					if !matchSlash || matched != wmAbortToStarStar {
						return matched
					}
				} else if !matchSlash && tCh == '/' {
					return wmAbortToStarStar
				}
				if m.poll.stopped() {
					return wmAbortAll
				}
				t++
				tCh = at(text, t)
			}
			return wmAbortAll
		case '[':
			p++
			pCh = at(pattern, p)
			if pCh == '^' {
				pCh = '!'
			}
			negated := pCh == '!'
			if negated {
				p++
				pCh = at(pattern, p)
			}
			var prevCh byte
			matched := false
			for {
				if pCh == 0 {
					return wmAbortAll
				}
				if pCh == '\\' {
					p++
					pCh = at(pattern, p)
					if pCh == 0 {
						return wmAbortAll
					}
					if tCh == pCh {
						matched = true
					}
				} else if pCh == '-' && prevCh != 0 && at(pattern, p+1) != 0 && pattern[p+1] != ']' {
					p++
					pCh = pattern[p]
					if pCh == '\\' {
						p++
						pCh = at(pattern, p)
						if pCh == 0 {
							return wmAbortAll
						}
					}
					if tCh <= pCh && tCh >= prevCh {
						matched = true
					}
					pCh = 0 // so prevCh becomes 0
				} else if pCh == '[' && at(pattern, p+1) == ':' {
					p += 2
					start := p
					for pCh = at(pattern, p); pCh != 0 && pCh != ']'; pCh = at(pattern, p) {
						p++
					}
					if pCh == 0 {
						return wmAbortAll
					}
					length := p - start - 1
					if length < 0 || pattern[p-1] != ':' {
						// No ":]": the "[" is a literal member.
						p = start - 2
						pCh = '['
						if tCh == pCh {
							matched = true
						}
					} else {
						member, known := characterClass(pattern[start:start+length], tCh)
						if !known {
							return wmAbortAll
						}
						if member {
							matched = true
						}
						pCh = 0 // so prevCh becomes 0
					}
				} else if tCh == pCh {
					matched = true
				}
				prevCh = pCh
				p++
				pCh = at(pattern, p)
				if pCh == ']' {
					break
				}
			}
			if matched == negated || (m.pathname && tCh == '/') {
				return wmNoMatch
			}
			continue
		}
	}
	if t < len(text) {
		return wmNoMatch
	}
	return wmMatch
}

// characterClass reports whether c is in the named POSIX class, with git's
// locale-independent ASCII ctype (sane-ctype.h); known is false for a name
// git doesn't accept.
func characterClass(name string, c byte) (member bool, known bool) {
	lower := c >= 'a' && c <= 'z'
	upper := c >= 'A' && c <= 'Z'
	digit := c >= '0' && c <= '9'
	graph := c >= 0x21 && c <= 0x7e
	switch name {
	case "alnum":
		return lower || upper || digit, true
	case "alpha":
		return lower || upper, true
	case "blank":
		return c == ' ' || c == '\t', true
	case "cntrl":
		return c < 0x20 || c == 0x7f, true
	case "digit":
		return digit, true
	case "graph":
		return graph, true
	case "lower":
		return lower, true
	case "print":
		return c >= 0x20 && c <= 0x7e, true
	case "punct":
		return graph && !lower && !upper && !digit, true
	case "space":
		return c == ' ' || c == '\t' || c == '\n' || c == '\r', true
	case "upper":
		return upper, true
	case "xdigit":
		return digit || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'), true
	}
	return false, false
}

// ignoreSet is the ignore files that apply to the entry a top-down walk is
// at: the root's and those of the entry's ancestor directories, like git's
// exclude stack in prep_exclude(). Each directory may hold one file of each
// name; names are listed highest precedence first.
type ignoreSet struct {
	names []string
	stack []ignoreDirectory
	poll  cancelPoll
}

// ignoreDirectory is one directory's ignore files, one per name, nil when
// absent.
type ignoreDirectory struct {
	dir   string
	lists []*ignoreList
}

// newIgnoreSet starts an empty set for a walk under ctx. Once ctx ends every
// path reads as excluded, so the walk lists and searches nothing more.
func newIgnoreSet(ctx context.Context, names ...string) *ignoreSet {
	return &ignoreSet{names: names, poll: cancelPoll{ctx: ctx}}
}

// rootGitignore is the set the grep, find and AST fallbacks apply: the root
// .gitignore only.
func rootGitignore(ctx context.Context, root *os.Root) *ignoreSet {
	set := newIgnoreSet(ctx, ".gitignore")
	set.enter(root, "")
	return set
}

// enter makes the root-relative dir ("" for the root) the directory the walk
// is in, dropping the files of directories it has left and reading dir's
// own.
func (set *ignoreSet) enter(root *os.Root, dir string) {
	set.leaveTo(dir)
	lists := make([]*ignoreList, len(set.names))
	found := false
	for index, name := range set.names {
		file := name
		if dir != "" {
			file = dir + "/" + name
		}
		if lists[index] = readIgnoreFile(root, file, dir); lists[index] != nil {
			found = true
		}
	}
	if found {
		set.stack = append(set.stack, ignoreDirectory{dir: dir, lists: lists})
	}
}

// leaveTo drops the files of directories that are not dir or above it.
func (set *ignoreSet) leaveTo(dir string) {
	for len(set.stack) > 0 && !isAncestorOrSelf(set.stack[len(set.stack)-1].dir, dir) {
		set.stack = set.stack[:len(set.stack)-1]
	}
}

func isAncestorOrSelf(ancestor string, dir string) bool {
	return ancestor == "" || dir == ancestor || (strings.HasPrefix(dir, ancestor) && dir[len(ancestor)] == '/')
}

// excluded reports whether the root-relative path is excluded by itself,
// assuming its directories are not, which a walk that never descends into an
// excluded directory guarantees. For each name in precedence order, the
// deepest file with a matching pattern decides, as in git's
// last_matching_pattern_from_lists() and the rg ignore order.
func (set *ignoreSet) excluded(pathname string, isDir bool) bool {
	set.leaveTo(parentDirectory(pathname))
	for index := range set.names {
		for depth := len(set.stack) - 1; depth >= 0; depth-- {
			list := set.stack[depth].lists[index]
			if list == nil {
				continue
			}
			if pattern := list.lastMatching(&set.poll, pathname, isDir); pattern != nil {
				return pattern.flags&patternNegative == 0
			}
			if set.poll.done {
				return true
			}
		}
	}
	return set.poll.done
}

func parentDirectory(pathname string) string {
	if slash := strings.LastIndexByte(pathname, '/'); slash >= 0 {
		return pathname[:slash]
	}
	return ""
}
