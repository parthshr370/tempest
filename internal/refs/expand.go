package refs

import "strings"

// expandSchemes are the URL prefixes rewritten in shell commands.
var expandSchemes = []string{SchemeSkill + "://", SchemeLocal + "://"}

// ExpandCommand rewrites every skill:// and local:// URL that forms a whole
// shell word into the single-quoted absolute host path from [HostPath].
// A word is unquoted (cat skill://x/SKILL.md, $(cat local://a)) or quoted in
// full ('local://my notes.md', "skill://x/"). A URL inside larger quoted
// text ("see skill://x") or glued to other text (--f=local://a) is left as
// written. Every expandable word is attempted; a word that cannot resolve
// stays unchanged and the first such failure is returned with the
// otherwise expanded command.
func (r *Resolver) ExpandCommand(command string) (string, error) {
	if !strings.Contains(command, "://") {
		return command, nil
	}
	var out strings.Builder
	var firstErr error
	last := 0
	for _, word := range urlWords(command) {
		replacement, err := r.expandWord(word.url)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out.WriteString(command[last:word.start])
		out.WriteString(replacement)
		last = word.end
	}
	out.WriteString(command[last:])
	return out.String(), firstErr
}

func (r *Resolver) expandWord(raw string) (string, error) {
	if strings.ContainsAny(raw, "$`*?[{~") {
		return "", newError(CodeUnsupported, raw, "shell expansions inside an internal URL are not supported in bash; expand the directory (for example local://) and put globs or variables after it", nil)
	}
	hostPath, err := r.HostPath(raw)
	if err != nil {
		return "", err
	}
	return shellQuote(hostPath), nil
}

// shellQuote single-quotes p for POSIX shells.
func shellQuote(p string) string {
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// urlWord is one expandable URL: command[start:end] is replaced, url is
// the unquoted URL text.
type urlWord struct {
	start, end int
	url        string
}

// urlWords scans command with enough shell syntax to tell words apart:
// quotes, backslash escapes, $( ) and backtick substitutions (which open a
// fresh quoting context even inside double quotes).
func urlWords(command string) []urlWord {
	type frame struct {
		quote    byte // quote state outside this substitution
		backtick bool
		depth    int
	}
	var words []urlWord
	var stack []frame
	quote := byte(0)
	wordStart := true // the next byte starts a new shell word
	for i := 0; i < len(command); i++ {
		c := command[i]
		if quote == '\'' {
			if c == '\'' {
				quote = 0
			}
			continue
		}
		if c == '\\' {
			i++
			wordStart = false
			continue
		}
		if quote == 0 && wordStart {
			if word, ok := matchWord(command, i); ok {
				words = append(words, word)
				i = word.end - 1
				wordStart = false
				continue
			}
		}
		switch {
		case c == '"':
			if quote == '"' {
				quote = 0
			} else {
				quote = '"'
			}
			wordStart = false
		case c == '\'' && quote == 0:
			quote = '\''
			wordStart = false
		case c == '$' && i+1 < len(command) && command[i+1] == '(':
			stack = append(stack, frame{quote: quote, depth: 1})
			quote = 0
			i++
			wordStart = true
		case c == '`':
			if n := len(stack); n > 0 && stack[n-1].backtick {
				quote = stack[n-1].quote
				stack = stack[:n-1]
				wordStart = false
			} else {
				stack = append(stack, frame{quote: quote, backtick: true})
				quote = 0
				wordStart = true
			}
		case quote == '"':
			wordStart = false
		case c == '(':
			if n := len(stack); n > 0 && !stack[n-1].backtick {
				stack[n-1].depth++
			}
			wordStart = true
		case c == ')':
			if n := len(stack); n > 0 && !stack[n-1].backtick {
				stack[n-1].depth--
				if stack[n-1].depth == 0 {
					quote = stack[n-1].quote
					stack = stack[:n-1]
					wordStart = false
					continue
				}
			}
			wordStart = true
		default:
			wordStart = isWordBreak(c)
		}
	}
	return words
}

// matchWord recognizes an expandable URL word starting at i: bare, or
// wrapped whole in one pair of single or double quotes.
func matchWord(command string, i int) (urlWord, bool) {
	open := command[i]
	if open == '\'' || open == '"' {
		body := i + 1
		if !hasScheme(command[body:]) {
			return urlWord{}, false
		}
		closing := strings.IndexByte(command[body:], open)
		if closing < 0 {
			return urlWord{}, false
		}
		end := body + closing + 1
		inner := command[body : body+closing]
		// Whitespace means quoted prose, not a name; spaces in names are
		// percent-encoded (local://my%20notes.md).
		if strings.ContainsAny(inner, " \t\n\r") || open == '"' && strings.ContainsAny(inner, "\\$`") {
			return urlWord{}, false
		}
		if end < len(command) && !isWordBreak(command[end]) {
			return urlWord{}, false
		}
		return urlWord{start: i, end: end, url: inner}, true
	}
	if !hasScheme(command[i:]) {
		return urlWord{}, false
	}
	end := i
	for end < len(command) && !isWordBreak(command[end]) && !strings.ContainsRune(`'"\`, rune(command[end])) {
		end++
	}
	if end < len(command) && !isWordBreak(command[end]) {
		// Glued to quoted or escaped text: not a whole word.
		return urlWord{}, false
	}
	return urlWord{start: i, end: end, url: command[i:end]}, true
}

func hasScheme(s string) bool {
	for _, prefix := range expandSchemes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// isWordBreak reports bytes that end an unquoted shell word.
func isWordBreak(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', ';', '|', '&', '(', ')', '<', '>', '`':
		return true
	}
	return false
}
