package cloudcmd

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Token is one shell word or control/redirection operator. Quotes and
// escapes are removed from Text; expansions are never evaluated.
type Token struct {
	Text     string
	Operator bool
	// Dynamic marks a word whose runtime value may differ from Text:
	// parameter/command expansion, unquoted glob or tilde, or an
	// unterminated quote.
	Dynamic bool

	expansion bool // $ or ` outside single quotes
	pattern   bool // unquoted glob (* ? [) or brace ({...}) syntax
	heredoc   int  // analysis lexing: 1-based index into Lexed.heredocs for << and <<-
	bodies    int  // analysis lexing: substitution bodies recorded when the token ended
}

// Lexed is a command's token stream plus whole-command syntax findings.
type Lexed struct {
	Tokens []Token
	// Unterminated reports an unclosed single or double quote.
	Unterminated bool
	// Substitution reports $(...), `...`, <(...) or >(...) anywhere outside
	// single quotes.
	Substitution bool

	// Analysis lexing only.
	bodies      []string  // command/process substitution code, in source order
	heredocs    []heredoc // here-documents, in operator order
	openHeredoc bool      // a here-document has no terminating delimiter line
}

// heredoc is one here-document body. start and end are byte offsets of the
// body in the lexed text.
type heredoc struct {
	delim      string
	strip      bool // <<- strips leading tabs from the delimiter line
	quoted     bool // quoted delimiter: the body is not expanded
	start, end int
	// literal reports that the shell passes the body through unchanged: a
	// quoted delimiter, or no $, ` or \ in the body.
	literal bool
}

func (h heredoc) body(src string) string { return src[h.start:h.end] }

const shellOperators = ";|&()<>\n"

func isBlank(c byte) bool { return c == ' ' || c == '\t' }

// Lex splits command into words and operators using POSIX-shell quoting:
// single quotes are literal, double quotes honor \ before $ ` " \ and
// newline, and an unquoted backslash escapes the next byte. A backslash-newline
// continuation is removed. Comments start with # at a word boundary. A bare
// digit word immediately before < or > is a file-descriptor prefix and is
// dropped. Lex never expands, evaluates or executes anything.
//
// Lex is the destructive guard's flat token stream: here-document bodies are
// ordinary tokens and substitutions are not delimited. [Analyze] lexes with
// here-document bodies and substitution boundaries resolved instead.
func Lex(command string) Lexed { return lex(command, false) }

func lex(command string, analysis bool) Lexed {
	l := lexer{src: command, analysis: analysis, delimiter: -1, commandStart: true}
	l.run(false)
	if len(l.pending) > 0 || l.delimiter >= 0 {
		l.out.openHeredoc = true
	}
	return l.out
}

type lexer struct {
	src      string
	i        int
	analysis bool
	out      Lexed
	// Analysis lexing: delimiter is the here-document awaiting its delimiter
	// word (-1 for none); pending ones read their bodies after the next newline.
	delimiter int
	pending   []int
	// Analysis lexing: parenthesis and case nesting decide which ) closes a
	// substitution; commandStart tracks command position for case/esac.
	parens, cases int
	commandStart  bool
}

// run lexes until the end of input or, when stop is set, until the ) that
// closes the substitution being lexed; it reports whether that ) was found.
func (l *lexer) run(stop bool) bool {
	src := l.src
	for l.i < len(src) {
		c := src[l.i]
		switch {
		case c == '\\' && l.i+1 < len(src) && src[l.i+1] == '\n':
			l.i += 2
		case isBlank(c):
			l.i++
		case c == '#':
			for l.i < len(src) && src[l.i] != '\n' {
				l.i++
			}
		case strings.IndexByte(shellOperators, c) >= 0:
			if l.operator(stop) {
				return true
			}
		default:
			l.word()
		}
	}
	return false
}

func (l *lexer) operator(stop bool) bool {
	src := l.src
	start := l.i
	c := src[l.i]
	l.i++
	next := byte(0)
	if l.i < len(src) {
		next = src[l.i]
	}
	if l.analysis {
		switch {
		case (c == '<' || c == '>') && next == '(':
			body, end, closed := commandSubstitution(src, l.i+1)
			l.substitution(body, closed)
			l.i = end
			l.emit(Token{Text: src[start:end], Dynamic: true, expansion: true})
			l.commandStart = false
			return false
		case c == '<' && next == '<':
			l.i++
			if l.i < len(src) && (src[l.i] == '<' || src[l.i] == '-') {
				l.i++
			}
		case c != '(' && c != ')' && c != '\n':
			l.i = mergeOperator(src, start, l.i)
		}
	} else {
		l.i = mergeOperator(src, start, l.i)
		if (c == '<' || c == '>') && l.i == start+1 && next == '(' {
			l.out.Substitution = true
		}
	}
	token := Token{Text: src[start:l.i], Operator: true}
	if l.analysis {
		switch token.Text {
		case "<<", "<<-":
			l.out.heredocs = append(l.out.heredocs, heredoc{strip: token.Text == "<<-"})
			l.delimiter = len(l.out.heredocs) - 1
			token.heredoc = len(l.out.heredocs)
		case "(":
			l.parens++
		case ")":
			switch {
			case l.parens > 0:
				l.parens--
			case l.cases > 0:
				// A case pattern's closing parenthesis.
			case stop:
				return true
			}
		case "\n":
			l.readHeredocs()
		}
		if !strings.ContainsAny(token.Text, "<>") {
			l.commandStart = true
		}
	}
	l.emit(token)
	return false
}

// mergeOperator extends the one-byte operator at start to the multi-byte
// operators the flat lexer has always produced.
func mergeOperator(src string, start, i int) int {
	c := src[start]
	if i < len(src) && (src[i] == c || c == '&' && src[i] == '>' || c == '>' && (src[i] == '&' || src[i] == '|')) {
		i++
		if src[start:i] == "&>" && i < len(src) && src[i] == '>' {
			i++
		}
	}
	return i
}

func (l *lexer) emit(token Token) {
	token.bodies = len(l.out.bodies)
	l.out.Tokens = append(l.out.Tokens, token)
}

func (l *lexer) substitution(body string, closed bool) {
	l.out.Substitution = true
	l.out.bodies = append(l.out.bodies, body)
	if !closed {
		l.out.Unterminated = true
	}
}

func (l *lexer) word() {
	src := l.src
	var word strings.Builder
	var token Token
	var quote byte
	// Brace syntax is opaque to cloud analysis but, as before, not Dynamic
	// for the destructive guard (find -exec ... {} stays literal).
	tilde, glob, braceOpen, brace, quoted := false, false, false, false, false
	for l.i < len(src) {
		ch := src[l.i]
		if quote == 0 && (isBlank(ch) || strings.IndexByte(shellOperators, ch) >= 0) {
			break
		}
		if ch == '\\' && quote == 0 && l.i+1 < len(src) {
			quoted = true
			if src[l.i+1] != '\n' {
				word.WriteByte(src[l.i+1])
			}
			l.i += 2
			continue
		}
		if ch == '\\' && quote == '"' && l.i+1 < len(src) {
			switch next := src[l.i+1]; next {
			case '\n':
				l.i += 2
				continue
			case '$', '`', '"', '\\':
				word.WriteByte(next)
				l.i += 2
				continue
			}
			word.WriteByte('\\')
			l.i++
			continue
		}
		if l.analysis && quote != '\'' && (ch == '`' || ch == '$' && l.i+1 < len(src) && src[l.i+1] == '(') {
			var body string
			var end int
			var closed bool
			if ch == '`' {
				body, end, closed = backquoted(src, l.i+1)
			} else {
				body, end, closed = commandSubstitution(src, l.i+2)
			}
			l.substitution(body, closed)
			token.expansion = true
			word.WriteString(src[l.i:end])
			l.i = end
			continue
		}
		switch {
		case (ch == '\'' || ch == '"') && quote == 0:
			quote = ch
			quoted = true
		case ch == quote:
			quote = 0
		default:
			if quote != '\'' && (ch == '$' || ch == '`') {
				token.expansion = true
				if ch == '`' || l.i+1 < len(src) && src[l.i+1] == '(' {
					l.out.Substitution = true
				}
			}
			if quote == 0 {
				switch ch {
				case '*', '?', '[':
					glob = true
				case '~':
					tilde = true
				case '{':
					braceOpen = true
				case '}':
					brace = brace || braceOpen
				}
			}
			word.WriteByte(ch)
		}
		l.i++
	}
	if quote != 0 {
		l.out.Unterminated = true
	}
	if l.i < len(src) && (src[l.i] == '<' || src[l.i] == '>') && allDigits(word.String()) {
		return
	}
	token.Text = word.String()
	token.Dynamic = token.expansion || tilde || glob || quote != 0
	token.pattern = glob || brace
	if l.analysis {
		switch {
		case l.delimiter >= 0:
			doc := &l.out.heredocs[l.delimiter]
			doc.delim, doc.quoted = token.Text, quoted
			l.pending = append(l.pending, l.delimiter)
			l.delimiter = -1
		case l.commandStart:
			if token.Text == "case" {
				l.cases++
			} else if token.Text == "esac" && l.cases > 0 {
				l.cases--
			}
			l.commandStart = shellKeywords[token.Text]
		}
	}
	l.emit(token)
}

// readHeredocs consumes the bodies of pending here-documents, which start
// after the newline just lexed, and records substitutions inside bodies the
// shell expands.
func (l *lexer) readHeredocs() {
	src := l.src
	for _, index := range l.pending {
		doc := &l.out.heredocs[index]
		doc.start, doc.end = l.i, len(src)
		found := false
		for l.i < len(src) {
			lineEnd, next := len(src), len(src)
			if n := strings.IndexByte(src[l.i:], '\n'); n >= 0 {
				lineEnd, next = l.i+n, l.i+n+1
			}
			line := src[l.i:lineEnd]
			if doc.strip {
				line = strings.TrimLeft(line, "\t")
			}
			if line == doc.delim {
				doc.end, found = l.i, true
				l.i = next
				break
			}
			l.i = next
		}
		if !found {
			l.out.openHeredoc = true
		}
		body := doc.body(src)
		doc.literal = doc.quoted || !strings.ContainsAny(body, "$`\\")
		if !doc.quoted {
			l.expansions(body)
		}
	}
	l.pending = l.pending[:0]
}

// expansions records the substitutions in text the shell expands outside
// any quoting context (an unquoted here-document body).
func (l *lexer) expansions(text string) {
	for i := 0; i < len(text); i++ {
		switch {
		case text[i] == '\\':
			i++
		case text[i] == '`':
			body, end, closed := backquoted(text, i+1)
			l.substitution(body, closed)
			i = end - 1
		case text[i] == '$' && i+1 < len(text) && text[i+1] == '(':
			body, end, closed := commandSubstitution(text, i+2)
			l.substitution(body, closed)
			i = end - 1
		}
	}
}

// commandSubstitution lexes the shell code that starts at src[start] up to
// its closing parenthesis and returns that code and the index after the
// parenthesis. The code is lexed with full shell rules (quotes, nested
// substitutions, here-documents, case patterns), so a ) inside them does not
// end it early.
func commandSubstitution(src string, start int) (body string, end int, closed bool) {
	sub := lexer{src: src, i: start, analysis: true, delimiter: -1, commandStart: true}
	if sub.run(true) {
		return src[start : sub.i-1], sub.i, true
	}
	return src[start:], len(src), false
}

// backquoted returns the code of a `...` substitution starting at src[start],
// with \$, \` and \\ unescaped, and the index after the closing backquote.
func backquoted(src string, start int) (body string, end int, closed bool) {
	var b strings.Builder
	for i := start; i < len(src); i++ {
		c := src[i]
		if c == '\\' && i+1 < len(src) && strings.IndexByte("$`\\", src[i+1]) >= 0 {
			b.WriteByte(src[i+1])
			i++
			continue
		}
		if c == '`' {
			return b.String(), i + 1, true
		}
		b.WriteByte(c)
	}
	return b.String(), len(src), false
}

func allDigits(text string) bool {
	if text == "" {
		return false
	}
	for i := range len(text) {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
}

// literal reports whether a word's text is exactly what the program receives.
func (t Token) literal() bool { return !t.expansion && !t.pattern && !t.Dynamic }

// maxNesting bounds substitution and inline-shell recursion; deeper code is
// opaque.
const maxNesting = 8

// Analyze classifies command's cloud intent. cwd is the directory in which the
// command starts; it is used only lexically to contain terraform -chdir.
//
// Shell constructs the analysis cannot resolve (substitutions, here-documents,
// compounds, unresolved wrappers, unterminated quotes) make the result opaque
// only when the command names a cloud tool somewhere; otherwise Effect stays
// as classified and the caller's non-cloud checks decide. A dynamic command
// word or shell code the analysis cannot see is opaque regardless, because it
// can run a cloud tool without naming it.
func Analyze(command, cwd string) Analysis {
	a := analyzer{effect: EffectNone, proven: true, dir: newDirState(cwd)}
	a.code(command)
	return a.result()
}

type analyzer struct {
	effect      Effect
	cloud       bool
	invocations []Invocation
	reasons     []string
	simpleCount int
	envMutated  bool
	dir         dirState
	// unresolved: shell syntax the analysis could not resolve; it makes the
	// result opaque only when named reports a cloud tool name in the text.
	unresolved, named bool
	// proven is false for any construct that is not direct, resolved syntax,
	// including wrappers the analysis looked through; plan mode requires it.
	proven bool
	// wrapped is set while analyzing code reached through a substitution,
	// inline shell or eval: such invocations are never grantable.
	wrapped bool
	depth   int
}

// stdinSource is what a simple command reads on standard input, as far as the
// command text shows.
type stdinSource struct {
	kind stdinKind
	// code is a here-document body or here-string word as a shell would
	// read it; Dynamic when the shell expands it first.
	code Token
}

type stdinKind uint8

const (
	stdinInherited stdinKind = iota
	stdinPipe
	stdinFile
	stdinHeredoc
	stdinString
)

// invocationContext carries what the surrounding syntax implies for one
// simple command.
type invocationContext struct {
	prev, next string
	stdin      stdinSource
	// envChanged: assignments, env options or shell startup can change tool
	// configuration.
	envChanged bool
	// wrapped: reached through a wrapper; never grantable.
	wrapped bool
	// appended: xargs or find -exec appends arguments at run time.
	appended bool
}

// code analyzes one complete piece of shell code: the command itself, a
// substitution body or an inline shell string.
func (a *analyzer) code(command string) {
	if a.depth >= maxNesting {
		a.opaque(Invocation{Effect: EffectOpaque, cluster: true}, "shell code nested too deeply")
		return
	}
	a.depth++
	defer func() { a.depth-- }()
	if len(command) > MaxCommandBytes {
		a.opaque(Invocation{Effect: EffectOpaque, cluster: true}, "command exceeds 64 KiB")
		return
	}
	if strings.IndexByte(command, 0) >= 0 || !utf8.ValidString(command) {
		a.opaque(Invocation{Effect: EffectOpaque, cluster: true}, "command contains NUL or invalid UTF-8")
		return
	}
	lexed := lex(command, true)
	if len(lexed.Tokens) > MaxTokens {
		a.opaque(Invocation{Effect: EffectOpaque, cluster: true}, "command exceeds 4096 tokens")
		return
	}
	if lexed.Unterminated {
		a.syntax("unterminated quote or substitution")
	}
	if lexed.openHeredoc {
		a.syntax("unterminated here-document")
	}
	if lexed.Substitution {
		a.syntax("command or process substitution")
	}
	data := a.commands(command, lexed)
	scan := command
	for index := len(lexed.heredocs) - 1; index >= 0; index-- {
		if doc := lexed.heredocs[index]; data[index] && doc.end > doc.start {
			scan = scan[:doc.start] + scan[doc.end:]
		}
	}
	if namesCloudTool(scan) {
		a.named = true
	}
}

// commands analyzes the simple commands of lexed code and their substitution
// bodies, and reports which here-documents feed a data sink (cat or tee):
// those bodies are data, every other body is searched for cloud tool names.
func (a *analyzer) commands(command string, lexed Lexed) (data []bool) {
	data = make([]bool, len(lexed.heredocs))
	tokens := lexed.Tokens
	var words []Token
	var docs []int
	stdin := stdinSource{}
	prev := ""
	analyzed := 0
	flush := func(next string, bodies int) {
		for ; analyzed < bodies; analyzed++ {
			a.nested(lexed.bodies[analyzed])
		}
		if len(words) > 0 {
			scope := invocationContext{prev: prev, next: next, stdin: stdin}
			if stdin.kind == stdinInherited && (prev == "|" || prev == "|&") {
				scope.stdin.kind = stdinPipe
			}
			if dataSink(words) {
				for _, index := range docs {
					data[index] = true
				}
			}
			a.simple(words, scope)
		}
		words, docs, stdin, prev = nil, nil, stdinSource{}, next
	}
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if !token.Operator {
			words = append(words, token)
			continue
		}
		switch text := token.Text; {
		case text == "<<" || text == "<<-":
			a.syntax("here-document")
			if i+1 < len(tokens) && !tokens[i+1].Operator {
				i++
			}
			if token.heredoc > 0 {
				doc := lexed.heredocs[token.heredoc-1]
				docs = append(docs, token.heredoc-1)
				stdin = stdinSource{kind: stdinHeredoc, code: Token{Text: doc.body(command), Dynamic: !doc.literal}}
			}
		case text == "<<<":
			a.syntax("here-string")
			if i+1 >= len(tokens) || tokens[i+1].Operator {
				a.syntax("incomplete redirection")
				continue
			}
			i++
			stdin = stdinSource{kind: stdinString, code: tokens[i]}
		case strings.ContainsAny(text, "<>"):
			if text == "<" && i+1 < len(tokens) && tokens[i+1].Text == "&" && tokens[i+1].Operator {
				i++
			}
			if i+1 >= len(tokens) || tokens[i+1].Operator {
				a.syntax("incomplete redirection")
				continue
			}
			i++
			if strings.HasPrefix(text, "<") {
				stdin = stdinSource{kind: stdinFile}
			}
		case text == "(" || text == ")":
			a.syntax("subshell or grouping")
			flush(text, token.bodies)
		case text == ";;":
			a.syntax("case clause")
			flush(text, token.bodies)
		default:
			flush(text, token.bodies)
		}
	}
	flush("", len(lexed.bodies))
	return data
}

// dataSink reports a command that only copies its standard input (the
// cat/tee file-writing idiom), so a here-document it reads cannot run code.
func dataSink(words []Token) bool {
	for len(words) > 0 && isAssignment(words[0].Text) {
		words = words[1:]
	}
	if len(words) == 0 || !words[0].literal() {
		return false
	}
	name := filepath.Base(words[0].Text)
	return name == "cat" || name == "tee"
}

// nested analyzes a command substitution body, which runs in a subshell.
func (a *analyzer) nested(body string) {
	dir, wrapped := a.dir, a.wrapped
	a.wrapped = true
	a.code(body)
	a.dir, a.wrapped = dir, wrapped
}

// inline analyzes shell code run by eval, trap or a shell's -c string or
// stdin. envChanged marks an environment the code's cloud tools may not
// trust. sameShell code (eval, trap) can change the directory of later
// commands; a child shell's cannot.
func (a *analyzer) inline(code string, envChanged, sameShell bool) {
	dir, wrapped := a.dir, a.wrapped
	if envChanged {
		a.envMutated = true
	}
	a.wrapped = true
	a.code(code)
	changed := a.dir != dir
	a.dir, a.wrapped = dir, wrapped
	if sameShell && changed {
		a.dir.known = false
	}
}

// syntax records a construct the analysis cannot resolve.
func (a *analyzer) syntax(reason string) {
	a.unresolved, a.proven = true, false
	a.addReason("unresolved shell syntax: " + reason)
}

// indirect records a construct the analysis resolved by looking through it
// (a wrapper or inline shell code). The effect is the inner command's, but
// the command is no longer plain syntax for plan mode.
func (a *analyzer) indirect(reason string) {
	a.proven = false
	a.addReason(reason)
}

// opaque records code the analysis cannot see: it may run a cloud tool
// without naming it.
func (a *analyzer) opaque(inv Invocation, reason string) {
	a.proven = false
	a.add(inv, reason)
}

func (a *analyzer) addReason(reason string) {
	for _, existing := range a.reasons {
		if existing == reason {
			return
		}
	}
	a.reasons = append(a.reasons, reason)
}

func (a *analyzer) result() Analysis {
	effect := a.effect
	if a.unresolved && (a.named || a.cloud) {
		effect = Join(effect, EffectOpaque)
		a.addReason("a cloud tool is named inside shell syntax the analysis cannot resolve")
	}
	return Analysis{Effect: effect, Cloud: a.cloud, Proven: a.proven, Invocations: a.invocations, Reason: clip(strings.Join(a.reasons, "; "))}
}

func (a *analyzer) add(inv Invocation, reason string) {
	a.effect = Join(a.effect, inv.Effect)
	if len(a.invocations) < MaxInvocations {
		a.invocations = append(a.invocations, inv)
	}
	text := describe(inv) + ": " + string(inv.Effect)
	if reason != "" {
		text += " (" + reason + ")"
	}
	a.addReason(text)
}

func describe(inv Invocation) string {
	text := inv.Tool
	if text == "" {
		text = "command"
	}
	if inv.Verb != "" {
		text += " " + inv.Verb
	}
	if inv.Context != "" {
		text += fmt.Sprintf(" [context %q]", bounded(inv.Context))
	}
	return text
}

func clip(text string) string {
	if len(text) <= maxReasonBytes {
		return text
	}
	cut := maxReasonBytes - len("...")
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "..."
}

var shellKeywords = map[string]bool{
	"{": true, "}": true, "!": true, "[[": true, "]]": true, "if": true, "then": true, "elif": true, "else": true, "fi": true,
	"for": true, "while": true, "until": true, "do": true, "done": true, "case": true, "esac": true, "in": true,
	"select": true, "function": true, "coproc": true,
}

// envBuiltins change variables or options seen by later commands.
var envBuiltins = map[string]bool{
	"export": true, "declare": true, "typeset": true, "readonly": true, "local": true, "unset": true, "set": true,
}

func isAssignment(text string) bool {
	eq := strings.IndexByte(text, '=')
	if eq <= 0 {
		return false
	}
	for i := range eq {
		c := text[i]
		if c != '_' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func (a *analyzer) simple(words []Token, scope invocationContext) {
	a.simpleCount++
	if a.simpleCount == MaxInvocations+1 {
		a.syntax("more than 64 simple commands")
	}
	for len(words) > 0 && isAssignment(words[0].Text) {
		scope.envChanged = true
		words = words[1:]
	}
	if len(words) == 0 {
		// A bare assignment changes the environment of later commands when
		// the variable is already exported (KUBECONFIG, TF_CLI_ARGS, ...).
		a.envMutated = true
		return
	}
	exe := words[0]
	// "[" and "[[" are unquoted brackets but name the test command, not a glob.
	if exe.expansion || exe.pattern && exe.Text != "[" && exe.Text != "[[" || exe.Text == "" {
		a.opaque(Invocation{Effect: EffectOpaque, cluster: true}, "dynamic executable")
		return
	}
	name := filepath.Base(exe.Text)
	args := words[1:]
	switch {
	case shellKeywords[exe.Text]:
		// The compound is unresolved, but the command it introduces is still
		// classified so its literal targets reach deny rules.
		a.syntax("shell compound command")
		if len(args) > 0 {
			a.simple(args, scope)
		}
		return
	case name == "cd":
		a.dir.cd(args, scope.prev, scope.next)
		return
	case envBuiltins[name]:
		a.envMutated = true
		return
	case name == "find":
		a.find(args, scope)
		return
	case wrappers[name]:
		a.wrapper(name, args, scope)
		return
	case shells[name]:
		a.shell(name, args, scope)
		return
	}
	var inv Invocation
	var reason string
	switch name {
	case ToolKubectl:
		inv, reason = classifyKubectl(args)
	case ToolHelm:
		inv, reason = classifyHelm(args)
	case ToolTerraform:
		inv, reason = classifyTerraform(args, &a.dir)
	case ToolKustomize:
		inv, reason = classifyKustomize(args)
	default:
		return
	}
	a.cloud = true
	inv.Tool = name
	switch {
	case scope.envChanged || a.envMutated:
		inv.Effect, inv.ExplicitContext = EffectOpaque, false
		reason = "environment assignment can change tool configuration"
	case scope.appended:
		inv.Effect, inv.ExplicitContext = EffectOpaque, false
		reason = "arguments are appended at run time"
	}
	if scope.wrapped || a.wrapped {
		inv.grantable = false
	}
	a.add(inv, reason)
}

// dirState tracks the directory commands run in, purely lexically. A cd
// whose success or scope is uncertain makes the directory unknown.
type dirState struct {
	root  string // session cwd; "" means relative-only containment
	cur   string
	known bool
}

func newDirState(cwd string) dirState {
	if cwd == "" {
		return dirState{cur: ".", known: true}
	}
	clean := filepath.Clean(cwd)
	return dirState{root: clean, cur: clean, known: true}
}

func (d *dirState) cd(args []Token, prev, next string) {
	if len(args) != 1 || !args[0].literal() || args[0].Text == "-" || next != "&&" || (prev != "" && prev != ";" && prev != "\n" && prev != "\n\n" && prev != "&&") {
		d.known = false
		return
	}
	d.cur = d.resolve(args[0].Text)
}

func (d *dirState) resolve(dir string) string {
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	return filepath.Join(d.cur, dir)
}

// contains reports whether dir, resolved from the current directory, stays
// inside the session root.
func (d *dirState) contains(dir string) bool {
	if !d.known {
		return false
	}
	if d.root == "" {
		if filepath.IsAbs(dir) {
			return false
		}
		resolved := d.resolve(dir)
		return resolved != ".." && !strings.HasPrefix(resolved, "../")
	}
	rel, err := filepath.Rel(d.root, d.resolve(dir))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}
