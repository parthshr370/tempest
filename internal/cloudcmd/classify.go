package cloudcmd

import "strings"

// Option tables, version 1. Each table is closed: an option missing from a
// read form's table makes that form opaque, because its arity and effect are
// unknown. Extend a table only together with a classification test.

type arity uint8

const (
	// switchOption takes no separate value; "--name=value" may attach one.
	switchOption arity = iota + 1
	// valueOption takes the next word or an attached "=value".
	valueOption
)

type optionTable map[string]arity

func options(switches, values string) optionTable {
	table := make(optionTable)
	for _, name := range strings.Fields(switches) {
		table[name] = switchOption
	}
	for _, name := range strings.Fields(values) {
		table[name] = valueOption
	}
	return table
}

func merge(tables ...optionTable) optionTable {
	out := make(optionTable)
	for _, table := range tables {
		for name, kind := range table {
			out[name] = kind
		}
	}
	return out
}

func nameSet(names string) map[string]bool {
	out := make(map[string]bool)
	for _, name := range strings.Fields(names) {
		out[name] = true
	}
	return out
}

// form is one closed subcommand path's classification.
type form struct {
	effect    Effect
	options   optionTable // full table (globals included) for parsing
	maxArgs   int         // positional operands after the path; -1 unbounded
	cluster   bool
	grantable bool
}

// ---- kubectl -------------------------------------------------------------

var (
	kubectlGlobal = options(
		"-h --help --insecure-skip-tls-verify --match-server-version --warnings-as-errors --disable-compression",
		"--context --kubeconfig -n --namespace --request-timeout -v --v --cluster --user -s --server --as --as-group --as-uid --token --certificate-authority --client-certificate --client-key --tls-server-name --username --password")
	kubectlOverrides = nameSet("--kubeconfig --cluster --user -s --server --as --as-group --as-uid --token --certificate-authority --client-certificate --client-key --insecure-skip-tls-verify --tls-server-name --username --password")
	kubectlPrinter   = options("--show-managed-fields --allow-missing-template-keys", "-o --output --template")
	// kubectlWriteOptions only establishes arity for grant eligibility; write
	// and destructive forms keep their class with unknown options.
	kubectlWriteOptions = merge(kubectlGlobal, kubectlPrinter, options(
		"-R --recursive -A --all-namespaces --all --dry-run --server-side --force-conflicts --prune --overwrite --record --validate --wait --cascade --force --now --ignore-not-found --local --save-config --edit --windows-line-endings -i --stdin -t --tty -q --quiet --ignore-daemonsets --delete-emptydir-data --disable-eviction --share-processes",
		"-f --filename -k --kustomize -l --selector --field-selector --field-manager --prune-allowlist --timeout --grace-period --replicas --current-replicas --resource-version --type -p --patch --patch-file --subresource -c --container --pod-running-timeout --image --min --max --cpu-percent --name --to-revision --skip-wait-for-delete-timeout --address --port --accept-hosts --api-prefix --target --copy-to --profile -e --env"))

	kubectlForms = map[string]form{
		"get": kubectlRead(-1, merge(kubectlPrinter, options(
			"-A --all-namespaces -w --watch --watch-only --output-watch-events --show-labels --show-kind --no-headers --ignore-not-found -R --recursive --server-print",
			"-l --selector --field-selector -L --label-columns --sort-by --chunk-size --subresource -f --filename"))),
		"describe": kubectlRead(-1, options("-A --all-namespaces --show-events -R --recursive", "-l --selector --chunk-size -f --filename")),
		"logs": kubectlRead(2, options(
			"-f --follow --timestamps --all-containers -p --previous --prefix --ignore-errors --all-pods --insecure-skip-tls-verify-backend",
			"--tail --since --since-time -c --container -l --selector --max-log-requests --limit-bytes --pod-running-timeout")),
		"explain":                kubectlRead(1, options("--recursive", "--api-version --output")),
		"api-resources":          kubectlRead(0, options("--namespaced --cached --no-headers", "--verbs --api-group --sort-by --categories -o --output")),
		"api-versions":           kubectlRead(0, nil),
		"cluster-info":           kubectlRead(0, nil),
		"version":                kubectlRead(0, options("--client", "-o --output")),
		"rollout status":         kubectlRead(1, options("-w --watch -R --recursive", "--revision --timeout -l --selector -f --filename")),
		"rollout history":        kubectlRead(1, merge(kubectlPrinter, options("-R --recursive", "--revision -l --selector -f --filename"))),
		"rollout pause":          kubectlChange(EffectWrite),
		"rollout resume":         kubectlChange(EffectWrite),
		"rollout undo":           kubectlChange(EffectDestructive),
		"rollout restart":        kubectlChange(EffectDestructive),
		"config view":            kubectlConfig(EffectRead, 0, options("--minify --flatten --allow-missing-template-keys", "-o --output --template")),
		"config current-context": kubectlConfig(EffectRead, 0, nil),
		"config get-contexts":    kubectlConfig(EffectRead, 1, options("--no-headers", "-o --output")),
		"config get-clusters":    kubectlConfig(EffectRead, 0, nil),
		"config get-users":       kubectlConfig(EffectRead, 0, nil),
		"delete":                 kubectlChange(EffectDestructive),
		"drain":                  kubectlChange(EffectDestructive),
		"cordon":                 kubectlChange(EffectDestructive),
		"uncordon":               kubectlChange(EffectDestructive),
		"taint":                  kubectlChange(EffectDestructive),
	}
)

func init() {
	for _, verb := range strings.Fields("apply create replace patch edit label annotate scale autoscale set exec attach cp port-forward proxy debug diff") {
		kubectlForms[verb] = kubectlChange(EffectWrite)
	}
	for _, sub := range strings.Fields("set set-context set-cluster set-credentials unset use-context use delete-context delete-cluster delete-user rename-context") {
		kubectlForms["config "+sub] = kubectlConfig(EffectWrite, -1, nil)
	}
}

func kubectlRead(maxArgs int, extra optionTable) form {
	return form{effect: EffectRead, options: merge(kubectlGlobal, extra), maxArgs: maxArgs, cluster: true}
}

func kubectlChange(effect Effect) form {
	return form{effect: effect, options: kubectlWriteOptions, maxArgs: -1, cluster: true, grantable: effect == EffectWrite}
}

// kubectlConfig forms edit or print kubeconfig only: no cluster target and
// never grantable.
func kubectlConfig(effect Effect, maxArgs int, extra optionTable) form {
	return form{effect: effect, options: merge(kubectlGlobal, extra), maxArgs: maxArgs}
}

var kubectlProgram = cobraProgram{
	contextFlag: "--context",
	kubeconfig:  "--kubeconfig",
	global:      kubectlGlobal,
	overrides:   kubectlOverrides,
	forms:       kubectlForms,
	groups:      nameSet("rollout config"),
	escalate: func(path string, args []Token) bool {
		return path == "replace" && hasEnabledFlag(args, "--force")
	},
	refine: func(path string, parsed cobraArgs, inv *Invocation) string {
		if path == "version" && parsed.enabled("--client") {
			inv.cluster = false
		}
		return ""
	},
}

func classifyKubectl(args []Token) (Invocation, string) {
	return classifyCobra(args, kubectlProgram)
}

// ---- helm ----------------------------------------------------------------

var (
	helmGlobal = options(
		"-h --help --debug --kube-insecure-skip-tls-verify",
		"--kube-context --kubeconfig -n --namespace --burst-limit --qps --registry-config --repository-cache --repository-config --content-cache --kube-apiserver --kube-as-user --kube-as-group --kube-ca-file --kube-token --kube-tls-server-name")
	helmValues         = options("", "-f --values --set --set-string --set-file --set-json --set-literal")
	helmInstallOptions = merge(helmGlobal, helmValues, options(
		"-i --install --atomic --rollback-on-failure --wait --wait-for-jobs --create-namespace --reuse-values --reset-values --reset-then-reuse-values --force --replace --force-replace --force-conflicts --cleanup-on-fail --no-hooks --devel --dry-run --skip-crds -g --generate-name --dependency-update --disable-openapi-validation --skip-schema-validation --take-ownership --recreate-pods --logs --server-side --render-subchart-notes --enable-dns --insecure-skip-tls-verify --plain-http --verify --pass-credentials",
		"--version --timeout --description --history-max --name-template --labels --filter -o --output --repo --username --password --ca-file --cert-file --key-file --keyring --post-renderer --post-renderer-args"))
	helmTemplateOptions = merge(helmGlobal, helmValues, options(
		"--include-crds --skip-crds --skip-tests --release-name --is-upgrade --no-hooks -g --generate-name --create-namespace --devel --dry-run --disable-openapi-validation --skip-schema-validation --dependency-update --validate",
		"--version -s --show-only -a --api-versions --kube-version --name-template --description --timeout --labels --output-dir --post-renderer --post-renderer-args"))
	helmForms = map[string]form{
		"list": {effect: EffectRead, cluster: true, maxArgs: 0, options: merge(helmGlobal, options(
			"-a --all -A --all-namespaces -q --short -r --reverse -d --date --deployed --failed --pending --superseded --uninstalled --uninstalling --no-headers",
			"-o --output -f --filter -l --selector -m --max --offset --time-format"))},
		"status":       {effect: EffectRead, cluster: true, maxArgs: 1, options: merge(helmGlobal, options("--show-desc --show-resources", "-o --output --revision"))},
		"history":      {effect: EffectRead, cluster: true, maxArgs: 1, options: merge(helmGlobal, options("", "-o --output --max"))},
		"get all":      {effect: EffectRead, cluster: true, maxArgs: 1, options: merge(helmGlobal, options("", "--revision --template"))},
		"get values":   {effect: EffectRead, cluster: true, maxArgs: 1, options: merge(helmGlobal, options("-a --all", "--revision -o --output"))},
		"get manifest": {effect: EffectRead, cluster: true, maxArgs: 1, options: merge(helmGlobal, options("", "--revision"))},
		"get hooks":    {effect: EffectRead, cluster: true, maxArgs: 1, options: merge(helmGlobal, options("", "--revision"))},
		"get notes":    {effect: EffectRead, cluster: true, maxArgs: 1, options: merge(helmGlobal, options("", "--revision"))},
		"get metadata": {effect: EffectRead, cluster: true, maxArgs: 1, options: merge(helmGlobal, options("", "--revision -o --output"))},
		"show all":     helmShowForm(nil),
		"show chart":   helmShowForm(nil),
		"show values":  helmShowForm(options("", "--jsonpath")),
		"show readme":  helmShowForm(nil),
		"version":      {effect: EffectRead, maxArgs: 0, options: merge(helmGlobal, options("--short", "--template"))},
		"template":     {effect: EffectRead, maxArgs: 2, options: helmTemplateOptions},
		"install":      helmReleaseWrite(),
		"upgrade":      helmReleaseWrite(),
		"rollback":     helmReleaseWrite(),
		"test":         helmReleaseWrite(),
		"uninstall":    {effect: EffectDestructive, cluster: true, maxArgs: -1, options: merge(helmGlobal, options("--keep-history --no-hooks --wait --dry-run --ignore-not-found", "--timeout --description --cascade"))},
		// Local chart, repository and registry writes: no cluster target and
		// never grantable by a context rule.
		"lint":              helmLocalWrite(),
		"package":           helmLocalWrite(),
		"pull":              helmLocalWrite(),
		"dependency update": helmLocalWrite(),
		"dependency build":  helmLocalWrite(),
		"repo add":          helmLocalWrite(),
		"repo update":       helmLocalWrite(),
		"repo remove":       helmLocalWrite(),
		"registry login":    helmLocalWrite(),
		"registry logout":   helmLocalWrite(),
	}
	helmProgram = cobraProgram{
		contextFlag: "--kube-context",
		kubeconfig:  "--kubeconfig",
		global:      helmGlobal,
		overrides:   nameSet("--kubeconfig --kube-apiserver --kube-as-user --kube-as-group --kube-ca-file --kube-token --kube-insecure-skip-tls-verify --kube-tls-server-name"),
		forms:       helmForms,
		groups:      nameSet("get show dependency repo registry"),
		aliases: map[string]string{
			"ls": "list", "hist": "history", "inspect": "show", "delete": "uninstall", "del": "uninstall", "un": "uninstall",
			"fetch": "pull", "dep": "dependency", "dependencies": "dependency",
		},
		escalate: func(path string, args []Token) bool {
			switch path {
			case "install", "upgrade", "rollback":
				return hasEnabledFlag(args, "--force") || hasEnabledFlag(args, "--replace") || hasEnabledFlag(args, "--force-replace")
			}
			return false
		},
		refine: refineHelmTemplate,
	}
)

func helmShowForm(extra optionTable) form {
	return form{effect: EffectRead, options: merge(helmGlobal, options("--devel", "--version"), extra), maxArgs: 1}
}

func helmReleaseWrite() form {
	return form{effect: EffectWrite, options: helmInstallOptions, maxArgs: -1, cluster: true, grantable: true}
}

func helmLocalWrite() form {
	return form{effect: EffectWrite, options: helmGlobal, maxArgs: -1}
}

func classifyHelm(args []Token) (Invocation, string) {
	inv, reason := classifyCobra(args, helmProgram)
	if hasFlag(args, "--post-renderer") || hasFlag(args, "--post-renderer-args") {
		inv.Effect, inv.ExplicitContext = EffectOpaque, false
		reason = "post-renderer executes a local program"
	}
	return inv, reason
}

// refineHelmTemplate keeps helm template read-only only without file output,
// dependency downloads or cluster-contacting validation.
func refineHelmTemplate(path string, parsed cobraArgs, inv *Invocation) string {
	if path != "template" || inv.Effect != EffectRead {
		return ""
	}
	if value, ok := parsed.value("--dry-run"); ok {
		switch value {
		case "", "true", "false", "client", "none":
		case "server":
			inv.Effect, inv.cluster = EffectWrite, true
			return "server dry-run contacts the cluster"
		default:
			inv.Effect = EffectOpaque
			return "unrecognized --dry-run mode"
		}
	}
	if parsed.enabled("--validate") {
		inv.Effect, inv.cluster = EffectWrite, true
		return "--validate contacts the cluster"
	}
	if _, ok := parsed.value("--output-dir"); ok {
		inv.Effect = EffectWrite
		return "--output-dir writes files"
	}
	if parsed.enabled("--dependency-update") {
		inv.Effect = EffectWrite
		return "--dependency-update downloads charts"
	}
	return ""
}

// ---- terraform -----------------------------------------------------------

var terraformReadForms = map[string]optionTable{
	"version":        options("-json", ""),
	"show":           options("-json -no-color", ""),
	"output":         options("-json -raw -no-color", ""),
	"state list":     options("", "-id"),
	"state show":     options("", ""),
	"workspace list": options("", ""),
	"workspace show": options("", ""),
}

var terraformReadArgs = map[string]int{"version": 0, "show": 1, "output": 1, "state list": -1, "state show": 1, "workspace list": 0, "workspace show": 0}

var (
	terraformWrite       = nameSet("plan validate init apply import refresh fmt providers graph")
	terraformDestructive = nameSet("destroy taint untaint force-unlock")
	terraformSubforms    = map[string]Effect{
		"state rm": EffectDestructive, "state mv": EffectDestructive, "state push": EffectDestructive, "state replace-provider": EffectDestructive,
		"workspace new": EffectWrite, "workspace select": EffectWrite, "workspace delete": EffectDestructive,
	}
	terraformFmtReadOptions = nameSet("-check -diff -recursive -no-color -list")
)

// terraformOption normalizes -name, --name and -name=value.
func terraformOption(text string) (name, value string, hasValue bool) {
	if !strings.HasPrefix(text, "-") || text == "-" {
		return "", "", false
	}
	name, value, hasValue = strings.Cut(text, "=")
	if strings.HasPrefix(name, "--") {
		name = name[1:]
	}
	return name, value, hasValue
}

func classifyTerraform(args []Token, dir *dirState) (Invocation, string) {
	inv := Invocation{}
	if dynamicArgs(args) {
		inv.Effect = EffectOpaque
		return inv, "dynamic argument"
	}
	help, chdirSeen := false, false
	chdir := ""
	for _, arg := range args {
		name, value, hasValue := terraformOption(arg.Text)
		switch {
		case strings.HasPrefix(name, "-plugin"):
			inv.Effect = EffectOpaque
			return inv, "plugin override"
		case name == "-help" || name == "-h":
			help = true
		case name == "-chdir":
			if chdirSeen || !hasValue || value == "" {
				inv.Effect = EffectOpaque
				return inv, "-chdir must appear once as -chdir=DIR"
			}
			chdirSeen, chdir = true, value
		}
	}
	if chdirSeen && !dir.contains(chdir) {
		inv.Effect = EffectOpaque
		return inv, "-chdir leaves the workspace or follows an uncertain cd"
	}
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i].Text, "-"); i++ {
		name, _, _ := terraformOption(args[i].Text)
		switch name {
		case "-chdir", "-help", "-h":
		case "-version", "-v":
			inv.Effect, inv.Verb = EffectRead, "version"
			return inv, ""
		default:
			inv.Effect = EffectOpaque
			return inv, "unrecognized option before subcommand"
		}
	}
	if help || i == len(args) {
		inv.Effect = EffectRead
		return inv, "help"
	}
	verb := args[i].Text
	rest := args[i+1:]
	inv.Verb = verb
	switch verb {
	case "state", "workspace":
		if len(rest) == 0 || strings.HasPrefix(rest[0].Text, "-") {
			inv.Effect = EffectOpaque
			return inv, "subcommand not established"
		}
		inv.Verb = verb + " " + rest[0].Text
		rest = rest[1:]
	}
	if table, ok := terraformReadForms[inv.Verb]; ok {
		if !terraformOptionsKnown(rest, table, terraformReadArgs[inv.Verb]) {
			inv.Effect = EffectOpaque
			return inv, "unrecognized option or operand for read form"
		}
		inv.Effect = EffectRead
		return inv, ""
	}
	if effect, ok := terraformSubforms[inv.Verb]; ok {
		inv.Effect = effect
		return inv, ""
	}
	switch {
	case terraformDestructive[verb]:
		inv.Effect = EffectDestructive
	case (verb == "apply" || verb == "plan") && hasEnabledFlag(rest, "-destroy"):
		inv.Effect = EffectDestructive
		return inv, "-destroy"
	case verb == "fmt" && terraformFmtReadOnly(rest):
		inv.Effect = EffectRead
		return inv, "fmt -check"
	case terraformWrite[verb]:
		inv.Effect = EffectWrite
	default:
		inv.Effect = EffectOpaque
		return inv, "unknown terraform subcommand"
	}
	return inv, ""
}

func terraformOptionsKnown(args []Token, table optionTable, maxArgs int) bool {
	operands := 0
	for i := 0; i < len(args); i++ {
		name, _, hasValue := terraformOption(args[i].Text)
		if name == "" {
			operands++
			continue
		}
		kind, ok := table[name]
		if !ok {
			return false
		}
		if kind == valueOption && !hasValue {
			if i+1 >= len(args) {
				return false
			}
			i++
		}
	}
	return maxArgs < 0 || operands <= maxArgs
}

func terraformFmtReadOnly(args []Token) bool {
	check := false
	for _, arg := range args {
		name, value, hasValue := terraformOption(arg.Text)
		if name == "" {
			continue
		}
		if !terraformFmtReadOptions[name] {
			return false
		}
		if name == "-check" {
			check = !hasValue || value != "false"
		}
	}
	return check
}

// ---- kustomize -----------------------------------------------------------

func classifyKustomize(args []Token) (Invocation, string) {
	inv := Invocation{}
	if dynamicArgs(args) {
		inv.Effect = EffectOpaque
		return inv, "dynamic argument"
	}
	var operands []string
	for _, arg := range args {
		switch text := arg.Text; {
		case text == "-h" || text == "--help":
			inv.Effect = EffectRead
			return inv, "help"
		case text == "--stack-trace" && len(operands) == 0:
		case strings.HasPrefix(text, "-") && text != "-":
			if len(operands) == 0 || operands[0] == "version" {
				inv.Effect = EffectOpaque
				return inv, "unrecognized option"
			}
		default:
			operands = append(operands, text)
		}
	}
	if len(operands) == 0 {
		inv.Effect = EffectRead
		return inv, "help"
	}
	inv.Verb = operands[0]
	switch {
	case inv.Verb == "version" && len(operands) == 1:
		inv.Effect = EffectRead
		return inv, ""
	case inv.Verb == "edit" || inv.Verb == "create" || inv.Verb == "cfg":
		inv.Effect = EffectWrite
		return inv, ""
	case inv.Verb == "build":
		inv.Effect = EffectOpaque
		return inv, "build may run plugins, generators, Helm or remote resources"
	}
	inv.Effect = EffectOpaque
	return inv, "unknown kustomize subcommand"
}

// ---- shared cobra/pflag parsing -----------------------------------------

type cobraProgram struct {
	contextFlag, kubeconfig string
	global                  optionTable
	overrides               map[string]bool
	forms                   map[string]form
	groups                  map[string]bool
	aliases                 map[string]string
	escalate                func(path string, args []Token) bool
	refine                  func(path string, parsed cobraArgs, inv *Invocation) string
}

type flagUse struct {
	name, value string
}

type cobraArgs struct {
	operands []string
	flags    []flagUse
	unknown  bool
	missing  bool
	dashdash bool
	help     bool
	// reliable is the number of operands established before an unknown
	// option whose arity is unknown; -1 when every operand is reliable.
	reliable int
}

func (p cobraArgs) operandReliable(index int) bool {
	return p.reliable < 0 || index < p.reliable
}

func (p cobraArgs) value(name string) (string, bool) {
	for _, use := range p.flags {
		if use.name == name {
			return use.value, true
		}
	}
	return "", false
}

func (p cobraArgs) enabled(name string) bool {
	value, ok := p.value(name)
	return ok && value != "false"
}

func (p cobraArgs) count(name string) int {
	n := 0
	for _, use := range p.flags {
		if use.name == name {
			n++
		}
	}
	return n
}

// parseCobra follows pflag rules: interspersed options, "--name=value",
// "--name value" for value options, shorthand clusters "-abc" and attached
// shorthand values "-ovalue"/"-o=value", and "--" ending option parsing.
// tableFor selects the option table for the operands seen so far.
func parseCobra(args []Token, tableFor func(operands []string) optionTable) cobraArgs {
	p := cobraArgs{reliable: -1}
	unknownSpaced := func() {
		p.unknown = true
		if p.reliable < 0 {
			p.reliable = len(p.operands)
		}
	}
	for i := 0; i < len(args); i++ {
		text := args[i].Text
		switch {
		case text == "--":
			p.dashdash = true
			return p
		case strings.HasPrefix(text, "--"):
			table := tableFor(p.operands)
			name, value, hasValue := strings.Cut(text, "=")
			kind, ok := table[name]
			if !ok {
				if hasValue {
					p.unknown = true
				} else {
					unknownSpaced()
				}
				continue
			}
			if kind == valueOption && !hasValue {
				if i+1 >= len(args) {
					p.missing = true
					continue
				}
				i++
				value = args[i].Text
			}
			if kind == switchOption && !hasValue {
				value = "true"
			}
			p.flags = append(p.flags, flagUse{name, value})
			if name == "--help" && value != "false" {
				p.help = true
			}
		case len(text) > 1 && text[0] == '-':
			table := tableFor(p.operands)
			for j := 1; j < len(text); j++ {
				name := "-" + text[j:j+1]
				kind, ok := table[name]
				if !ok {
					unknownSpaced()
					break
				}
				if kind == valueOption {
					value := strings.TrimPrefix(text[j+1:], "=")
					if j+1 == len(text) {
						if i+1 >= len(args) {
							p.missing = true
							break
						}
						i++
						value = args[i].Text
					}
					p.flags = append(p.flags, flagUse{name, value})
					break
				}
				if j+1 < len(text) && text[j+1] == '=' {
					p.flags = append(p.flags, flagUse{name, text[j+2:]})
					if name == "-h" && text[j+2:] != "false" {
						p.help = true
					}
					break
				}
				p.flags = append(p.flags, flagUse{name, "true"})
				if name == "-h" {
					p.help = true
				}
			}
		default:
			p.operands = append(p.operands, text)
		}
	}
	return p
}

// classifyCobra resolves the subcommand path, parses with the path's closed
// option table and derives effect and target eligibility.
func classifyCobra(args []Token, program cobraProgram) (Invocation, string) {
	inv := Invocation{cluster: true}
	scan := scanTargets(args, program)
	inv.Context = scan.context()
	if dynamicArgs(args) {
		inv.Effect = EffectOpaque
		return inv, "dynamic argument"
	}
	resolve := func(operands []string) (string, int, bool) {
		if len(operands) == 0 {
			return "", 0, false
		}
		verb := operands[0]
		if alias, ok := program.aliases[verb]; ok {
			verb = alias
		}
		if program.groups[verb] {
			if len(operands) < 2 {
				return verb, 1, false
			}
			return verb + " " + operands[1], 2, true
		}
		return verb, 1, true
	}
	parsed := parseCobra(args, func(operands []string) optionTable {
		if path, _, complete := resolve(operands); complete {
			if f, ok := program.forms[path]; ok && f.options != nil {
				return f.options
			}
		}
		return program.global
	})
	path, depth, complete := resolve(parsed.operands)
	inv.Verb = path
	inv.ExplicitContext = explicitTarget(inv.Context, scan, parsed, program)
	switch {
	case !parsed.operandReliable(0) || complete && depth == 2 && !parsed.operandReliable(1):
		inv.Effect = EffectOpaque
		return inv, "option of unknown arity before the subcommand"
	case parsed.help || path == "" || !complete:
		// Help output, a bare program, or a bare command group print usage
		// and contact nothing.
		inv.Effect, inv.cluster = EffectRead, false
		return inv, "help"
	case scan.repeated():
		inv.Effect = EffectOpaque
		return inv, "repeated " + program.contextFlag + " or " + program.kubeconfig
	}
	f, ok := program.forms[path]
	if !ok || f.effect == "" {
		inv.Effect = EffectOpaque
		return inv, "unknown subcommand"
	}
	inv.Effect, inv.cluster, inv.grantable = f.effect, f.cluster, f.grantable
	if program.escalate != nil && program.escalate(path, args) {
		inv.Effect, inv.grantable = EffectDestructive, false
		return inv, "force or replace option"
	}
	operands := len(parsed.operands) - depth
	if f.effect == EffectRead && (parsed.unknown || parsed.missing || parsed.dashdash || f.maxArgs >= 0 && operands > f.maxArgs) {
		inv.Effect = EffectOpaque
		return inv, "unrecognized option or operand for read form"
	}
	if program.refine != nil {
		return inv, program.refine(path, parsed, &inv)
	}
	return inv, ""
}

// explicitTarget reports that the literal context is the established target:
// exactly one context flag that the precise parse agrees on, no kubeconfig,
// endpoint or credential override and no option of unknown arity.
func explicitTarget(context string, scan targetScan, parsed cobraArgs, program cobraProgram) bool {
	if context == "" || scan.override || scan.kubeconfig || parsed.unknown || parsed.missing || parsed.count(program.contextFlag) != 1 {
		return false
	}
	for _, use := range parsed.flags {
		if program.overrides[use.name] {
			return false
		}
	}
	value, _ := parsed.value(program.contextFlag)
	return value == context
}

// targetScan is a literal pass over every word before "--". It ignores
// option arity on purpose: a deny rule must match "--context prod" even when
// the precise parse is impossible.
type targetScan struct {
	contexts   []string
	dynamic    bool
	kubeconfig bool
	configs    int
	override   bool
}

func (s targetScan) context() string {
	if len(s.contexts) != 1 || s.dynamic {
		return ""
	}
	return s.contexts[0]
}

func (s targetScan) repeated() bool { return len(s.contexts) > 1 || s.configs > 1 }

func scanTargets(args []Token, program cobraProgram) targetScan {
	var scan targetScan
	for i := 0; i < len(args); i++ {
		text := args[i].Text
		if text == "--" {
			break
		}
		name, value, hasValue := strings.Cut(text, "=")
		if program.overrides[name] {
			scan.override = true
		}
		if name != program.contextFlag && name != program.kubeconfig {
			continue
		}
		dynamic := args[i].expansion || args[i].pattern
		if !hasValue && i+1 < len(args) {
			i++
			value, dynamic = args[i].Text, args[i].expansion || args[i].pattern
		}
		if name == program.kubeconfig {
			scan.kubeconfig = true
			scan.configs++
			continue
		}
		scan.contexts = append(scan.contexts, value)
		scan.dynamic = scan.dynamic || dynamic || value == ""
	}
	return scan
}

func dynamicArgs(args []Token) bool {
	for _, arg := range args {
		if arg.expansion || arg.pattern {
			return true
		}
	}
	return false
}

func hasFlag(args []Token, name string) bool {
	for _, arg := range args {
		if arg.Text == "--" {
			return false
		}
		if flag, _, _ := strings.Cut(arg.Text, "="); flag == name {
			return true
		}
	}
	return false
}

// hasEnabledFlag finds "name" or "name=value" (value not false) anywhere
// before "--". It is a literal superset used only to escalate risk.
func hasEnabledFlag(args []Token, name string) bool {
	for _, arg := range args {
		if arg.Text == "--" {
			return false
		}
		flag, value, hasValue := strings.Cut(arg.Text, "=")
		if flag == name && (!hasValue || value != "false") {
			return true
		}
	}
	return false
}
