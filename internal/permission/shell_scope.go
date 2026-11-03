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

var unknownState = shellState{cwd: unknownDirectory}

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
// shell scopes them: a subshell, a pipeline member, a background list, a
// command substitution and a function definition keep their changes to
// themselves; a statement after && or || runs only in the states that
// branch can reach; the branches of if and case and every iteration of a
// loop all count. Nothing is evaluated, so a cd whose effect is uncertain
// leaves each possible directory as a candidate.
func walkShell(command, cwd string, check shellCheck) bool {
	w := shellWalk{words: shellWords(command), written: map[string]bool{}, check: check, list: statement{cur: shellStates{{cwd: cwd}}}}
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
	changed  bool        // a statement inside sets the directory or GIT_WORK_TREE
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
	reopen  map[int]shellStates
	passes  map[int]int
}

func (w *shellWalk) walk() bool {
	for w.i = 0; ; w.i++ {
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
	if out == nil {
		for _, state := range l.in {
			next, changed := stateAfter(words, state)
			out = out.union(shellStates{next})
			if changed {
				for i := range w.scopes {
					w.scopes[i].changed = true
				}
			}
		}
	}
	if w.heredoc {
		out = out.union(l.in)
	}
	l.member, l.in, l.out, l.words = out, nil, nil, nil
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
	s := scope{kind: kind, function: w.function, at: w.i, in: w.list.in, body: w.list.in, outer: w.list, pattern: kind == "case"}
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
		s.written = maps.Clone(w.written)
	}
	w.scopes = append(w.scopes, s)
	w.list = statement{cur: s.body}
}

// close ends the innermost compound command. A loop whose iterations can
// start in states its body was not read in is read again in those too.
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
	if s.kind == "loop" && !out.within(s.body) {
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
		// Defining a function runs nothing; calling it later may change state.
		out = s.in
		if s.changed {
			out = out.union(shellStates{unknownState})
		}
	}
	if w.heredoc {
		out = out.union(s.in, s.seen, w.list.cur)
	}
	w.list = s.outer
	w.list.out = out
	return false
}

// stateAfter returns the state a simple command leaves for the statements
// after it, and whether the command sets the directory or GIT_WORK_TREE.
func stateAfter(words []cloudcmd.Token, state shellState) (shellState, bool) {
	workTree := state.workTree.after(words)
	cwd, changed := directoryAfter(words, state.cwd)
	return shellState{cwd: cwd, workTree: workTree}, changed || workTree != state.workTree
}

// directoryAfter returns the directory a simple command leaves the shell in
// and whether it is a cd, pushd or popd. A destination the guard cannot know
// (an expansion, a missing directory, $HOME, $OLDPWD or the directory stack)
// is unknownDirectory.
func directoryAfter(words []cloudcmd.Token, cwd string) (string, bool) {
	for len(words) > 0 && (words[0].Text == "command" || words[0].Text == "builtin" || strings.Contains(words[0].Text, "=")) {
		words = words[1:]
	}
	if len(words) == 0 {
		return cwd, false
	}
	switch words[0].Text {
	case "cd", "pushd":
		args := words[1:]
		if words[0].Text == "cd" {
			for len(args) > 0 && len(args[0].Text) > 1 && args[0].Text[0] == '-' {
				option := args[0].Text
				args = args[1:]
				if option == "--" {
					break
				}
			}
		}
		if len(args) != 1 || args[0].Dynamic || args[0].Text == "-" || strings.HasPrefix(args[0].Text, "+") || words[0].Text == "pushd" && strings.HasPrefix(args[0].Text, "-") {
			return unknownDirectory, true
		}
		target := resolveTarget(args[0].Text, cwd)
		if info, err := os.Stat(target); err != nil || !info.IsDir() {
			return unknownDirectory, true
		}
		return target, true
	case "popd":
		return unknownDirectory, true
	}
	return cwd, false
}
