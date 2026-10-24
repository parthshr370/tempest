package cloudcmd

import "strings"

// wrappers execute another command named by their arguments.
var wrappers = map[string]bool{
	"env": true, "command": true, "exec": true, "xargs": true, "nohup": true, "time": true, "timeout": true,
	"nice": true, "ionice": true, "stdbuf": true, "setsid": true, "sudo": true, "doas": true, "su": true,
	"watch": true, "chroot": true, "flock": true, "unbuffer": true, "parallel": true, "builtin": true,
	"eval": true, "source": true, ".": true, "busybox": true, "trap": true,
}

var shells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "mksh": true, "ash": true,
	"fish": true, "csh": true, "tcsh": true,
}

// posixShells run -c or stdin code without startup files unless an option
// asks for them: bash reads only BASH_ENV, sh and dash read ENV only when
// interactive. Other shells (zsh always reads .zshenv) may change the
// environment before the code runs.
var posixShells = map[string]bool{"sh": true, "bash": true, "dash": true}

// getopt is a wrapper's closed option table: short switches, short options
// taking a value (attached or the next word), short options with an optional
// attached value, and their long forms.
type getopt struct {
	switches, values, optional     string
	long, longValues, longOptional map[string]bool
}

// wrapperOptions lists the wrappers whose wrapped command is resolved and
// analyzed. Any option outside a table leaves the wrapper unresolved.
var wrapperOptions = map[string]getopt{
	"builtin": {},
	"command": {switches: "p"},
	"exec":    {},
	"nohup":   {},
	"time":    {switches: "p"},
	"nice":    {values: "n", longValues: nameSet("adjustment")},
	"timeout": {
		switches: "fpv", values: "sk",
		long: nameSet("foreground preserve-status verbose"), longValues: nameSet("signal kill-after"),
	},
	"xargs": {
		switches: "0oprtx", values: "adEILnPsJRS", optional: "eil",
		long:         nameSet("null open-tty interactive no-run-if-empty verbose exit show-limits"),
		longValues:   nameSet("arg-file delimiter max-args max-procs max-chars process-slot-var"),
		longOptional: nameSet("eof replace max-lines"),
	},
}

// skip returns the index of the first operand after the options, or false
// for an unknown or dynamic option.
func (g getopt) skip(args []Token) (int, bool) {
	for i := 0; i < len(args); i++ {
		text := args[i].Text
		if text == "--" {
			return i + 1, true
		}
		if len(text) < 2 || text[0] != '-' {
			return i, true
		}
		if !args[i].literal() {
			return 0, false
		}
		if strings.HasPrefix(text, "--") {
			name, _, hasValue := strings.Cut(text[2:], "=")
			switch {
			case g.longValues[name]:
				if !hasValue {
					i++
				}
			case g.long[name] && !hasValue, g.longOptional[name]:
			default:
				return 0, false
			}
			continue
		}
	cluster:
		for j := 1; j < len(text); j++ {
			switch c := text[j]; {
			case strings.IndexByte(g.values, c) >= 0:
				if j == len(text)-1 {
					i++
				}
				break cluster
			case strings.IndexByte(g.optional, c) >= 0:
				break cluster
			case strings.IndexByte(g.switches, c) < 0:
				return 0, false
			}
		}
	}
	return len(args), true
}

// wrappedCommand returns the command a resolvable wrapper runs (empty when it
// runs none) and whether the wrapper changes its environment.
func wrappedCommand(name string, args []Token) (inner []Token, envChanged, ok bool) {
	if name == "env" {
		return envCommand(args)
	}
	table, known := wrapperOptions[name]
	if !known {
		return nil, false, false
	}
	if name == "nice" && len(args) > 0 && args[0].literal() && isNiceIncrement(args[0].Text) {
		args = args[1:]
	}
	at, ok := table.skip(args)
	if !ok {
		return nil, false, false
	}
	if name == "timeout" && at < len(args) {
		at++ // DURATION
	}
	return args[at:], false, true
}

// envCommand parses env [-i] [-0] [-u NAME] [NAME=VALUE]... [COMMAND].
func envCommand(args []Token) (inner []Token, envChanged, ok bool) {
	i := 0
options:
	for ; i < len(args); i++ {
		text := args[i].Text
		if strings.HasPrefix(text, "-") && !args[i].literal() {
			return nil, false, false
		}
		switch {
		case text == "--":
			i++
			break options
		case text == "-" || text == "-i" || text == "--ignore-environment":
			envChanged = true
		case text == "-0" || text == "--null":
		case text == "-u" || text == "--unset":
			envChanged = true
			i++
		case strings.HasPrefix(text, "--unset=") || strings.HasPrefix(text, "-u"):
			envChanged = true
		case strings.HasPrefix(text, "-"):
			return nil, false, false
		default:
			break options
		}
	}
	for ; i < len(args) && isAssignment(args[i].Text); i++ {
		envChanged = true
	}
	if i > len(args) {
		i = len(args)
	}
	return args[i:], envChanged, true
}

// isNiceIncrement matches nice's historical -N / --N adjustment syntax.
func isNiceIncrement(text string) bool {
	digits := strings.TrimPrefix(strings.TrimPrefix(text, "-"), "-")
	return len(digits) < len(text) && allDigits(digits)
}

func (a *analyzer) wrapper(name string, args []Token, scope invocationContext) {
	switch name {
	case "eval":
		if len(args) > 0 {
			a.evalCode(name, args, scope)
		}
		return
	case "trap":
		a.trap(args, scope)
		return
	case "source", ".":
		a.envMutated = true
		if len(args) > 0 {
			a.syntax(name + " runs a script in the current shell")
		}
		return
	case "command":
		// command -v/-V looks a name up without running it.
		for _, arg := range args {
			if arg.Text == "--" || !strings.HasPrefix(arg.Text, "-") {
				break
			}
			if strings.ContainsAny(arg.Text, "vV") {
				return
			}
		}
	}
	inner, envChanged, ok := wrappedCommand(name, args)
	if !ok {
		a.unresolvedWrapper(name, args)
		return
	}
	if len(inner) == 0 {
		return
	}
	a.indirect("runs through " + name)
	scope.wrapped = true
	scope.envChanged = scope.envChanged || envChanged
	if name == "xargs" {
		a.syntax("xargs appends arguments read from stdin")
		scope.appended, scope.stdin = true, stdinSource{}
	}
	a.simple(inner, scope)
}

// unresolvedWrapper handles a wrapper whose command position the analysis
// does not parse. Literal arguments are covered by the cloud tool name
// search; dynamic ones, or text a wrapper may hand to a shell, are opaque.
func (a *analyzer) unresolvedWrapper(name string, args []Token) {
	if len(args) == 0 {
		return
	}
	a.syntax("command wrapper " + name)
	for _, arg := range args {
		if !arg.literal() || strings.ContainsAny(arg.Text, "$`") {
			a.opaque(Invocation{Tool: name, Effect: EffectOpaque, cluster: true}, "wrapper arguments are dynamic or shell code")
			return
		}
	}
}

func (a *analyzer) evalCode(name string, code []Token, scope invocationContext) {
	a.indirect(name + " runs shell code")
	texts := make([]string, len(code))
	for i, word := range code {
		if !word.literal() {
			a.opaque(Invocation{Tool: name, Effect: EffectOpaque, cluster: true}, "dynamic shell code")
			return
		}
		texts[i] = word.Text
	}
	a.inline(strings.Join(texts, " "), scope.envChanged, true)
}

func (a *analyzer) trap(args []Token, scope invocationContext) {
	if len(args) > 0 && args[0].Text == "--" {
		args = args[1:]
	}
	// One operand resets a signal; none prints the traps.
	if len(args) < 2 {
		return
	}
	action := args[0]
	if action.literal() && (action.Text == "" || strings.HasPrefix(action.Text, "-")) {
		return
	}
	a.evalCode("trap", args[:1], scope)
}

type shellInput uint8

const (
	shellScript  shellInput = iota // a named script file, like any program
	shellString                    // -c code at args[at]
	shellStdin                     // commands from standard input
	shellNothing                   // -c without code: nothing runs
)

var safeShellOptions = nameSet("errexit nounset pipefail xtrace verbose noglob")

// shellSource reports where a shell invocation reads its commands. exact is
// false for options that read startup files or change shell behavior beyond
// errexit/nounset/pipefail/xtrace/verbose/noglob.
func shellSource(args []Token) (input shellInput, at int, exact bool) {
	exact = true
	command, stdin := false, false
	operand := func(i int) (shellInput, int, bool) {
		switch {
		case command && i < len(args):
			return shellString, i, exact
		case command:
			return shellNothing, 0, exact
		case stdin || i >= len(args):
			return shellStdin, 0, exact
		}
		return shellScript, 0, exact
	}
	for i := 0; i < len(args); i++ {
		text := args[i].Text
		switch {
		case text == "--" || text == "-":
			return operand(i + 1)
		case text == "--command":
			command, exact = true, false
		case text == "--rcfile" || text == "--init-file":
			exact = false
			i++
		case strings.HasPrefix(text, "--"):
			exact = false
		case len(text) > 1 && (text[0] == '-' || text[0] == '+'):
			setOption, shopt := false, false
			for j := 1; j < len(text); j++ {
				switch text[j] {
				case 'c':
					command = command || text[0] == '-'
				case 's':
					stdin = stdin || text[0] == '-'
				case 'o':
					setOption = true
				case 'O':
					shopt = true
				case 'e', 'u', 'x', 'v', 'f':
				default:
					exact = false
				}
			}
			if setOption || shopt {
				i++
				if shopt || i >= len(args) || !safeShellOptions[args[i].Text] {
					exact = false
				}
			}
		default:
			return operand(i)
		}
	}
	return operand(len(args))
}

func (a *analyzer) shell(name string, args []Token, scope invocationContext) {
	input, at, exact := shellSource(args)
	exact = exact && posixShells[name]
	switch input {
	case shellString:
		a.shellCode(name, args[at], exact, scope)
	case shellStdin:
		switch scope.stdin.kind {
		case stdinHeredoc, stdinString:
			a.shellCode(name, scope.stdin.code, exact, scope)
		case stdinFile:
			a.syntax(name + " reads commands from a redirected file")
		default:
			// A pipe (curl ... | sh) or inherited input: code the analysis
			// cannot see.
			a.opaque(Invocation{Tool: name, Effect: EffectOpaque, cluster: true}, "shell reads commands from stdin")
		}
	}
}

// shellCode analyzes the code a shell runs from -c or a here-document. Code
// the shell receives only after expansion is unknown.
func (a *analyzer) shellCode(name string, code Token, exact bool, scope invocationContext) {
	a.indirect(name + " runs inline shell code")
	if !code.literal() {
		a.opaque(Invocation{Tool: name, Effect: EffectOpaque, cluster: true}, "dynamic shell code")
		return
	}
	if !exact {
		a.syntax(name + " options or startup files can change the environment")
	}
	a.inline(code.Text, scope.envChanged || !exact, false)
}

// find runs the command after -exec/-execdir/-ok/-okdir with match paths
// appended.
func (a *analyzer) find(args []Token, scope invocationContext) {
	for i := 0; i < len(args); i++ {
		switch args[i].Text {
		case "-exec", "-execdir", "-ok", "-okdir":
			end := i + 1
			for end < len(args) && args[end].Text != ";" && args[end].Text != "+" {
				end++
			}
			if end > i+1 {
				a.syntax("find " + args[i].Text + " runs a command for each match")
				inner := scope
				inner.wrapped, inner.appended, inner.stdin = true, true, stdinSource{}
				a.simple(args[i+1:end], inner)
			}
			i = end
		}
	}
}

// cloudTools are the executables the verb tables classify.
var cloudTools = []string{ToolKubectl, ToolHelm, ToolTerraform, ToolKustomize}

// namesCloudTool reports whether text may name a classified tool as an
// executable anywhere, including inside quoted strings, substitutions and
// here-documents: the name ignoring ASCII case, once quotes, backslashes and
// line continuations are removed, not joined to other name characters and not
// a directory component. It is a deliberate superset; it only decides whether
// unresolved syntax makes the command opaque.
func namesCloudTool(text string) bool {
	normalized := normalizeCloudText(text)
	for _, tool := range cloudTools {
		if namesNormalizedCloudTool(normalized, tool) {
			return true
		}
	}
	return false
}

func normalizeCloudText(text string) string {
	folded := make([]byte, 0, len(text))
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '\\' && i+1 < len(text) && text[i+1] == '\n':
			i++
		case c == '\\' || c == '\'' || c == '"':
		case 'A' <= c && c <= 'Z':
			folded = append(folded, c+'a'-'A')
		default:
			folded = append(folded, c)
		}
	}
	return string(folded)
}

func namesNormalizedCloudTool(normalized, tool string) bool {
	for from := 0; ; {
		offset := strings.Index(normalized[from:], tool)
		if offset < 0 {
			return false
		}
		start, end := from+offset, from+offset+len(tool)
		if (start == 0 || !nameByte(normalized[start-1])) && (end == len(normalized) || !nameByte(normalized[end]) && normalized[end] != '/') {
			return true
		}
		from = start + 1
	}
}

func nameByte(c byte) bool {
	return c == '_' || c == '-' || c == '.' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z'
}
