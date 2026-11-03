package permission

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.harness.dev/harness/internal/cloudcmd"
)

var inlineDestructivePattern = regexp.MustCompile(`(?i)(\brmtree\s*\(|\b(?:os\.)?remove\s*\(|\bunlink(?:Sync)?\b|\brmdir(?:Sync)?\b|\b(?:fs\.|require\(['"]fs['"]\)\.)(?:rm|rmSync|writeFile|writeFileSync|truncate|truncateSync)\s*\(|\bopen\s*\([^\n]*[,=]\s*['"]w[b+t]*['"]|\b(?:write_text|write_bytes)\s*\(|\b(?:File|Dir)\.(?:delete|write|binwrite)\s*\()`)

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
	var workTree gitWorkTree
	var invocation []cloudcmd.Token
	for i := 0; i < len(words); i++ {
		token := words[i]
		if token.Operator && strings.Contains(token.Text, ">") {
			if i+1 >= len(words) || words[i+1].Operator {
				return true
			}
			i++
			target := words[i]
			if strings.HasSuffix(token.Text, ">&") && (target.Text == "-" || allDigits(target.Text)) {
				continue
			}
			if strings.Contains(token.Text, ">>") {
				if !target.Dynamic {
					written[resolveTarget(target.Text, cwd)] = true
				}
			} else if targetNeedsApproval(target, cwd, written) {
				return true
			}
			continue
		}
		if token.Operator {
			if token.Text == "<" && i+1 < len(words) {
				i++
				continue
			}
			if destructiveInvocation(invocation, cwd, written, workTree) {
				return true
			}
			cwd = directoryAfter(invocation, cwd)
			workTree = workTree.after(invocation)
			invocation = nil
			continue
		}
		invocation = append(invocation, token)
	}
	return destructiveInvocation(invocation, cwd, written, workTree)
}

// destructiveInvocation reports whether one simple command needs approval.
// workTree carries GIT_WORK_TREE from earlier statements and from wrappers
// such as env, so git's operands resolve where git writes.
func destructiveInvocation(words []cloudcmd.Token, cwd string, written map[string]bool, workTree gitWorkTree) bool {
	for len(words) > 0 && strings.Contains(words[0].Text, "=") {
		workTree = workTree.assign(words[0])
		words = words[1:]
	}
	if len(words) == 0 {
		return false
	}
	name := filepath.Base(words[0].Text)
	args := words[1:]
	switch name {
	case "rm", "rmdir", "unlink", "shred", "truncate":
		return true
	case "sed", "perl":
		for _, arg := range args {
			if strings.HasPrefix(arg.Text, "--in-place") || strings.HasPrefix(arg.Text, "-") && !strings.HasPrefix(arg.Text, "--") && strings.Contains(arg.Text, "i") {
				return true
			}
		}
	case "find":
		for i, arg := range args {
			if arg.Text == "-delete" {
				return true
			}
			if arg.Text == "-exec" || arg.Text == "-execdir" || arg.Text == "-ok" || arg.Text == "-okdir" {
				if destructiveInvocation(args[i+1:], cwd, written, workTree) {
					return true
				}
			}
		}
	case "xargs", "env", "command", "exec":
		for i := range args {
			if destructiveInvocation(args[i:], cwd, written, workTree) {
				return true
			}
			if name == "env" {
				cwd = envDirectory(args[i:], cwd)
			}
		}
	case "dd":
		for _, arg := range args {
			if strings.HasPrefix(arg.Text, "of=") {
				return true
			}
		}
	case "tee":
		for _, arg := range args {
			if arg.Text == "--append" || strings.HasPrefix(arg.Text, "-") && !strings.HasPrefix(arg.Text, "--") && strings.Contains(arg.Text, "a") {
				return false
			}
		}
		for _, arg := range args {
			if !strings.HasPrefix(arg.Text, "-") && targetNeedsApproval(arg, cwd, written) {
				return true
			}
		}
	case "cp", "mv":
		return copyNeedsApproval(args, cwd, written)
	case "git":
		return destructiveGit(args, cwd, workTree)
	}
	if name == "python" || name == "python3" || name == "node" || name == "perl" || name == "ruby" {
		for i, arg := range args {
			if (arg.Text == "-c" || arg.Text == "-e" || arg.Text == "--eval" || arg.Text == "-pe" || arg.Text == "-ne") && i+1 < len(args) && inlineDestructivePattern.MatchString(args[i+1].Text) {
				return true
			}
		}
	}
	return false
}

// envDirectory is the directory the command after "env -C dir" (or
// --chdir) runs in, when words starts with that option; otherwise cwd.
func envDirectory(words []cloudcmd.Token, cwd string) string {
	if dir, ok := strings.CutPrefix(words[0].Text, "--chdir="); ok {
		return gitDirectory(cloudcmd.Token{Text: dir, Dynamic: !literalWord(words[0])}, cwd)
	}
	if (words[0].Text == "-C" || words[0].Text == "--chdir") && len(words) > 1 {
		return gitDirectory(words[1], cwd)
	}
	return cwd
}

func copyNeedsApproval(args []cloudcmd.Token, cwd string, written map[string]bool) bool {
	var paths []cloudcmd.Token
	var destination cloudcmd.Token
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if (arg.Text == "-t" || arg.Text == "--target-directory") && i+1 < len(args) {
			i++
			destination = args[i]
		} else if strings.HasPrefix(arg.Text, "--target-directory=") {
			destination = cloudcmd.Token{Text: strings.TrimPrefix(arg.Text, "--target-directory="), Dynamic: arg.Dynamic}
		} else if arg.Text == "--" {
			paths = append(paths, args[i+1:]...)
			break
		} else if !strings.HasPrefix(arg.Text, "-") {
			paths = append(paths, arg)
		}
	}
	if destination.Text == "" {
		if len(paths) < 2 {
			return false
		}
		destination = paths[len(paths)-1]
		paths = paths[:len(paths)-1]
	}
	if destination.Dynamic {
		return true
	}
	info, err := os.Stat(resolveTarget(destination.Text, cwd))
	if err == nil && info.IsDir() {
		for _, source := range paths {
			if source.Dynamic || targetNeedsApproval(cloudcmd.Token{Text: filepath.Join(destination.Text, filepath.Base(source.Text))}, cwd, written) {
				return true
			}
		}
		return false
	}
	return targetNeedsApproval(destination, cwd, written)
}

func targetNeedsApproval(target cloudcmd.Token, cwd string, written map[string]bool) bool {
	if target.Dynamic || target.Text == "" {
		return true
	}
	if isSafeRedirectSink(target.Text) {
		return false
	}
	path := resolveTarget(target.Text, cwd)
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

// unknownDirectory stands for a directory the guard cannot know; no path
// under it can be checked, so every check under it asks.
const unknownDirectory = "\x00"

func directoryAfter(words []cloudcmd.Token, cwd string) string {
	if len(words) >= 2 && words[0].Text == "cd" {
		if words[1].Dynamic {
			return unknownDirectory
		}
		target := resolveTarget(words[1].Text, cwd)
		if info, err := os.Stat(target); err != nil || !info.IsDir() {
			return unknownDirectory
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
		if token.Operator {
			if strings.ContainsAny(token.Text, "<>") {
				if i+1 >= len(words) || words[i+1].Operator {
					return false
				}
				i++
				target := words[i]
				if token.Text == "<" || isSafeRedirectSink(target.Text) || strings.HasSuffix(token.Text, ">&") && (allDigits(target.Text) || target.Text == "-") {
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
		invocation = append(invocation, token.Text)
	}
	return readOnlyInvocation(invocation)
}

// cloudPrograms are classified by cloudcmd.Analyze; plan readability of the
// whole command additionally requires that analysis to be none or read.
var cloudPrograms = map[string]bool{cloudcmd.ToolKubectl: true, cloudcmd.ToolHelm: true, cloudcmd.ToolTerraform: true, cloudcmd.ToolKustomize: true}

func readOnlyInvocation(words []string) bool {
	if len(words) == 0 {
		return true
	}
	if cloudPrograms[filepath.Base(words[0])] {
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

// shellWords uses the shared quote-aware lexer: words and control/redirection
// operators only. Expansions and scripts are never evaluated, so uncertain
// destinations require approval.
func shellWords(command string) []cloudcmd.Token {
	return cloudcmd.Lex(command).Tokens
}
