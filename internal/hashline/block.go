package hashline

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
)

// Block locators (`N*`) resolve the construct opening on line N without
// tree-sitter:
//
//   - Go (.go): go/parser. Declarations, statements, case clauses, composite
//     literals and multi-line expressions starting at the line's first token.
//     A doc comment opening on N sweeps the declaration it documents.
//   - Markdown (.md, .markdown, .mdx): an ATX heading spans its section
//     through deeper headings; a code fence spans to its closing fence.
//   - Brace languages (JavaScript, TypeScript, Java, C, C++, Rust, C#,
//     Kotlin, Swift, PHP, Scala, JSON): bracket matching that skips strings
//     and comments. Annotations/attributes and doc comments (`/**`, `///`)
//     opening on N sweep the construct they precede.
//   - Python and YAML: indentation. Python decorators sweep the decorated
//     definition and if/try/for/while include their else/elif/except/finally
//     clauses.
//
// Standalone (non-doc) comments are never openers.

// blockLanguages names the supported set for error messages.
const blockLanguages = "Go, Markdown, JavaScript/TypeScript, Java, C/C++, Rust, C#, Kotlin, Swift, PHP, Scala, JSON, Python, YAML"

// resolver returns the last line of the construct opening on line, or a
// problem describing why line opens no construct.
type resolver func(line int) (end int, problem string)

// newResolver picks the block resolver for path's language. ok is false for
// an unsupported language.
func newResolver(path string, text string, lines []string) (resolver, bool) {
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".go":
		return goResolver(path, text, lines), true
	case ".md", ".markdown", ".mdx":
		return markdownResolver(lines), true
	case ".py", ".pyi":
		return pythonResolver(lines), true
	case ".yaml", ".yml":
		return yamlResolver(lines), true
	default:
		if lang, ok := braceLanguages[ext]; ok {
			return braceResolver(lang, lines), true
		}
		return nil, false
	}
}

func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " \t"))
}

func isCloser(c byte) bool { return c == '}' || c == ')' || c == ']' }

func blankProblem(line int) string { return fmt.Sprintf("line %d is blank", line) }

func closerProblem(line int) string {
	return fmt.Sprintf("line %d starts with a closing delimiter, which ends a construct rather than opening one", line)
}

func commentProblem(line int) string {
	return fmt.Sprintf("line %d is a standalone comment; block ops never sweep standalone comments", line)
}

// ---- Go ----------------------------------------------------------------

func goResolver(path, text string, lines []string) resolver {
	var fset *token.FileSet
	var file *ast.File
	var parseErr error
	parsed := false
	return func(line int) (int, string) {
		if !parsed {
			parsed = true
			fset = token.NewFileSet()
			file, parseErr = parser.ParseFile(fset, path, text, parser.ParseComments|parser.SkipObjectResolution)
		}
		if parseErr != nil {
			msg := parseErr.Error()
			if list, ok := parseErr.(scanner.ErrorList); ok && len(list) > 0 {
				msg = list[0].Error()
			}
			return 0, "the Go file does not parse (" + msg + ")"
		}
		raw := lines[line-1]
		if strings.TrimSpace(raw) == "" {
			return 0, blankProblem(line)
		}
		col := indentOf(raw)
		if isCloser(raw[col]) {
			return 0, closerProblem(line)
		}
		tf := fset.File(file.Pos())
		pos := tf.LineStart(line) + token.Pos(col)
		endLine := func(n ast.Node) int { return tf.Line(n.End() - 1) }
		for _, group := range file.Comments {
			if group.Pos() != pos {
				continue
			}
			if owner := docOwner(file, group); owner != nil {
				return endLine(owner), ""
			}
			return 0, commentProblem(line)
		}
		var best ast.Node
		ast.Inspect(file, func(n ast.Node) bool {
			if n == nil || n.Pos() > pos || n.End() <= pos {
				return false
			}
			switch n.(type) {
			case *ast.File, *ast.CommentGroup, *ast.Comment:
				return true
			}
			if n.Pos() == pos && (best == nil || n.End() > best.End()) {
				best = n
			}
			return true
		})
		if best == nil {
			return 0, fmt.Sprintf("line %d does not begin a Go declaration, statement or expression", line)
		}
		return endLine(best), ""
	}
}

// docOwner returns the declaration, spec or field group documents.
func docOwner(file *ast.File, group *ast.CommentGroup) ast.Node {
	var owner ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if owner != nil || n == nil {
			return false
		}
		var doc *ast.CommentGroup
		switch d := n.(type) {
		case *ast.FuncDecl:
			doc = d.Doc
		case *ast.GenDecl:
			doc = d.Doc
		case *ast.TypeSpec:
			doc = d.Doc
		case *ast.ValueSpec:
			doc = d.Doc
		case *ast.ImportSpec:
			doc = d.Doc
		case *ast.Field:
			doc = d.Doc
		}
		if doc == group {
			owner = n
			return false
		}
		// A documented node starts after its doc comment, so subtrees that
		// end before the comment hold no owner.
		return n.End() > group.Pos()
	})
	return owner
}

// ---- Markdown ----------------------------------------------------------

func markdownResolver(lines []string) resolver {
	// fenceEnd maps an opening fence line (0-based) to its closing line
	// (1-based); inFence marks the lines after an opener through its closer.
	fenceEnd := map[int]int{}
	inFence := make([]bool, len(lines))
	for i := 0; i < len(lines); i++ {
		marker := fenceMarker(lines[i])
		if marker == "" {
			continue
		}
		j := i + 1
		for j < len(lines) && !closesFence(lines[j], marker) {
			inFence[j] = true
			j++
		}
		fenceEnd[i] = min(j+1, len(lines))
		if j < len(lines) {
			inFence[j] = true
		}
		i = j
	}
	return func(line int) (int, string) {
		raw := lines[line-1]
		if strings.TrimSpace(raw) == "" {
			return 0, blankProblem(line)
		}
		if end, ok := fenceEnd[line-1]; ok {
			return end, ""
		}
		if inFence[line-1] {
			return 0, fmt.Sprintf("line %d is inside a fenced code block; only the opening fence line opens a block", line)
		}
		level := headingLevel(raw)
		if level == 0 {
			return 0, fmt.Sprintf("line %d is not a Markdown heading or code fence; only those open blocks in Markdown", line)
		}
		for k := line + 1; k <= len(lines); k++ {
			if l := headingLevel(lines[k-1]); l > 0 && l <= level && !inFence[k-1] {
				return k - 1, ""
			}
		}
		return len(lines), ""
	}
}

func fenceMarker(line string) string {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return ""
	}
	for _, c := range []byte{'`', '~'} {
		n := 0
		for n < len(trimmed) && trimmed[n] == c {
			n++
		}
		if n >= 3 {
			return trimmed[:n]
		}
	}
	return ""
}

func closesFence(line, open string) bool {
	marker := fenceMarker(line)
	return marker != "" && marker[0] == open[0] && len(marker) >= len(open) && strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), marker[:1])) == ""
}

func headingLevel(line string) int {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return 0
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n < len(trimmed) && trimmed[n] != ' ' && trimmed[n] != '\t' {
		return 0
	}
	return n
}

// ---- brace languages ---------------------------------------------------

// braceLang configures the lexer for one family of brace languages.
type braceLang struct {
	singleQuoteStrings bool // ' delimits strings (JS, TS, PHP)
	charLiterals       bool // 'x' is a char literal; other ' are plain (lifetimes)
	templates          bool // ` template literals with ${} (JS, TS)
	tripleQuotes       bool // """ text blocks (Java, Kotlin, Swift, Scala, C#)
	nestedComments     bool // /* /* */ */ (Rust, Swift, Kotlin, Scala)
	hashComments       bool // # line comments (PHP), except #[
	rawStrings         bool // r"..", r#".."# (Rust)
	cppRawStrings      bool // R"d(..)d" (C++)
	verbatimStrings    bool // @".." (C#)
	attributes         string
}

const (
	attrAt      = "@"  // @Annotation
	attrHash    = "#[" // #[attribute]
	attrBracket = "["  // [Attribute] (C#)
)

var (
	jsLang    = braceLang{singleQuoteStrings: true, templates: true, attributes: attrAt}
	javaLang  = braceLang{charLiterals: true, tripleQuotes: true, attributes: attrAt}
	cLang     = braceLang{charLiterals: true}
	cppLang   = braceLang{charLiterals: true, cppRawStrings: true}
	rustLang  = braceLang{charLiterals: true, nestedComments: true, rawStrings: true, attributes: attrHash}
	csLang    = braceLang{charLiterals: true, tripleQuotes: true, verbatimStrings: true, attributes: attrBracket}
	ktLang    = braceLang{charLiterals: true, tripleQuotes: true, nestedComments: true, attributes: attrAt}
	swiftLang = braceLang{tripleQuotes: true, nestedComments: true, attributes: attrAt}
	phpLang   = braceLang{singleQuoteStrings: true, hashComments: true, attributes: attrHash}
	jsonLang  = braceLang{}
)

var braceLanguages = map[string]braceLang{
	".js": jsLang, ".jsx": jsLang, ".mjs": jsLang, ".cjs": jsLang,
	".ts": jsLang, ".tsx": jsLang, ".mts": jsLang, ".cts": jsLang,
	".java": javaLang, ".scala": ktLang, ".sc": ktLang,
	".c": cLang, ".h": cLang,
	".cc": cppLang, ".cpp": cppLang, ".cxx": cppLang, ".hpp": cppLang, ".hh": cppLang, ".hxx": cppLang,
	".rs": rustLang, ".cs": csLang, ".kt": ktLang, ".kts": ktLang,
	".swift": swiftLang, ".php": phpLang,
	".json": jsonLang, ".jsonc": jsonLang, ".json5": jsonLang,
}

// lexedLine is one line after lexing: code keeps only code bytes (string
// and comment bytes become spaces), inside marks a line that begins inside
// a multi-line string or comment, and comment describes a comment that
// begins at the line's first non-blank byte.
type lexedLine struct {
	code    string
	inside  bool
	comment *commentInfo
}

type commentInfo struct {
	doc bool
	end int // 1-based last line of the comment (a /// run spans lines)
}

// lexBrace masks strings and comments out of lines.
func lexBrace(lang braceLang, lines []string) []lexedLine {
	out := make([]lexedLine, len(lines))
	type mode int
	const (
		code mode = iota
		blockComment
		str
	)
	state := code
	depth := 0          // block comment nesting
	closing := ""       // terminator of the current string
	escapes := true     // backslash escapes in the current string
	var templates []int // brace depth stack for ${ } inside template literals
	var commentStart *commentInfo
	for i, line := range lines {
		out[i].inside = state != code
		buf := []byte(line)
		first := indentOf(line)
		for j := 0; j < len(buf); {
			switch state {
			case blockComment:
				if lang.nestedComments && strings.HasPrefix(line[j:], "/*") {
					depth++
					buf[j], buf[j+1] = ' ', ' '
					j += 2
					continue
				}
				if strings.HasPrefix(line[j:], "*/") {
					buf[j], buf[j+1] = ' ', ' '
					j += 2
					depth--
					if depth == 0 {
						state = code
						if commentStart != nil {
							commentStart.end = i + 1
							commentStart = nil
						}
					}
					continue
				}
				buf[j] = ' '
				j++
				continue
			case str:
				if escapes && line[j] == '\\' && j+1 < len(buf) {
					buf[j], buf[j+1] = ' ', ' '
					j += 2
					continue
				}
				if closing == "`" && strings.HasPrefix(line[j:], "${") {
					templates = append(templates, 0)
					buf[j], buf[j+1] = ' ', ' '
					state = code
					j += 2
					continue
				}
				if strings.HasPrefix(line[j:], closing) {
					for k := range len(closing) {
						buf[j+k] = ' '
					}
					j += len(closing)
					state = code
					continue
				}
				buf[j] = ' '
				j++
				continue
			}
			c := line[j]
			rest := line[j:]
			switch {
			case strings.HasPrefix(rest, "//") || lang.hashComments && c == '#' && !strings.HasPrefix(rest, "#["):
				if j == first {
					doc := strings.HasPrefix(rest, "///") || strings.HasPrefix(rest, "//!")
					out[i].comment = &commentInfo{doc: doc, end: i + 1}
				}
				for k := j; k < len(buf); k++ {
					buf[k] = ' '
				}
				j = len(buf)
			case strings.HasPrefix(rest, "/*"):
				if j == first {
					commentStart = &commentInfo{doc: strings.HasPrefix(rest, "/**") && !strings.HasPrefix(rest, "/**/"), end: i + 1}
					out[i].comment = commentStart
				}
				state, depth = blockComment, 1
				buf[j], buf[j+1] = ' ', ' '
				j += 2
			case len(templates) > 0 && c == '{':
				templates[len(templates)-1]++
				j++
			case len(templates) > 0 && c == '}' && templates[len(templates)-1] == 0:
				templates = templates[:len(templates)-1]
				buf[j] = ' '
				state, closing, escapes = str, "`", true
				j++
			case len(templates) > 0 && c == '}':
				templates[len(templates)-1]--
				j++
			case lang.tripleQuotes && strings.HasPrefix(rest, `"""`):
				state, closing, escapes = str, `"""`, true
				buf[j], buf[j+1], buf[j+2] = ' ', ' ', ' '
				j += 3
			case lang.verbatimStrings && (strings.HasPrefix(rest, `@"`) || strings.HasPrefix(rest, `$@"`) || strings.HasPrefix(rest, `@$"`)):
				n := strings.IndexByte(rest, '"') + 1
				for k := range n {
					buf[j+k] = ' '
				}
				state, closing, escapes = str, `"`, false
				j += n
			case lang.rawStrings && (j == 0 || !isIdentByte(line[j-1])) && isRustRawStart(rest):
				n, hashes := rustRawPrefix(rest)
				for k := range n {
					buf[j+k] = ' '
				}
				state, closing, escapes = str, `"`+strings.Repeat("#", hashes), false
				j += n
			case lang.cppRawStrings && (j == 0 || !isIdentByte(line[j-1])) && strings.HasPrefix(rest, `R"`) && strings.Contains(rest, "("):
				open := strings.IndexByte(rest, '(')
				delim := rest[2:open]
				for k := 0; k <= open; k++ {
					buf[j+k] = ' '
				}
				state, closing, escapes = str, ")"+delim+`"`, false
				j += open + 1
			case c == '"' || c == '`' && lang.templates || c == '\'' && lang.singleQuoteStrings:
				buf[j] = ' '
				state, closing, escapes = str, string(c), true
				j++
			case c == '\'' && lang.charLiterals:
				if n := charLiteralLen(rest); n > 0 {
					for k := range n {
						buf[j+k] = ' '
					}
					j += n
				} else {
					j++ // a lifetime or label quote
				}
			default:
				j++
			}
		}
		out[i].code = string(buf)
		if state == str && !strings.HasPrefix(closing, `"""`) && closing != "`" && escapes && !strings.HasSuffix(line, "\\") {
			// Ordinary string literals end at the line break.
			state = code
		}
	}
	if commentStart != nil {
		commentStart.end = len(lines)
	}
	// Merge /// and //! runs into one doc comment spanning the run.
	for i := 0; i < len(out); i++ {
		c := out[i].comment
		if c == nil || !c.doc || c.end != i+1 || !strings.HasPrefix(strings.TrimSpace(lines[i]), "//") {
			continue
		}
		k := i
		for k+1 < len(out) && out[k+1].comment != nil && out[k+1].comment.doc && strings.HasPrefix(strings.TrimSpace(lines[k+1]), "//") {
			k++
		}
		c.end = k + 1
		i = k
	}
	return out
}

func isRustRawStart(s string) bool {
	s = strings.TrimPrefix(s, "b")
	if !strings.HasPrefix(s, "r") {
		return false
	}
	s = strings.TrimLeft(s[1:], "#")
	return strings.HasPrefix(s, `"`)
}

func rustRawPrefix(s string) (n, hashes int) {
	if s[0] == 'b' {
		n++
	}
	n++ // r
	for s[n] == '#' {
		n++
		hashes++
	}
	return n + 1, hashes
}

// charLiteralLen returns the length of a char literal at s ('x', '\n',
// '\u{1F600}', '\”), or 0 when the quote starts no char literal.
func charLiteralLen(s string) int {
	if len(s) < 3 {
		return 0
	}
	if s[1] == '\\' {
		if end := strings.IndexByte(s[3:], '\''); end >= 0 && end <= 10 {
			return end + 4
		}
		return 0
	}
	for size := 1; size <= 4 && size+1 < len(s); size++ {
		if s[size+1] == '\'' {
			if size == 1 || s[1] >= 0x80 {
				return size + 2
			}
			return 0
		}
	}
	return 0
}

// continuation reports whether the next code line, after a construct that
// closed its brackets on a line ending in prev, still belongs to it (else
// branches, catch clauses, chained calls, Allman braces).
func continuation(next, prev string) bool {
	t := strings.TrimSpace(next)
	if strings.HasPrefix(t, "{") {
		return !strings.HasSuffix(prev, "}")
	}
	for _, prefix := range []string{".", "?.", "->", "=>", ":"} {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	for _, word := range []string{"else", "catch", "finally", "where", "throws"} {
		if strings.HasPrefix(t, word) && (len(t) == len(word) || !isIdentByte(t[len(word)])) {
			return true
		}
	}
	return false
}

// trailingOperator reports a line that ends mid-expression.
func trailingOperator(code string) bool {
	t := strings.TrimSpace(code)
	for _, suffix := range []string{"=", "=>", "->", "&&", "||", "?", "+", "."} {
		if strings.HasSuffix(t, suffix) && !strings.HasSuffix(t, "==") && !strings.HasSuffix(t, "++") {
			return true
		}
	}
	return false
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func braceResolver(lang braceLang, lines []string) resolver {
	var lexed []lexedLine
	var construct func(line int, depth int) (int, string)
	construct = func(line int, hops int) (int, string) {
		if line > len(lines) {
			return 0, "the file ends before a construct begins"
		}
		raw := lines[line-1]
		if strings.TrimSpace(raw) == "" {
			return 0, blankProblem(line)
		}
		l := lexed[line-1]
		if l.inside {
			return 0, fmt.Sprintf("line %d is inside a multi-line comment or string", line)
		}
		if c := l.comment; c != nil {
			if !c.doc {
				return 0, commentProblem(line)
			}
			if hops > 8 {
				return 0, "too many doc comments and annotations in a row"
			}
			end, problem := construct(c.end+1, hops+1)
			if problem != "" {
				return 0, fmt.Sprintf("the doc comment at line %d is not followed by a construct (%s)", line, problem)
			}
			return end, ""
		}
		code := l.code
		first := strings.TrimSpace(code)
		if first == "" {
			return 0, fmt.Sprintf("line %d has no code", line)
		}
		if isCloser(first[0]) {
			return 0, closerProblem(line)
		}
		if attrEnd, ok := annotationEnd(lang, lexed, line); ok {
			if hops > 8 {
				return 0, "too many doc comments and annotations in a row"
			}
			end, problem := construct(attrEnd+1, hops+1)
			if problem != "" {
				return 0, fmt.Sprintf("the annotation at line %d is not followed by a construct (%s)", line, problem)
			}
			return end, ""
		}
		depth := 0
		opened := false
		for k := line; k <= len(lines); k++ {
			code := lexed[k-1].code
			for i := range len(code) {
				switch code[i] {
				case '{', '(', '[':
					depth++
					opened = true
				case '}', ')', ']':
					depth--
					if depth < 0 {
						return 0, fmt.Sprintf("line %d closes a bracket opened before line %d, so line %d sits inside a construct rather than opening one", k, line, line)
					}
				}
			}
			if depth > 0 {
				continue
			}
			prev := strings.TrimSpace(code)
			if !strings.HasSuffix(prev, ";") && !strings.HasSuffix(prev, ",") {
				if next := nextCodeLine(lexed, k); next > 0 && (trailingOperator(prev) || continuation(lexed[next-1].code, prev)) {
					k = next - 1
					continue
				}
			}
			if !opened && k == line {
				return line, ""
			}
			return k, ""
		}
		return 0, fmt.Sprintf("the brackets opened from line %d never close", line)
	}
	return func(line int) (int, string) {
		if lexed == nil {
			lexed = lexBrace(lang, lines)
		}
		return construct(line, 0)
	}
}

// nextCodeLine returns the next line after k with code, or 0.
func nextCodeLine(lexed []lexedLine, k int) int {
	for n := k + 1; n <= len(lexed); n++ {
		if strings.TrimSpace(lexed[n-1].code) != "" {
			return n
		}
		if lexed[n-1].comment == nil && !lexed[n-1].inside {
			return 0 // a blank line ends the construct
		}
	}
	return 0
}

// annotationEnd reports whether line holds only annotations/attributes and
// returns the line they end on.
func annotationEnd(lang braceLang, lexed []lexedLine, line int) (int, bool) {
	if lang.attributes == "" {
		return 0, false
	}
	code := lexed[line-1].code
	t := strings.TrimLeft(code, " \t")
	if !strings.HasPrefix(t, lang.attributes) {
		return 0, false
	}
	if lang.attributes == attrBracket && (len(t) < 2 || !isIdentByte(t[1])) {
		return 0, false
	}
	k, i := line, len(code)-len(t)
	for {
		// One annotation: sigil, name, optional bracketed arguments.
		switch lang.attributes {
		case attrAt:
			i++
			for i < len(lexed[k-1].code) && (isIdentByte(lexed[k-1].code[i]) || lexed[k-1].code[i] == '.') {
				i++
			}
			if i < len(lexed[k-1].code) && lexed[k-1].code[i] == '(' {
				var ok bool
				if k, i, ok = matchBracket(lexed, k, i); !ok {
					return 0, false
				}
			}
		default:
			if lang.attributes == attrHash {
				i++
			}
			var ok bool
			if k, i, ok = matchBracket(lexed, k, i); !ok {
				return 0, false
			}
		}
		rest := strings.TrimSpace(lexed[k-1].code[i:])
		if rest == "" {
			return k, true
		}
		if !strings.HasPrefix(rest, lang.attributes) {
			return 0, false // the construct follows on the same line
		}
		i = len(lexed[k-1].code) - len(strings.TrimLeft(lexed[k-1].code[i:], " \t"))
	}
}

// matchBracket returns the position just past the bracket matching the one
// at (k, i).
func matchBracket(lexed []lexedLine, k, i int) (int, int, bool) {
	depth := 0
	for ; k <= len(lexed); k, i = k+1, 0 {
		code := lexed[k-1].code
		for ; i < len(code); i++ {
			switch code[i] {
			case '{', '(', '[':
				depth++
			case '}', ')', ']':
				depth--
				if depth == 0 {
					return k, i + 1, true
				}
			}
		}
	}
	return 0, 0, false
}

// ---- Python ------------------------------------------------------------

func pythonResolver(lines []string) resolver {
	// start[i]: line i+1 begins a logical line (bracket depth 0, not inside
	// a string).
	start := make([]bool, len(lines))
	depth := 0
	closing := ""
	for i, line := range lines {
		start[i] = depth == 0 && closing == ""
		for j := 0; j < len(line); j++ {
			if closing != "" {
				if line[j] == '\\' {
					j++
					continue
				}
				if strings.HasPrefix(line[j:], closing) {
					j += len(closing) - 1
					closing = ""
				}
				continue
			}
			switch c := line[j]; c {
			case '#':
				j = len(line)
			case '"', '\'':
				if strings.HasPrefix(line[j:], strings.Repeat(string(c), 3)) {
					closing = strings.Repeat(string(c), 3)
					j += 2
				} else {
					closing = string(c)
				}
			case '(', '[', '{':
				depth++
			case ')', ']', '}':
				if depth > 0 {
					depth--
				}
			}
		}
		if len(closing) == 1 && !strings.HasSuffix(line, "\\") {
			closing = "" // single-quoted strings end at the line break
		}
	}
	isComment := func(i int) bool { return strings.HasPrefix(strings.TrimSpace(lines[i]), "#") }
	isBlank := func(i int) bool { return strings.TrimSpace(lines[i]) == "" }
	var construct func(line int) (int, string)
	construct = func(line int) (int, string) {
		i := line - 1
		if isBlank(i) {
			return 0, blankProblem(line)
		}
		if !start[i] {
			return 0, fmt.Sprintf("line %d continues the statement above it", line)
		}
		if isComment(i) {
			return 0, commentProblem(line)
		}
		indent := indentOf(lines[i])
		head := strings.TrimSpace(lines[i])
		if isCloser(head[0]) {
			return 0, closerProblem(line)
		}
		// The logical line itself.
		end := i
		for end+1 < len(lines) && !start[end+1] {
			end++
		}
		if strings.HasPrefix(head, "@") {
			next := end + 1
			if next >= len(lines) || isBlank(next) || indentOf(lines[next]) != indent {
				return 0, fmt.Sprintf("the decorator at line %d is not followed by a definition", line)
			}
			return construct(next + 1)
		}
		keyword := firstWord(head)
		clauses := map[string][]string{
			"if": {"elif", "else"}, "elif": {"elif", "else"}, "try": {"except", "else", "finally"},
			"except": {"except", "else", "finally"}, "for": {"else"}, "while": {"else"}, "async": {"else"},
		}[keyword]
		last := end
		for k := end + 1; k < len(lines); k++ {
			if isBlank(k) {
				continue
			}
			if !start[k] || indentOf(lines[k]) > indent {
				last = k
				continue
			}
			if indentOf(lines[k]) == indent && !isComment(k) {
				word := firstWord(strings.TrimSpace(lines[k]))
				matched := slices.Contains(clauses, word)
				if matched {
					last = k
					continue
				}
			}
			break
		}
		return last + 1, ""
	}
	return construct
}

// firstWord returns the leading identifier of s.
func firstWord(s string) string {
	n := 0
	for n < len(s) && isIdentByte(s[n]) {
		n++
	}
	return s[:n]
}

// ---- YAML --------------------------------------------------------------

func yamlResolver(lines []string) resolver {
	return func(line int) (int, string) {
		i := line - 1
		head := strings.TrimSpace(lines[i])
		if head == "" {
			return 0, blankProblem(line)
		}
		if strings.HasPrefix(head, "#") {
			return 0, commentProblem(line)
		}
		if head == "---" || head == "..." {
			return 0, fmt.Sprintf("line %d is a document marker", line)
		}
		indent := indentOf(lines[i])
		keyOnly := !strings.HasPrefix(head, "- ") && head != "-" && strings.HasSuffix(stripYAMLComment(head), ":")
		last := i
		for k := i + 1; k < len(lines); k++ {
			t := strings.TrimSpace(lines[k])
			if t == "" {
				continue
			}
			ind := indentOf(lines[k])
			if ind > indent || keyOnly && ind == indent && (strings.HasPrefix(t, "- ") || t == "-") {
				last = k
				continue
			}
			break
		}
		return last + 1, ""
	}
}

func stripYAMLComment(s string) string {
	if i := strings.Index(s, " #"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
