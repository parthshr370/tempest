package permission

import (
	"os"
	"strings"

	"go.harness.dev/harness/internal/cloudcmd"
)

// gitGlobalValueOptions are git's global options whose value is the next
// word when it is not attached with "=".
var gitGlobalValueOptions = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true, "--config-env": true}

// gitValueOptions are the subcommand options whose value is the next word (or
// the rest of a short-option cluster), for the subcommands whose rules read
// operands. Without them a branch name after -b would read as a path.
var gitValueOptions = map[string]map[string]bool{
	"checkout": {"-b": true, "-B": true, "--orphan": true, "--conflict": true, "--pathspec-from-file": true},
	"push":     {"-o": true, "--push-option": true, "--repo": true, "--receive-pack": true, "--exec": true},
	"stash":    {"-m": true, "--message": true},
	"worktree": {"-b": true, "-B": true, "--reason": true},
}

// gitCommand is one git invocation parsed the way git reads it: global
// options, the subcommand, its options (short clusters split into single
// flags, long options without their "=value"), its operands, and the
// pathspecs after a "--".
type gitCommand struct {
	subcommand string
	flags      map[string]bool
	operands   []cloudcmd.Token
	pathspecs  []cloudcmd.Token
	// cwd is the directory relative pathspecs resolve against, after -C.
	cwd string
}

func parseGitCommand(args []cloudcmd.Token, cwd string) (gitCommand, bool) {
	for len(args) > 0 && strings.HasPrefix(args[0].Text, "-") {
		option := args[0]
		args = args[1:]
		if !gitGlobalValueOptions[option.Text] || len(args) == 0 {
			continue
		}
		if option.Text == "-C" {
			cwd = gitDirectory(args[0], cwd)
		}
		args = args[1:]
	}
	if len(args) == 0 {
		return gitCommand{}, false
	}
	command := gitCommand{subcommand: args[0].Text, flags: map[string]bool{}, cwd: cwd}
	valueOptions := gitValueOptions[command.subcommand]
	args = args[1:]
	for i := 0; i < len(args); i++ {
		word := args[i]
		switch {
		case word.Text == "--":
			command.pathspecs = args[i+1:]
			return command, true
		case !literalWord(word) || len(word.Text) < 2 || word.Text[0] != '-':
			command.operands = append(command.operands, word)
		case strings.HasPrefix(word.Text, "--"):
			name, _, attached := strings.Cut(word.Text, "=")
			command.flags[name] = true
			if valueOptions[name] && !attached {
				i++
			}
		default:
			for j := 1; j < len(word.Text); j++ {
				flag := "-" + word.Text[j:j+1]
				command.flags[flag] = true
				if valueOptions[flag] {
					if j == len(word.Text)-1 {
						i++
					}
					break
				}
			}
		}
	}
	return command, true
}

// gitDirectory is the directory "git -C dir" runs in; an expanded value is
// unknown, which makes every pathspec existence check ask.
func gitDirectory(dir cloudcmd.Token, cwd string) string {
	if !literalWord(dir) {
		return unknownDirectory
	}
	return resolveTarget(dir.Text, cwd)
}

// literalWord reports whether the shell passes word to git as written. The
// shared lexer marks every word holding "~" dynamic, but the shell expands
// only a leading tilde or one after "=" or ":", so a revision like HEAD~1 is
// literal.
func literalWord(word cloudcmd.Token) bool {
	if !word.Dynamic {
		return true
	}
	text := word.Text
	return !strings.HasPrefix(text, "~") && !strings.Contains(text, "=~") && !strings.Contains(text, ":~") && !strings.ContainsAny(text, "$`*?[{")
}

func (c gitCommand) has(flags ...string) bool {
	for _, flag := range flags {
		if c.flags[flag] {
			return true
		}
	}
	return false
}

func (c gitCommand) firstOperand() string {
	if len(c.operands) == 0 {
		return ""
	}
	return c.operands[0].Text
}

// destructiveGit reports whether a git invocation can discard uncommitted
// work, delete untracked files, or drop commits, stashes, branches or remote
// history. Read-only and history-adding commands never match.
func destructiveGit(args []cloudcmd.Token, cwd string) bool {
	command, ok := parseGitCommand(args, cwd)
	if !ok {
		return false
	}
	switch command.subcommand {
	case "reset":
		return command.has("--hard", "--merge", "--keep")
	case "clean":
		return !command.has("-n", "--dry-run")
	case "rm":
		// --cached only unstages; the file stays in the worktree.
		return !command.has("--cached", "-n", "--dry-run")
	case "restore":
		// --staged alone restores only the index.
		return command.has("-W", "--worktree") || !command.has("-S", "--staged")
	case "switch":
		return command.has("-f", "--force", "--discard-changes")
	case "checkout":
		return command.checkoutOverwritesFiles()
	case "stash":
		return command.firstOperand() == "drop" || command.firstOperand() == "clear"
	case "branch":
		// -D, -M and -C delete or overwrite a branch; -f forces any of them.
		return command.has("-D", "-M", "-C", "-f", "--force")
	case "push":
		return command.pushRewritesRemote()
	case "worktree":
		return command.firstOperand() == "remove" && command.has("-f", "--force")
	case "rebase":
		return true
	}
	return false
}

// checkoutOverwritesFiles reports whether a checkout writes paths over the
// worktree, which discards their uncommitted changes. git reads its first
// operand as a branch or commit unless it names a path, and every later
// operand as a path, so an operand that exists as a path asks.
func (c gitCommand) checkoutOverwritesFiles() bool {
	if c.has("-f", "--force", "-p", "--patch", "--pathspec-from-file") || len(c.pathspecs) > 0 {
		return true
	}
	operands := c.operands
	if c.has("-b", "-B", "--orphan") {
		// The first operand is the new branch's start point.
		if len(operands) > 0 {
			operands = operands[1:]
		}
		return len(operands) > 0
	}
	if len(operands) > 1 {
		return true
	}
	return len(operands) == 1 && gitPathspecMayMatch(operands[0], c.cwd)
}

// gitRefForbidden holds characters git forbids in ref names (see
// git-check-ref-format) that mark a word as a pathspec glob or magic.
const gitRefForbidden = "*?[:"

// gitPathspecMayMatch reports whether word can name a path: a glob or
// pathspec magic, an expansion, or a path that exists or cannot be checked.
func gitPathspecMayMatch(word cloudcmd.Token, cwd string) bool {
	if !literalWord(word) || strings.ContainsAny(word.Text, gitRefForbidden) {
		return true
	}
	_, err := os.Lstat(resolveTarget(word.Text, cwd))
	return err == nil || !os.IsNotExist(err)
}

// pushRewritesRemote reports whether a push can drop remote commits or refs:
// a force, a delete, a mirror or prune, a "+" force refspec or a ":" delete
// refspec. A dry run changes nothing.
func (c gitCommand) pushRewritesRemote() bool {
	if c.has("-n", "--dry-run") {
		return false
	}
	if c.has("-f", "--force", "--force-with-lease", "--force-if-includes", "-d", "--delete", "--mirror", "--prune") {
		return true
	}
	for _, operand := range c.operands {
		if strings.HasPrefix(operand.Text, "+") || strings.HasPrefix(operand.Text, ":") {
			return true
		}
	}
	return false
}
