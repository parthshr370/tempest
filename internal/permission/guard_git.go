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
	// workTree is the work tree --work-tree or GIT_WORK_TREE selects, or
	// "" when neither is set and the work tree is the one around cwd.
	workTree string
}

// gitWorkTree is the work tree the environment selects for git through
// GIT_WORK_TREE, set by an assignment before the command or left by an
// earlier one. A Dynamic value is unknown.
type gitWorkTree struct {
	value cloudcmd.Token
	set   bool
}

// assign returns w after the shell assignment word, which selects a work
// tree only when it sets GIT_WORK_TREE. GIT_DIR alone keeps the current
// directory as the work tree's top, where operands already resolve.
func (w gitWorkTree) assign(word cloudcmd.Token) gitWorkTree {
	value, ok := strings.CutPrefix(word.Text, "GIT_WORK_TREE=")
	if !ok {
		return w
	}
	return gitWorkTree{value: cloudcmd.Token{Text: value, Dynamic: !literalWord(word)}, set: true}
}

// after returns the GIT_WORK_TREE an invocation leaves for the commands
// after it: an export (or declare, typeset, readonly, local) of it, or a
// statement made only of assignments, which reaches git when the variable
// is already exported.
func (w gitWorkTree) after(words []cloudcmd.Token) gitWorkTree {
	if len(words) == 0 {
		return w
	}
	switch words[0].Text {
	case "export", "declare", "typeset", "readonly", "local":
		words = words[1:]
	default:
		for _, word := range words {
			if !strings.Contains(word.Text, "=") {
				// Assignments before a command apply to that command only.
				return w
			}
		}
	}
	for _, word := range words {
		w = w.assign(word)
	}
	return w
}

func parseGitCommand(args []cloudcmd.Token, cwd string, workTree gitWorkTree) (gitCommand, bool) {
	for len(args) > 0 && strings.HasPrefix(args[0].Text, "-") {
		option := args[0]
		args = args[1:]
		if value, ok := strings.CutPrefix(option.Text, "--work-tree="); ok {
			workTree = gitWorkTree{value: cloudcmd.Token{Text: value, Dynamic: !literalWord(option)}, set: true}
			continue
		}
		if !gitGlobalValueOptions[option.Text] || len(args) == 0 {
			continue
		}
		switch option.Text {
		case "-C":
			cwd = gitDirectory(args[0], cwd)
		case "--work-tree":
			workTree = gitWorkTree{value: args[0], set: true}
		}
		args = args[1:]
	}
	if len(args) == 0 {
		return gitCommand{}, false
	}
	command := gitCommand{subcommand: args[0].Text, flags: map[string]bool{}, cwd: cwd}
	if workTree.set {
		// git resolves a relative work tree after every -C has run.
		command.workTree = gitDirectory(workTree.value, cwd)
	}
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

// gitDirectory is the directory "git -C dir" runs in, or the work tree
// "--work-tree dir" selects; an expanded value is unknown, which makes every
// pathspec existence check ask.
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
func destructiveGit(args []cloudcmd.Token, cwd string, workTree gitWorkTree) bool {
	command, ok := parseGitCommand(args, cwd, workTree)
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
	return len(operands) == 1 && c.pathspecMayMatch(operands[0])
}

// gitRefForbidden holds characters git forbids in ref names (see
// git-check-ref-format) that mark a word as a pathspec glob or magic.
const gitRefForbidden = "*?[:"

// pathspecMayMatch reports whether word can name a path: a glob or pathspec
// magic, an expansion, or a path that exists or cannot be checked. With a
// selected work tree git reads a relative path from cwd when cwd lies inside
// the work tree and from the work tree's top otherwise, so both are checked.
func (c gitCommand) pathspecMayMatch(word cloudcmd.Token) bool {
	if !literalWord(word) || strings.ContainsAny(word.Text, gitRefForbidden) {
		return true
	}
	return pathMayExist(resolveTarget(word.Text, c.cwd)) || c.workTree != "" && pathMayExist(resolveTarget(word.Text, c.workTree))
}

// pathMayExist reports whether path exists or cannot be checked.
func pathMayExist(path string) bool {
	_, err := os.Lstat(path)
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
