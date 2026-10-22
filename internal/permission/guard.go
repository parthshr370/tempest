package permission

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

var inlineDestructivePattern = regexp.MustCompile(`(?i)(\brmtree\s*\(|\b(?:os\.)?remove\s*\(|\bunlink(?:Sync)?\b|\brmdir(?:Sync)?\b|\b(?:fs\.|require\(['"]fs['"]\)\.)(?:rm|rmSync|writeFile|writeFileSync|truncate|truncateSync)\s*\(|\bopen\s*\([^\n]*[,=]\s*['"]w[b+t]*['"]|\b(?:write_text|write_bytes)\s*\(|\b(?:File|Dir)\.(?:delete|write|binwrite)\s*\()`)

// shellToken keeps quoted operators out of the shell syntax we inspect.
type shellToken struct {
	text     string
	operator bool
	dynamic  bool
}

// IsDestructiveCommand reports whether command needs destructive-operation approval.
// Relative paths use the process directory; session gates supply their own cwd.
func IsDestructiveCommand(command string) bool {
	return isDestructiveCommand(command, "")
}

func isDestructiveCommand(command, cwd string) bool {
	if mkfsCommandPattern.MatchString(command) {
		return true
	}
	for _, pattern := range destructivePatterns {
		if pattern.MatchString(command) {
			return true
		}
	}
	words := shellWords(command)
	written := make(map[string]bool)
	var invocation []shellToken
	for i := 0; i < len(words); i++ {
		token := words[i]
		if token.operator && strings.Contains(token.text, ">") {
			if i+1 >= len(words) || words[i+1].operator {
				return true
			}
			i++
			target := words[i]
			if strings.HasSuffix(token.text, ">&") && (target.text == "-" || allDigits(target.text)) {
				continue
			}
			if strings.Contains(token.text, ">>") {
				if !target.dynamic {
					written[resolveTarget(target.text, cwd)] = true
				}
			} else if targetNeedsApproval(target, cwd, written) {
				return true
			}
			continue
		}
		if token.operator {
			if token.text == "<" && i+1 < len(words) {
				i++
				continue
			}
			if destructiveInvocation(invocation, cwd, written) {
				return true
			}
			cwd = directoryAfter(invocation, cwd)
			invocation = nil
			continue
		}
		invocation = append(invocation, token)
	}
	return destructiveInvocation(invocation, cwd, written)
}

func destructiveInvocation(words []shellToken, cwd string, written map[string]bool) bool {
	for len(words) > 0 && strings.Contains(words[0].text, "=") {
		words = words[1:]
	}
	if len(words) == 0 {
		return false
	}
	name := filepath.Base(words[0].text)
	args := words[1:]
	switch name {
	case "rm", "rmdir", "unlink", "shred", "truncate":
		return true
	case "sed", "perl":
		for _, arg := range args {
			if strings.HasPrefix(arg.text, "--in-place") || strings.HasPrefix(arg.text, "-") && !strings.HasPrefix(arg.text, "--") && strings.Contains(arg.text, "i") {
				return true
			}
		}
	case "find":
		for i, arg := range args {
			if arg.text == "-delete" {
				return true
			}
			if arg.text == "-exec" || arg.text == "-execdir" || arg.text == "-ok" || arg.text == "-okdir" {
				if destructiveInvocation(args[i+1:], cwd, written) {
					return true
				}
			}
		}
	case "xargs", "env", "command", "exec":
		for i := range args {
			if destructiveInvocation(args[i:], cwd, written) {
				return true
			}
		}
	case "dd":
		for _, arg := range args {
			if strings.HasPrefix(arg.text, "of=") {
				return true
			}
		}
	case "tee":
		for _, arg := range args {
			if arg.text == "--append" || strings.HasPrefix(arg.text, "-") && !strings.HasPrefix(arg.text, "--") && strings.Contains(arg.text, "a") {
				return false
			}
		}
		for _, arg := range args {
			if !strings.HasPrefix(arg.text, "-") && targetNeedsApproval(arg, cwd, written) {
				return true
			}
		}
	case "cp", "mv":
		return copyNeedsApproval(args, cwd, written)
	case "git":
		return destructiveGit(args)
	}
	if name == "python" || name == "python3" || name == "node" || name == "perl" || name == "ruby" {
		for i, arg := range args {
			if (arg.text == "-c" || arg.text == "-e" || arg.text == "--eval" || arg.text == "-pe" || arg.text == "-ne") && i+1 < len(args) && inlineDestructivePattern.MatchString(args[i+1].text) {
				return true
			}
		}
	}
	return false
}

func copyNeedsApproval(args []shellToken, cwd string, written map[string]bool) bool {
	var paths []shellToken
	var destination shellToken
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if (arg.text == "-t" || arg.text == "--target-directory") && i+1 < len(args) {
			i++
			destination = args[i]
		} else if strings.HasPrefix(arg.text, "--target-directory=") {
			destination = shellToken{text: strings.TrimPrefix(arg.text, "--target-directory="), dynamic: arg.dynamic}
		} else if arg.text == "--" {
			paths = append(paths, args[i+1:]...)
			break
		} else if !strings.HasPrefix(arg.text, "-") {
			paths = append(paths, arg)
		}
	}
	if destination.text == "" {
		if len(paths) < 2 {
			return false
		}
		destination = paths[len(paths)-1]
		paths = paths[:len(paths)-1]
	}
	if destination.dynamic {
		return true
	}
	info, err := os.Stat(resolveTarget(destination.text, cwd))
	if err == nil && info.IsDir() {
		for _, source := range paths {
			if source.dynamic || targetNeedsApproval(shellToken{text: filepath.Join(destination.text, filepath.Base(source.text))}, cwd, written) {
				return true
			}
		}
		return false
	}
	return targetNeedsApproval(destination, cwd, written)
}

func destructiveGit(args []shellToken) bool {
	for len(args) > 0 && strings.HasPrefix(args[0].text, "-") {
		option := args[0].text
		args = args[1:]
		if (option == "-C" || option == "-c" || option == "--git-dir" || option == "--work-tree") && len(args) > 0 {
			args = args[1:]
		}
	}
	if len(args) == 0 {
		return false
	}
	subcommand := args[0].text
	for _, arg := range args[1:] {
		switch subcommand {
		case "reset":
			if arg.text == "--hard" || arg.text == "--merge" || arg.text == "--keep" {
				return true
			}
		case "clean", "push", "checkout":
			if strings.HasPrefix(arg.text, "--force") || strings.HasPrefix(arg.text, "-") && !strings.HasPrefix(arg.text, "--") && strings.Contains(arg.text, "f") || subcommand == "checkout" && arg.text == "--" {
				return true
			}
		case "branch":
			if arg.text == "-D" || arg.text == "-f" || arg.text == "--force" {
				return true
			}
		case "stash":
			if arg.text == "drop" || arg.text == "clear" {
				return true
			}
		}
		if subcommand == "push" && strings.HasPrefix(arg.text, "+") {
			return true
		}
	}
	return subcommand == "restore" || subcommand == "rebase"
}

func targetNeedsApproval(target shellToken, cwd string, written map[string]bool) bool {
	if target.dynamic || target.text == "" {
		return true
	}
	if isSafeRedirectSink(target.text) {
		return false
	}
	path := resolveTarget(target.text, cwd)
	if written[path] {
		return true
	}
	_, err := os.Lstat(path)
	if err == nil || !os.IsNotExist(err) {
		return true
	}
	written[path] = true
	return false
}

func resolveTarget(target, cwd string) string {
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Join(cwd, target)
}

func directoryAfter(words []shellToken, cwd string) string {
	if len(words) >= 2 && words[0].text == "cd" {
		if words[1].dynamic {
			return "\x00"
		}
		target := resolveTarget(words[1].text, cwd)
		if info, err := os.Stat(target); err != nil || !info.IsDir() {
			return "\x00"
		}
		return target
	}
	return cwd
}

func allDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, char := range text {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func hasReadOnlyShellSyntax(command string) bool {
	var invocation []string
	words := shellWords(command)
	for i := 0; i < len(words); i++ {
		token := words[i]
		if token.operator {
			if strings.ContainsAny(token.text, "<>") {
				if i+1 >= len(words) || words[i+1].operator {
					return false
				}
				i++
				target := words[i]
				if token.text == "<" || isSafeRedirectSink(target.text) || strings.HasSuffix(token.text, ">&") && (allDigits(target.text) || target.text == "-") {
					continue
				}
				return false
			}
			if !readOnlyInvocation(invocation) {
				return false
			}
			invocation = nil
			continue
		}
		invocation = append(invocation, token.text)
	}
	return readOnlyInvocation(invocation)
}

func readOnlyInvocation(words []string) bool {
	if len(words) == 0 {
		return true
	}
	if words[0] == "find" {
		for _, arg := range words[1:] {
			if arg == "-exec" || arg == "-execdir" || arg == "-ok" || arg == "-okdir" || arg == "-fprint" || arg == "-fprint0" || arg == "-fprintf" || arg == "-fls" {
				return false
			}
		}
	}
	if words[0] == "git" && len(words) > 1 {
		switch words[1] {
		case "branch":
			for _, arg := range words[2:] {
				if !strings.HasPrefix(arg, "-") {
					return false
				}
			}
		case "remote":
			if len(words) > 2 && words[2] != "-v" && words[2] != "--verbose" {
				return false
			}
		}
	}
	for _, pattern := range safePatterns {
		if pattern.MatchString(strings.Join(words, " ")) {
			return true
		}
	}
	return false
}

// shellWords only recognizes quoting, words, and control/redirection operators.
// It doesn't evaluate expansions or scripts: uncertain destinations require approval.
func shellWords(command string) []shellToken {
	var tokens []shellToken
	for i := 0; i < len(command); {
		if unicode.IsSpace(rune(command[i])) && command[i] != '\n' {
			i++
			continue
		}
		if command[i] == '#' {
			for i < len(command) && command[i] != '\n' {
				i++
			}
			continue
		}
		if strings.ContainsRune(";|&()<>\n", rune(command[i])) {
			start := i
			i++
			if i < len(command) && (command[i] == command[start] || command[start] == '&' && command[i] == '>' || command[start] == '>' && (command[i] == '&' || command[i] == '|')) {
				i++
				if i < len(command) && command[start:i] == "&>" && command[i] == '>' {
					i++
				}
			}
			tokens = append(tokens, shellToken{text: command[start:i], operator: true})
			continue
		}
		var word strings.Builder
		var quote byte
		dynamic := false
		for i < len(command) {
			char := command[i]
			if quote == 0 && (unicode.IsSpace(rune(char)) || strings.ContainsRune(";|&()<>", rune(char))) {
				break
			}
			if char == '\\' && quote != '\'' && i+1 < len(command) {
				i++
				word.WriteByte(command[i])
			} else if (char == '\'' || char == '"') && quote == 0 {
				quote = char
			} else if char == quote {
				quote = 0
			} else {
				if quote != '\'' && (char == '$' || char == '`' || quote == 0 && strings.ContainsRune("~*?[", rune(char))) {
					dynamic = true
				}
				word.WriteByte(char)
			}
			i++
		}
		// A bare number attached to a redirect is an fd, not a command argument.
		if i < len(command) && strings.ContainsRune("<>", rune(command[i])) && allDigits(word.String()) {
			continue
		}
		tokens = append(tokens, shellToken{text: word.String(), dynamic: dynamic || quote != 0})
	}
	return tokens
}
