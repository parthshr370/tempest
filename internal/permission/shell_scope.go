package permission

import (
	"maps"
	"os"
	"slices"
	"strings"

	"go.harness.dev/harness/internal/cloudcmd"
)

// shellState is a directory and GIT_WORK_TREE a shell statement can run with.
type shellState struct {
	cwd      string
	workTree gitWorkTree
}

// shellStates holds every state a statement can run with. The guard never
// evaluates a condition, so a cd that may not have run leaves both
// directories as candidates, and every path check runs in each of them.
type shellStates []shellState

// maxShellStates bounds the candidates. Past it the directory is unknown,
// and every relative path check under it asks.
const maxShellStates = 16

// maxLoopPasses bounds re-reading a loop for the states its later iterations
// start in; a loop that needs more asks.
const maxLoopPasses = 32

// maxShellCommandBytes, maxShellDepth and maxShellWork bound what a walk may
// spend; a command past any of them asks instead. Linux caps one exec
// argument (MAX_ARG_STRLEN) at 128 KiB, so bash -c cannot run a longer
// command there. Work counts tokens read, loop passes included, and the
// candidate paths each statement copies.
const (
	maxShellCommandBytes = 128 << 10
	maxShellDepth        = 64
	maxShellWork         = 1 << 20
)

// unknownState is a state whose directory and GIT_WORK_TREE are unknown.
var unknownState = shellState{cwd: unknownDirectory, workTree: gitWorkTree{value: cloudcmd.Token{Dynamic: true}, set: true}}

func (s shellStates) union(more ...shellStates) shellStates {
	out := slices.Clone(s)
	for _, states := range more {
		for _, state := range states {
			if !slices.Contains(out, state) {
				out = append(out, state)
			}
		}
	}
	if len(out) > maxShellStates {
		return shellStates{unknownState}
	}
	return out
}

func (s shellStates) within(other shellStates) bool {
	for _, state := range s {
		if !slices.Contains(other, state) {
			return false
		}
	}
	return true
}

// shellCheck is what a walk asks of every simple command and output
// redirection, in each state it can run with. written holds the paths
// earlier statements created; a check may add to it.
type shellCheck struct {
	command  func(words []cloudcmd.Token, state shellState, written map[string]bool) bool
	redirect func(op string, target cloudcmd.Token, state shellState, written map[string]bool) bool
}

// walkShell reports whether check matches any statement of command, which
// starts in cwd. It follows the directory and GIT_WORK_TREE the way the
// shell scopes them: a subshell, a pipeline member, a background list and a
// command substitution keep their changes to themselves; a statement after
// && or || runs only in the states that branch can reach; the branches of
// if and case and every iteration of a loop all count. Nothing is
// evaluated, so a cd whose effect is uncertain leaves each possible
// directory as a candidate, and anything that may change the directory in a
// way the walk does not follow leaves it unknown (see directoryAfter). A
// command too large to walk within the bounds matches.
func walkShell(command, cwd string, check shellCheck) bool {
	if len(command) > maxShellCommandBytes {
		return true
	}
	start := shellState{cwd: cwd}
	if os.Getenv("BASH_ENV") != "" {
		// bash runs this file first, and it may cd.
		start = unknownState
	}
	w := shellWalk{words: shellWords(command), written: map[string]bool{}, check: check, list: statement{cur: shellStates{start}}, opaque: environmentOpaque()}
	return w.walk()
}

// statement is the progress through one list: the simple command being
// read, the pipeline it belongs to and the && / || chain around it.
type statement struct {
	cur      shellStates // the states the next and-or list starts in
	start    shellStates // the states the current and-or list started in
	ok, fail shellStates // the chain so far ended in success / failure in these
	op       string      // the connector before the next pipeline: "", "&&" or "||"
	pipeIn   shellStates // the states the current pipeline started in
	member   shellStates // the states the last finished pipeline member left
	piped    bool        // the pipeline runs a member in a subshell
	in       shellStates // the states the current member runs in; nil before it starts
	out      shellStates // a finished compound member's result
	words    []cloudcmd.Token
	header   bool // for, select or case header words, which run nothing
	// redirected marks a member with a redirection, which can fail and
	// skip the command.
	redirected bool
}

// scope is an open compound command.
type scope struct {
	kind     string      // "(" subshell, "$(" substitution, "{", "if", "loop" or "case"
	function bool        // the body of a function definition
	at       int         // the token opening it, re-read for a loop's later iterations
	in       shellStates // the states the compound command started in
	body     shellStates // the states its first list starts in
	cond     shellStates // if and loop: the states after the latest condition
	seen     shellStates // the states every list inside ended in
	opaque   bool        // the walk was opaque when this pass opened it
	outer    statement   // the enclosing list, resumed at the close
	written  map[string]bool
	pattern  bool // case: the next words are an arm's pattern
}

type shellWalk struct {
	words   []cloudcmd.Token
	i       int
	written map[string]bool
	check   shellCheck
	list    statement
	scopes  []scope
	// function marks the next compound command as a function body.
	function bool
	// heredoc reports that a here-document began. The flat lexer reads its
	// body as commands, so from there on no statement replaces a state; each
	// only adds candidates.
	heredoc bool
	// opaque reports that cd may no longer mean the builtin the walk
	// follows (see opaqueCommand); from there on every cd leaves an unknown
	// directory.
	opaque bool
	// work is the effort spent so far, against maxShellWork.
	work   int
	reopen map[int]shellStates
	passes map[int]int
}

func (w *shellWalk) walk() bool {
	for w.i = 0; ; w.i++ {
		w.work++
		if w.work > maxShellWork || len(w.scopes) > maxShellDepth {
			return true
		}
		if w.i >= len(w.words) {
			if len(w.scopes) == 0 {
				return w.endList(false)
			}
			// A compound command still open at the end closes there.
			if w.close() {
				return true
			}
			continue
		}
		token := w.words[w.i]
		var ask bool
		if token.Operator {
			ask = w.operator(token.Text)
		} else {
			ask = w.word(token)
		}
		if ask {
			return true
		}
	}
}

func (w *shellWalk) next() (cloudcmd.Token, bool) {
	if w.i+1 < len(w.words) {
		return w.words[w.i+1], true
	}
	return cloudcmd.Token{}, false
}

func (w *shellWalk) previous() string {
	if w.i > 0 && w.words[w.i-1].Operator {
		return w.words[w.i-1].Text
	}
	return ""
}

func (w *shellWalk) top() *scope {
	if len(w.scopes) == 0 {
		return nil
	}
	return &w.scopes[len(w.scopes)-1]
}

func (w *shellWalk) operator(text string) bool {
	w.list.header = false
	switch text {
	case "<":
		// A process substitution <(...) is read as a substitution.
		if next, ok := w.next(); ok && next.Text != "(" {
			w.begin()
			w.list.redirected = true
			w.i++
			if next.Operator && next.Text == "&" {
				if fd, ok := w.next(); ok && !fd.Operator {
					w.i++
				}
			}
		}
		return false
	case "<<":
		if next, ok := w.next(); ok && !next.Operator {
			w.begin()
			w.list.redirected = true
			w.heredoc = true
			w.i++
		}
		return false
	case "(", "((":
		for range text {
			w.openParen()
		}
		return false
	case ")", "))":
		for range text {
			if w.closeParen() {
				return true
			}
		}
		return false
	case "|":
		if w.endMember() {
			return true
		}
		w.list.piped = true
		return false
	case "&&", "||":
		if w.endPipeline() {
			return true
		}
		w.list.op = text
		return false
	case "&":
		// |& pipes stderr; ;& and ;;& end a case arm.
		switch w.previous() {
		case "|":
			return false
		case ";", ";;":
			if s := w.top(); s != nil && s.kind == "case" {
				s.pattern = true
			}
			return false
		}
		return w.endList(true)
	case "\n":
		l := &w.list
		if l.in == nil && (l.op != "" && l.pipeIn == nil || l.piped) {
			// A line ending in &&, || or | continues on the next line.
			return false
		}
		return w.endList(false)
	case ";;":
		if w.endList(false) {
			return true
		}
		if s := w.top(); s != nil && s.kind == "case" {
			s.pattern = true
			// After ;;& the next pattern is tested where the arm ended.
			if next, _ := w.next(); next.Text != "&" {
				w.restart(s.body)
			}
		}
		return false
	}
	if strings.Contains(text, ">") {
		return w.redirect(text)
	}
	return w.endList(false)
}

func (w *shellWalk) redirect(op string) bool {
	target := cloudcmd.Token{Dynamic: true}
	if next, ok := w.next(); ok && !next.Operator {
		w.i++
		target = next
	}
	w.begin()
	w.list.redirected = true
	return w.eachState(func(state shellState, written map[string]bool) bool {
		return w.check.redirect(op, target, state, written)
	})
}

// word reads one word, which at the start of a command may be a reserved
// word that opens, divides or closes a compound command.
func (w *shellWalk) word(token cloudcmd.Token) bool {
	l := &w.list
	s := w.top()
	if l.header {
		switch {
		case token.Text == "in" && s.kind == "case":
			l.header = false
			return false
		case token.Text != "do":
			return false
		}
		l.header = false
	}
	if len(l.words) > 0 || l.out != nil || token.Dynamic {
		w.begin()
		l.words = append(l.words, token)
		return false
	}
	kind := ""
	if s != nil {
		kind = s.kind
	}
	switch {
	case token.Text == "!":
		// Negation changes the status, not the states a pipeline leaves.
		w.begin()
	case token.Text == "{":
		w.open("{")
	case token.Text == "if":
		w.open("if")
	case token.Text == "while" || token.Text == "until":
		w.open("loop")
	case token.Text == "for" || token.Text == "select":
		w.open("loop")
		w.list.header = true
	case token.Text == "case":
		w.open("case")
		w.list.header = true
	case token.Text == "function":
		if next, ok := w.next(); ok && !next.Operator {
			w.i++
		}
		w.skipEmptyParens()
		w.function = true
	case kind == "if" && token.Text == "then":
		if w.endList(false) {
			return true
		}
		s.cond = w.list.cur
		w.restart(s.cond)
	case kind == "if" && (token.Text == "elif" || token.Text == "else"):
		if w.endList(false) {
			return true
		}
		w.restart(s.cond)
	case kind == "loop" && token.Text == "do":
		if w.endList(false) {
			return true
		}
		s.cond = w.list.cur
		w.restart(s.cond)
	case kind == "case" && token.Text == "in":
	case kind == "if" && token.Text == "fi", kind == "loop" && token.Text == "done", kind == "case" && token.Text == "esac", kind == "{" && token.Text == "}":
		return w.close()
	default:
		w.begin()
		l.words = append(l.words, token)
	}
	return false
}

// skipEmptyParens skips the "()" after a function's name.
func (w *shellWalk) skipEmptyParens() {
	if w.i+2 < len(w.words) && w.words[w.i+1].Operator && w.words[w.i+1].Text == "(" && w.words[w.i+2].Operator && w.words[w.i+2].Text == ")" {
		w.i += 2
	}
}

func (w *shellWalk) openParen() {
	l := &w.list
	s := w.top()
	switch {
	case s != nil && s.kind == "case" && s.pattern && len(l.words) == 0:
		// The optional parenthesis before a pattern.
	case len(l.words) == 1 && l.out == nil && w.i+1 < len(w.words) && w.words[w.i+1].Operator && w.words[w.i+1].Text == ")":
		// name() starts a function definition.
		l.words = nil
		w.i++
		w.function = true
	case len(l.words) > 0 || l.out != nil || w.previous() == "<":
		w.open("$(")
	default:
		w.open("(")
	}
}

func (w *shellWalk) closeParen() bool {
	s := w.top()
	switch {
	case s == nil:
	case s.kind == "(" || s.kind == "$(":
		return w.close()
	case s.kind == "case":
		// A pattern's closing parenthesis starts the arm, entered from the
		// case or, after ;&, from the arm before it.
		if w.endList(false) {
			return true
		}
		s.pattern = false
		w.restart(s.body.union(w.list.cur))
	}
	return false
}

// begin starts a pipeline member in the states the chain lets it run in.
func (w *shellWalk) begin() {
	l := &w.list
	if l.in != nil {
		return
	}
	if l.pipeIn == nil {
		switch l.op {
		case "&&":
			l.pipeIn = l.ok
		case "||":
			l.pipeIn = l.fail
		default:
			l.pipeIn, l.start = l.cur, l.cur
		}
	}
	l.in = l.pipeIn
}

// eachState runs check in every state the current member can run in. Each
// sees the paths earlier statements created, not those the same statement
// creates from another candidate directory.
func (w *shellWalk) eachState(check func(state shellState, written map[string]bool) bool) bool {
	in := w.list.in
	if len(in) == 1 {
		return check(in[0], w.written)
	}
	// Each candidate copies the paths created so far.
	w.work += len(in) * len(w.written)
	merged := maps.Clone(w.written)
	for _, state := range in {
		written := maps.Clone(w.written)
		if check(state, written) {
			return true
		}
		maps.Copy(merged, written)
	}
	w.written = merged
	return false
}

// endMember checks the current simple command and records the states the
// member leaves.
func (w *shellWalk) endMember() bool {
	l := &w.list
	if l.in == nil {
		return false
	}
	words := l.words
	if w.eachState(func(state shellState, written map[string]bool) bool {
		return w.check.command(words, state, written)
	}) {
		return true
	}
	out := l.out
	switch {
	case out == nil && opaqueCommand(words):
		w.opaque = true
		out = shellStates{unknownState}
	case out == nil:
		for _, state := range l.in {
			out = out.union(shellStates{{cwd: w.directoryAfter(words, state.cwd, l.redirected), workTree: state.workTree.after(words)}})
		}
	case l.redirected:
		// A redirection that fails skips the compound command.
		out = out.union(l.in)
	}
	if w.heredoc {
		out = out.union(l.in)
	}
	l.member, l.in, l.out, l.words, l.redirected = out, nil, nil, nil, false
	return false
}

// endPipeline adds the finished pipeline to the && / || chain. A member that
// may run in a subshell leaves the pipeline's starting states too. A cd into
// a directory that exists is taken to succeed, as for a single statement, so
// a pipeline leaves the same states whether it succeeds or fails.
func (w *shellWalk) endPipeline() bool {
	if w.endMember() {
		return true
	}
	l := &w.list
	if l.pipeIn == nil {
		return false
	}
	result := l.member
	if l.piped {
		result = result.union(l.pipeIn)
	}
	switch l.op {
	case "&&":
		l.ok, l.fail = result, l.fail.union(result)
	case "||":
		l.ok, l.fail = l.ok.union(result), result
	default:
		l.ok, l.fail = result, result
	}
	l.pipeIn, l.member, l.piped, l.op = nil, nil, false, ""
	return false
}

// endList finishes an and-or list. A background list runs in a subshell, so
// the next one starts where it started.
func (w *shellWalk) endList(background bool) bool {
	if w.endPipeline() {
		return true
	}
	l := &w.list
	if l.ok == nil {
		return false
	}
	ended := l.ok.union(l.fail)
	switch {
	case !background:
		l.cur = ended
	case w.heredoc:
		l.cur = l.start.union(ended)
	default:
		l.cur = l.start
	}
	l.ok, l.fail, l.op, l.start = nil, nil, "", nil
	// break, continue and return leave from any list, so every enclosing
	// compound command up to the nearest subshell or function may end here.
	for i := len(w.scopes) - 1; i >= 0; i-- {
		s := &w.scopes[i]
		s.seen = s.seen.union(l.cur)
		if s.kind == "(" || s.kind == "$(" || s.function {
			break
		}
	}
	return false
}

// restart starts a branch, an arm or a loop body in states.
func (w *shellWalk) restart(states shellStates) {
	if w.heredoc {
		states = states.union(w.list.cur)
	}
	w.list = statement{cur: states}
}

func (w *shellWalk) open(kind string) {
	w.begin()
	s := scope{kind: kind, function: w.function, at: w.i, in: w.list.in, body: w.list.in, opaque: w.opaque, outer: w.list, pattern: kind == "case"}
	w.function = false
	if s.function {
		// A function body runs when it is called, in any later state.
		s.body = shellStates{unknownState}
	}
	if kind == "loop" {
		if again, ok := w.reopen[w.i]; ok {
			s.body = again
			delete(w.reopen, w.i)
		}
		w.work += len(w.written)
		s.written = maps.Clone(w.written)
	}
	w.scopes = append(w.scopes, s)
	w.list = statement{cur: s.body}
}

// close ends the innermost compound command. A loop whose iterations can
// start in states its body was not read in, or whose body made the walk
// opaque after its cds were read, is read again.
func (w *shellWalk) close() bool {
	if w.endList(false) {
		return true
	}
	s := w.scopes[len(w.scopes)-1]
	w.scopes = w.scopes[:len(w.scopes)-1]
	var out shellStates
	switch s.kind {
	case "(":
		out = s.in
	case "$(":
		inner := w.list.cur
		w.list = s.outer
		if w.heredoc {
			w.list.in = w.list.in.union(s.seen, inner)
		}
		return false
	case "{":
		out = w.list.cur
	case "if", "loop":
		out = s.seen.union(s.cond, w.list.cur)
	case "case":
		out = s.seen.union(s.body, w.list.cur)
	}
	if s.kind == "loop" && (!out.within(s.body) || w.opaque && !s.opaque) {
		if w.passes == nil {
			w.passes, w.reopen = map[int]int{}, map[int]shellStates{}
		}
		w.passes[s.at]++
		if w.passes[s.at] > maxLoopPasses {
			return true
		}
		w.reopen[s.at] = s.body.union(out)
		w.list, w.written = s.outer, s.written
		w.i = s.at - 1
		return false
	}
	if s.function {
		// Defining a function runs nothing, but calling it later may change
		// the directory, and it may replace cd itself.
		out = s.in.union(shellStates{unknownState})
		w.opaque = true
	}
	if w.heredoc {
		out = out.union(s.in, s.seen, w.list.cur)
	}
	w.list = s.outer
	w.list.out = out
	return false
}

// directoryAfter returns the directory a simple command leaves the shell in.
// It follows only the plain forms of cd and pushd it fully understands: the
// command alone or behind command or builtin, cd with no option but -L, one
// literal argument naming an existing directory (see logicalDirectory), and
// no redirection, which could fail and skip the cd. Every other command
// that names cd, pushd or popd, including wrappers such as time, leaves an
// unknown directory, as does every cd once the walk is opaque.
func (w *shellWalk) directoryAfter(words []cloudcmd.Token, cwd string, redirected bool) string {
	command := words
	if len(command) > 1 && (command[0].Text == "command" || command[0].Text == "builtin") {
		command = command[1:]
	}
	if len(command) > 0 && (command[0].Text == "cd" || command[0].Text == "pushd") {
		name, args := command[0].Text, command[1:]
		for name == "cd" && len(args) > 0 && args[0].Text == "-L" {
			args = args[1:]
		}
		if w.opaque || redirected || len(args) != 1 || args[0].Dynamic || args[0].Text == "" || strings.HasPrefix(args[0].Text, "-") || strings.HasPrefix(args[0].Text, "+") {
			return unknownDirectory
		}
		return logicalDirectory(args[0].Text, cwd)
	}
	for _, word := range words {
		switch word.Text {
		case "cd", "pushd", "popd":
			return unknownDirectory
		}
	}
	return cwd
}

// logicalDirectory is where cd -L dir leaves the shell when the guard can
// tell. bash removes the component before each ".." once it checks that it
// is a directory, then needs an existing directory; resolveTarget takes a
// ".." only where it is also the physical parent, so bash's logical result
// and the directory the kernel holds agree. Anything else is unknown.
func logicalDirectory(dir, cwd string) string {
	path := resolveTarget(dir, cwd)
	if path == unknownDirectory || !isDirectory(path) {
		return unknownDirectory
	}
	return path
}

func isDirectory(path string) bool {
	if path == "" {
		path = "."
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// opaqueCommand reports whether a simple command can change, in a way the
// walk does not follow, what cd means or where later commands run: it runs
// text as commands (eval, source, ., a computed command name), sets up
// commands to run later (trap, alias), replaces builtins (enable) or shell
// options (shopt, set -P, set -o physical), or may set CDPATH, which
// redirects a relative cd (any word naming CDPATH, a nameref, or a variable
// assigned through a computed name).
func opaqueCommand(words []cloudcmd.Token) bool {
	for _, word := range words {
		if strings.Contains(word.Text, "CDPATH") {
			return true
		}
	}
	for len(words) > 0 && isAssignment(words[0].Text) {
		words = words[1:]
	}
	for len(words) > 1 && (words[0].Text == "command" || words[0].Text == "builtin" || words[0].Text == "time" || words[0].Text == "-p") {
		words = words[1:]
	}
	if len(words) == 0 {
		return false
	}
	name, args := words[0], words[1:]
	if name.Dynamic {
		// An expanded name holding a "/" runs a file, never a builtin.
		return !strings.Contains(name.Text, "/")
	}
	switch name.Text {
	case "eval", "source", ".", "trap", "alias", "enable", "shopt":
		return true
	case "set":
		for _, arg := range args {
			if arg.Dynamic || arg.Text == "physical" || len(arg.Text) > 1 && (arg.Text[0] == '-' || arg.Text[0] == '+') && strings.Contains(arg.Text, "P") {
				return true
			}
		}
	case "printf":
		for i, arg := range args {
			if arg.Text == "-v" && i+1 < len(args) && strings.ContainsAny(args[i+1].Text, "$`") {
				return true
			}
		}
	case "declare", "typeset", "local", "export", "readonly", "read", "mapfile", "readarray", "getopts", "let":
		nameref := name.Text == "declare" || name.Text == "typeset" || name.Text == "local"
		for _, arg := range args {
			variable, _, _ := strings.Cut(arg.Text, "=")
			if strings.ContainsAny(variable, "$`") || nameref && strings.HasPrefix(arg.Text, "-") && strings.Contains(arg.Text, "n") {
				return true
			}
		}
	}
	return false
}

// isAssignment reports whether word is a shell variable assignment: a name
// of letters, digits and underscores, not starting with a digit, then "=".
func isAssignment(word string) bool {
	name, _, ok := strings.Cut(word, "=")
	if !ok || name == "" || name[0] >= '0' && name[0] <= '9' {
		return false
	}
	for _, char := range name {
		if char != '_' && (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}

// environmentOpaque reports whether the environment bash inherits can change
// what cd means: CDPATH, exported functions, or SHELLOPTS with physical.
// BASH_ENV, a file bash runs first, also leaves the start unknown.
func environmentOpaque() bool {
	if os.Getenv("CDPATH") != "" || os.Getenv("BASH_ENV") != "" || strings.Contains(os.Getenv("SHELLOPTS"), "physical") {
		return true
	}
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "BASH_FUNC_") {
			return true
		}
	}
	return false
}
