package cloudcmd

import (
	"fmt"
	"strings"
	"testing"
)

func TestCloudShellSyntax(t *testing.T) {
	for _, test := range []struct {
		command string
		want    Effect
		cloud   bool
	}{
		// Pipelines and lists join by maximum risk, not first/last stage.
		{"kubectl get pods | jq .", EffectRead, true},
		{"kubectl get pods 2>&1 | grep api", EffectRead, true},
		{"kubectl get pods > pods.yaml", EffectRead, true},
		{"kubectl get pods | tee out.yaml", EffectRead, true},
		{"echo ok; kubectl delete pod x", EffectDestructive, true},
		{"kubectl get pods && kubectl apply -f x.yaml", EffectWrite, true},
		{"kubectl get pods\nkubectl delete pod x", EffectDestructive, true},
		{"kubectl get pods || helm uninstall api", EffectDestructive, true},
		{"kubectl get pods | kubectl apply -f - | jq .", EffectWrite, true},
		{"echo manifest | kubectl apply -f -", EffectWrite, true},
		{"kubectl get pods &", EffectRead, true},
		{"aws eks update-kubeconfig --name dev && kubectl get pods", EffectRead, true},
		// Quoting and escapes yield the literal program and verb.
		{"'kubectl' get pods", EffectRead, true},
		{"k\\ubectl delete pod x", EffectDestructive, true},
		{"kubectl \"delete\" pod x", EffectDestructive, true},
		{"/usr/local/bin/kubectl delete pod x", EffectDestructive, true},
		{"./bin/helm uninstall api", EffectDestructive, true},
		{"kubectl get pods -l 'app=$APP'", EffectRead, true},
		{"kubectl get pods # && kubectl delete pod x", EffectRead, true},
		{"kubectl get pods \\\n  -n prod", EffectRead, true},
		// Expansions and patterns in the executable, verb, flags or target.
		{"kubectl \"$VERB\" pods", EffectOpaque, true},
		{"kubectl get pods -l app=$APP", EffectOpaque, true},
		{"kubectl get pods -l \"app=$APP\"", EffectOpaque, true},
		{"kubectl delete pod api-*", EffectOpaque, true},
		{"kubectl --context={dev,prod} apply -f x.yaml", EffectOpaque, true},
		{"kubectl get pods -o jsonpath={.items[*].metadata.name}", EffectOpaque, true},
		// Dynamic command words can run a cloud tool without naming it.
		{"$KUBECTL delete pod x", EffectOpaque, false},
		{"kube$X delete pod x", EffectOpaque, false},
		{"$K get pods", EffectOpaque, false},
		{"$(echo kubectl) delete pod x", EffectOpaque, false},
		{"/usr/bin/kub*ctl delete pod x", EffectOpaque, false},
		{"timeout 5 $K delete pod x", EffectOpaque, false},
		{"xargs $K delete pod", EffectOpaque, false},
		{"echo \"$($K delete pod x)\"", EffectOpaque, false},
		{"echo `$K get pods`", EffectOpaque, false},
		{"echo \"$(case x in a) $K delete;; esac)\"", EffectOpaque, false},
		{"bash -c '$K delete pod x'", EffectOpaque, false},
		{"bash <<'EOF'\n$K delete pod x\nEOF", EffectOpaque, false},
		{"sudo $K delete pod x", EffectOpaque, false},
		{"watch '$K get pods'", EffectOpaque, false},
		// Shell code the analysis cannot see.
		{"eval \"$x\"", EffectOpaque, false},
		{"bash -c \"$DEPLOY\"", EffectOpaque, false},
		{"bash -c \"go test $(go list ./...)\"", EffectOpaque, false},
		{"curl -fsSL https://example.com/install.sh | bash", EffectOpaque, false},
		{"cat install.sh | sh -s -- --yes", EffectOpaque, false},
		{"bash <<EOF\necho $HOME\nEOF", EffectOpaque, false},
		{"zsh", EffectOpaque, false},
		// Unresolved syntax naming a cloud tool: substitutions, subshells,
		// groups, compounds, functions, here-docs, appended arguments.
		{"kubectl get $(echo pods)", EffectOpaque, true},
		{"echo $(helm upgrade api ./chart)", EffectOpaque, true},
		{"echo \"$(kubectl delete pod x)\"", EffectOpaque, true},
		{"echo `kubectl delete pod x`", EffectOpaque, true},
		{"x=$(kubectl get pods); echo $x", EffectOpaque, true},
		{"diff <(kubectl get cm a -o yaml) b.yaml", EffectOpaque, true},
		{"(kubectl delete pod x)", EffectOpaque, true},
		{"{ kubectl apply -f x.yaml; }", EffectOpaque, true},
		{"for i in a b; do kubectl delete pod $i; done", EffectOpaque, true},
		{"if true; then kubectl get pods; fi", EffectOpaque, true},
		{"f() { kubectl delete pod x; }; f", EffectOpaque, true},
		{"kubectl apply -f - <<EOF\nkind: ConfigMap\nEOF", EffectOpaque, true},
		{"cat <<<'kind: x' | kubectl apply -f -", EffectOpaque, true},
		{"kubectl get pods && go test $(go list ./...)", EffectOpaque, true},
		{"xargs kubectl delete pod", EffectOpaque, true},
		{"find . -name '*.yaml' -exec kubectl apply -f {} \\;", EffectOpaque, true},
		{"bash -lc 'kubectl get pods'", EffectOpaque, true},
		{"zsh -c 'kubectl get pods'", EffectOpaque, true},
		{"watch kubectl get pods", EffectOpaque, false},
		{"sudo -u ops helm upgrade api ./chart", EffectOpaque, false},
		{"python3 <<EOF\nimport os; os.system('kubectl delete pod x')\nEOF", EffectOpaque, false},
		{"cat <<EOF > a.txt\nhi\nEOF\nkubectl delete pod x; echo it's", EffectOpaque, true},
		{"source ./env.sh && helm list", EffectOpaque, true},
		{"bash <<'EOF'\nkubectl get pods\nEOF", EffectOpaque, true},
		// Literal wrappers and inline shell resolve to the command they run.
		{"bash -c 'kubectl apply -f x.yaml'", EffectWrite, true},
		{"bash -c 'terraform apply'", EffectWrite, true},
		{"sh -c \"kubectl get pods\"", EffectRead, true},
		{"bash -euo pipefail -c 'kubectl get pods'", EffectRead, true},
		{"eval \"kubectl delete pod x\"", EffectDestructive, true},
		{"command kubectl delete pod x", EffectDestructive, true},
		{"exec kubectl apply -f x.yaml", EffectWrite, true},
		{"env kubectl get pods", EffectRead, true},
		{"nohup kubectl port-forward svc/api 8080 &", EffectWrite, true},
		{"time kubectl get pods", EffectRead, true},
		{"timeout 10 kubectl get pods", EffectRead, true},
		{"timeout -s KILL --kill-after=5 30 kubectl get pods", EffectRead, true},
		{"timeout 5 kubectl delete pod x", EffectDestructive, true},
		{"nice -n 10 timeout 30 helm uninstall api", EffectDestructive, true},
		// Environment changes stay opaque, through wrappers too.
		{"env KUBECONFIG=/tmp/x kubectl get pods", EffectOpaque, true},
		{"env FOO=1 kubectl get pods", EffectOpaque, true},
		{"env -i kubectl get pods", EffectOpaque, true},
		{"KUBECONFIG=/tmp/x kubectl get pods", EffectOpaque, true},
		{"KUBECONFIG=/tmp/x bash -c 'kubectl get pods'", EffectOpaque, true},
		{"TF_CLI_ARGS=-destroy terraform plan", EffectOpaque, true},
		{"export KUBECONFIG=/tmp/x; kubectl --context dev apply -f x.yaml", EffectOpaque, true},
		{"KUBECONFIG=/tmp/x; kubectl get pods", EffectOpaque, true},
		{"eval 'export KUBECONFIG=/tmp/x'; kubectl get pods", EffectOpaque, true},
		// Unresolved syntax without a cloud tool keeps the non-cloud
		// classification, exactly as before cloud analysis.
		{"echo $(date)", EffectNone, false},
		{"echo `date`", EffectNone, false},
		{"go test $(go list ./...)", EffectNone, false},
		{"git commit -m \"$(cat msg.txt)\"", EffectNone, false},
		{"echo $((1 + 2))", EffectNone, false},
		{"cat <<EOF > main.go\npackage main\n\nfunc main() {\n\tfor i := 0; i < 3; i++ {\n\t}\n}\nEOF", EffectNone, false},
		{"cat > Makefile <<EOF\nbuild:\n\t$(CC) -o app main.c\nEOF", EffectNone, false},
		{"cat <<'EOF' > main.tf\nterraform {\n  required_version = \">= 1.5\"\n}\nEOF", EffectNone, false},
		{"tee charts/helm/values.yaml <<EOF\nreplicas: 2\nEOF", EffectNone, false},
		{"timeout 30 make", EffectNone, false},
		{"find . | xargs grep x", EffectNone, false},
		{"find . -name '*.go' | xargs -0 -n1 -P4 gofmt -l", EffectNone, false},
		{"bash -c 'go test ./...'", EffectNone, false},
		{"bash -lc 'echo hi'", EffectNone, false},
		{"bash -o pipefail -c 'echo hi'", EffectNone, false},
		{"bash <<'EOF'\nset -e\ngo test ./...\nEOF", EffectNone, false},
		{"(cd sub && make)", EffectNone, false},
		{"for f in *.go; do gofmt -l $f; done", EffectNone, false},
		{"[ -f go.mod ] && go test ./...", EffectNone, false},
		{"[[ -f go.mod ]] && go test ./...", EffectNone, false},
		{"export GOFLAGS=-mod=mod && go build ./...", EffectNone, false},
		{"source ./deploy.sh", EffectNone, false},
		{". ./env.sh && make", EffectNone, false},
		{"sudo make install", EffectNone, false},
		{"nohup ./server > server.log 2>&1 &", EffectNone, false},
		{"echo 'unterminated", EffectNone, false},
		// Ordinary unrelated programs keep their existing classification.
		{"bash deploy.sh", EffectNone, false},
		{"bash -x ./deploy.sh arg", EffectNone, false},
		{"command -v kubectl", EffectNone, false},
		{"env", EffectNone, false},
		{"env -u HOME", EffectNone, false},
		{"GOOS=linux go build ./...", EffectNone, false},
		{"python3 -c 'print(1)'", EffectNone, false},
		{"make deploy", EffectNone, false},
		{"ls ~/charts", EffectNone, false},
		{"echo $HOME", EffectNone, false},
		{"find . -name '*.go' -exec gofmt -l {} +", EffectNone, false},
		{"", EffectNone, false},
		// Invalid or oversized input.
		{"kubectl get pods 'oops", EffectOpaque, true},
		{"kubectl get pods >", EffectOpaque, true},
		{"echo hi;; kubectl get pods", EffectOpaque, true},
		{"kubectl get pods\x00", EffectOpaque, false},
		{"kubectl get \xff", EffectOpaque, false},
		{strings.Repeat("a ", MaxTokens+1), EffectOpaque, false},
		{"echo " + strings.Repeat("x", MaxCommandBytes), EffectOpaque, false},
	} {
		got := Analyze(test.command, "/work")
		if got.Effect != test.want || got.Cloud != test.cloud {
			t.Errorf("Analyze(%q) = effect %s cloud %v (%q); want %s cloud %v", test.command, got.Effect, got.Cloud, got.Reason, test.want, test.cloud)
		}
		if got.Effect != EffectNone && got.Reason == "" {
			t.Errorf("Analyze(%q) has no reason", test.command)
		}
	}
}

func TestEveryCompoundTargetIsReported(t *testing.T) {
	got := Analyze("kubectl --context dev get pods; helm --kube-context prod uninstall api | kubectl --context stage apply -f x.yaml", "/work")
	want := []struct {
		tool, verb, context string
		effect              Effect
	}{
		{"kubectl", "get", "dev", EffectRead},
		{"helm", "uninstall", "prod", EffectDestructive},
		{"kubectl", "apply", "stage", EffectWrite},
	}
	if got.Effect != EffectDestructive || len(got.Invocations) != len(want) {
		t.Fatalf("analysis = %+v", got)
	}
	for i, w := range want {
		inv := got.Invocations[i]
		if inv.Tool != w.tool || inv.Verb != w.verb || inv.Context != w.context || inv.Effect != w.effect || !inv.ExplicitContext {
			t.Errorf("invocation %d = %+v; want %+v", i, inv, w)
		}
	}
	// A substitution is opaque as a whole, but an unquoted inner target is
	// still reported so deny rules can match it.
	got = Analyze("echo $(kubectl delete pod x --context prod)", "/work")
	if got.Effect != EffectOpaque || len(got.Invocations) != 1 || got.Invocations[0].Context != "prod" || got.Invocations[0].Effect != EffectDestructive {
		t.Fatalf("substitution analysis = %+v", got)
	}
}

func TestInvocationAndReasonBounds(t *testing.T) {
	got := Analyze(strings.Repeat("kubectl --context dev get pods; ", MaxInvocations+1), "/work")
	if got.Effect != EffectOpaque || len(got.Invocations) != MaxInvocations {
		t.Fatalf("effect=%s invocations=%d", got.Effect, len(got.Invocations))
	}
	var many strings.Builder
	for i := range 20 {
		fmt.Fprintf(&many, "kubectl --context %s-%d delete pod a; ", strings.Repeat("x", 60), i)
	}
	got = Analyze(many.String(), "/work")
	if len(got.Reason) > maxReasonBytes || !strings.HasSuffix(got.Reason, "...") {
		t.Fatalf("reason length %d: %q", len(got.Reason), got.Reason)
	}
}

func TestContextTargetEstablishment(t *testing.T) {
	for _, test := range []struct {
		command  string
		context  string
		explicit bool
		effect   Effect
	}{
		{"kubectl --context dev apply -f x.yaml", "dev", true, EffectWrite},
		{"kubectl apply -f x.yaml --context=dev", "dev", true, EffectWrite},
		{"kubectl apply --context dev -n app -f x.yaml --server-side", "dev", true, EffectWrite},
		{"kubectl --context dev exec -it api -- sh -c 'echo --context prod'", "dev", true, EffectWrite},
		{"helm --kube-context dev upgrade --install api ./chart", "dev", true, EffectWrite},
		{"helm upgrade api ./chart --kube-context=dev -f values.yaml", "dev", true, EffectWrite},
		// No explicit target.
		{"kubectl apply -f x.yaml", "", false, EffectWrite},
		{"kubectl --context= apply -f x.yaml", "", false, EffectWrite},
		// Repeated context/kubeconfig flags are opaque even when equal.
		{"kubectl --context dev --context dev get pods", "", false, EffectOpaque},
		{"kubectl --kubeconfig a --kubeconfig a get pods", "", false, EffectOpaque},
		// Endpoint, credential and kubeconfig overrides keep the literal
		// context for deny matching but make it ineligible for grants.
		{"kubectl --kubeconfig ~/.kube/other --context dev apply -f x.yaml", "dev", false, EffectWrite},
		{"kubectl --context dev --server https://10.0.0.1 apply -f x.yaml", "dev", false, EffectWrite},
		{"kubectl --context dev -s https://10.0.0.1 apply -f x.yaml", "dev", false, EffectWrite},
		{"kubectl --context dev --cluster prod apply -f x.yaml", "dev", false, EffectWrite},
		{"kubectl --context dev --user admin apply -f x.yaml", "dev", false, EffectWrite},
		{"kubectl --context dev --as system:admin apply -f x.yaml", "dev", false, EffectWrite},
		{"kubectl --context dev --token t apply -f x.yaml", "dev", false, EffectWrite},
		{"kubectl --context dev --insecure-skip-tls-verify apply -f x.yaml", "dev", false, EffectWrite},
		{"helm --kube-context dev --kube-apiserver https://x upgrade api ./chart", "dev", false, EffectWrite},
		{"helm --kube-context dev --kube-token t upgrade api ./chart", "dev", false, EffectWrite},
		// Unknown option arity: the literal context is kept, eligibility is not.
		{"kubectl --context dev apply --unknown v -f x.yaml", "dev", false, EffectWrite},
		{"kubectl --context dev --unknown v apply -f x.yaml", "dev", false, EffectOpaque},
		// "-l" consumes "--context", so kubectl would use the ambient context.
		{"kubectl get pods -l --context prod", "prod", false, EffectRead},
		// Dynamic context values are never established.
		{"kubectl --context \"$CTX\" apply -f x.yaml", "", false, EffectOpaque},
	} {
		got := Analyze(test.command, "/work")
		if len(got.Invocations) != 1 {
			t.Errorf("Analyze(%q) invocations = %+v", test.command, got.Invocations)
			continue
		}
		inv := got.Invocations[0]
		if inv.Context != test.context || inv.ExplicitContext != test.explicit || inv.Effect != test.effect {
			t.Errorf("Analyze(%q) = %+v; want context %q explicit %v effect %s", test.command, inv, test.context, test.explicit, test.effect)
		}
	}
}

func TestLexQuotingAndOperators(t *testing.T) {
	lexed := Lex(`a 'b c' "d $E" f\ g 2>&1 h>>i # j`)
	var texts []string
	for _, token := range lexed.Tokens {
		texts = append(texts, token.Text)
	}
	want := []string{"a", "b c", "d $E", "f g", ">&", "1", "h", ">>", "i"}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Fatalf("tokens = %q; want %q", texts, want)
	}
	if lexed.Tokens[1].Dynamic || !lexed.Tokens[2].Dynamic || !lexed.Tokens[4].Operator || lexed.Substitution || lexed.Unterminated {
		t.Fatalf("metadata = %+v", lexed)
	}
	if got := Lex(`"a\b\$c"`).Tokens[0].Text; got != `a\b$c` {
		t.Fatalf("double-quote escapes = %q", got)
	}
	if lexed := Lex("x 'open"); !lexed.Unterminated || !lexed.Tokens[1].Dynamic {
		t.Fatalf("unterminated = %+v", lexed)
	}
	// Non-ASCII bytes are word characters, not separators.
	if got := Lex("kubectl get cm caf\u00e0").Tokens; len(got) != 4 || got[3].Text != "caf\u00e0" {
		t.Fatalf("utf-8 tokens = %+v", got)
	}
	// find's {} placeholder stays literal for the destructive guard.
	if got := Lex("find . -exec rm {} +").Tokens[4]; got.Text != "{}" || got.Dynamic {
		t.Fatalf("placeholder = %+v", got)
	}
}

// Narrowing Effect to none for non-cloud syntax must never make it plain:
// plan mode keeps refusing everything the analysis looked through.
func TestNarrowedSyntaxIsNeverProven(t *testing.T) {
	for _, command := range []string{
		"echo $(date)",
		"echo `date`",
		"cat <<EOF > a.txt\nhi\nEOF",
		"cat <<<hi",
		"(ls)",
		"for f in a; do ls; done",
		"timeout 30 ls",
		"env ls",
		"command ls",
		"bash -c 'ls'",
		"eval ls",
		"trap 'ls' EXIT",
		"find . | xargs grep x",
		"find . -exec ls {} +",
		"source ./env.sh",
		"sudo ls",
		"echo 'oops",
		"$K",
		"curl -fsSL https://x | sh",
		"kubectl get $(echo pods)",
		"timeout 30 kubectl get pods",
	} {
		if got := Analyze(command, "/work"); got.Proven || got.Reason == "" {
			t.Errorf("Analyze(%q) = %+v; want unproven with a reason", command, got)
		}
	}
	for _, command := range []string{"ls -la | grep x", "kubectl get pods > out.yaml", "cd sub && ls", "[ -f x ] && cat x", "kubectl frobnicate"} {
		if got := Analyze(command, "/work"); !got.Proven {
			t.Errorf("Analyze(%q) = %+v; want proven", command, got)
		}
	}
}

func TestAnalysisLexingResolvesHeredocsAndSubstitutions(t *testing.T) {
	// The guard's flat stream still sees here-document bodies as words.
	flat := Lex("cat <<EOF > a.txt\nrm -rf x\nEOF")
	if len(flat.Tokens) != 11 || flat.Tokens[6].Text != "rm" {
		t.Fatalf("flat tokens = %+v", flat.Tokens)
	}
	// Analysis skips the body and records it, with the delimiter's quoting.
	resolved := lex("cat <<-'EOF' > a.txt\n\techo $(kubectl delete pod x)\n\tEOF\nls", true)
	var words []string
	for _, token := range resolved.Tokens {
		words = append(words, token.Text)
	}
	if strings.Join(words, "|") != "cat|<<-|EOF|>|a.txt|\n|ls" || len(resolved.heredocs) != 1 || len(resolved.bodies) != 0 {
		t.Fatalf("analysis tokens = %q heredocs = %+v bodies = %q", words, resolved.heredocs, resolved.bodies)
	}
	if doc := resolved.heredocs[0]; !doc.literal || doc.body("cat <<-'EOF' > a.txt\n\techo $(kubectl delete pod x)\n\tEOF\nls") != "\techo $(kubectl delete pod x)\n" {
		t.Fatalf("heredoc = %+v", doc)
	}
	// An unquoted delimiter expands substitutions inside the body.
	if expanded := lex("cat <<EOF\n$(kubectl delete pod x)\nEOF", true); len(expanded.bodies) != 1 || expanded.bodies[0] != "kubectl delete pod x" {
		t.Fatalf("expanded bodies = %q", expanded.bodies)
	}
	// Quotes, nested substitutions and case patterns do not end $( early.
	for command, body := range map[string]string{
		`echo "$(echo ")")"`:                      `echo ")"`,
		`echo "$(echo "$(date)")"`:                `echo "$(date)"`,
		"echo $(case x in a) ls;; esac)":          "case x in a) ls;; esac",
		"echo $(cat <<EOF\n)\nEOF\n)":             "cat <<EOF\n)\nEOF\n",
		"echo `echo \\`date\\``":                  "echo `date`",
		"diff <(kubectl get cm a -o yaml) b.yaml": "kubectl get cm a -o yaml",
	} {
		if got := lex(command, true); len(got.bodies) == 0 || got.bodies[0] != body {
			t.Errorf("lex(%q) bodies = %q; want %q first", command, got.bodies, body)
		}
	}
	// A command after a quoted substitution is still analyzed.
	got := Analyze(`echo "$(echo ")")"; kubectl delete pod x`, "/work")
	if got.Effect != EffectOpaque || len(got.Invocations) != 1 || got.Invocations[0].Verb != "delete" {
		t.Fatalf("analysis = %+v", got)
	}
}
