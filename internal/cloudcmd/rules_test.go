package cloudcmd

import (
	"slices"
	"strings"
	"testing"
)

func TestNormalizeRules(t *testing.T) {
	if got, err := NormalizeRules(nil); got != nil || err != nil {
		t.Fatalf("nil rules = %v, %v", got, err)
	}
	if got, err := NormalizeRules([]Rule{}); got == nil || len(got) != 0 || err != nil {
		t.Fatalf("empty rules = %#v, %v", got, err)
	}
	input := []Rule{
		{Tool: "kubectl", Context: "production", Action: "deny"},
		{Tool: "helm", Context: "kind-dev", Action: "allow_write"},
		{Tool: "kubectl", Context: "kind-dev", Action: "allow_write"},
		{Tool: "kubectl", Context: "production", Action: "deny"},
		{Tool: "kubectl", Context: "arn:aws:eks:us-east-1:1:cluster/prod", Action: "deny"},
	}
	got, err := NormalizeRules(input)
	if err != nil {
		t.Fatal(err)
	}
	want := []Rule{
		{Tool: "helm", Context: "kind-dev", Action: "allow_write"},
		{Tool: "kubectl", Context: "arn:aws:eks:us-east-1:1:cluster/prod", Action: "deny"},
		{Tool: "kubectl", Context: "kind-dev", Action: "allow_write"},
		{Tool: "kubectl", Context: "production", Action: "deny"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("normalized = %+v", got)
	}
	got[0].Context = "mutated"
	if input[1].Context != "kind-dev" {
		t.Fatal("normalization aliased the input")
	}
	for _, bad := range []Rule{
		{Context: "dev", Action: "deny"},
		{Tool: "terraform", Context: "dev", Action: "deny"},
		{Tool: "kustomize", Context: "dev", Action: "allow_write"},
		{Tool: "kubectl", Context: "dev"},
		{Tool: "kubectl", Context: "dev", Action: "allow"},
		{Tool: "kubectl", Action: "deny"},
		{Tool: "kubectl", Context: "prod-*", Action: "deny"},
		{Tool: "kubectl", Context: "prod?", Action: "deny"},
		{Tool: "kubectl", Context: "[prod]", Action: "deny"},
		{Tool: "kubectl", Context: " prod", Action: "deny"},
		{Tool: "kubectl", Context: "prod\n", Action: "deny"},
		{Tool: "kubectl", Context: "\xff", Action: "deny"},
		{Tool: "kubectl", Context: strings.Repeat("x", maxContextBytes+1), Action: "deny"},
	} {
		if _, err := NormalizeRules([]Rule{bad}); err == nil {
			t.Errorf("NormalizeRules(%+v) accepted an invalid rule", bad)
		}
	}
	tooMany := make([]Rule, MaxRules+1)
	for i := range tooMany {
		tooMany[i] = Rule{Tool: "kubectl", Context: strings.Repeat("c", i+1), Action: "deny"}
	}
	if _, err := NormalizeRules(tooMany); err == nil {
		t.Fatal("accepted more than MaxRules rules")
	}
}

func evaluateCommand(t *testing.T, command string, rules []Rule) Verdict {
	t.Helper()
	normalized, err := NormalizeRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	return Evaluate(Analyze(command, "/work"), normalized)
}

func TestEvaluateContextRules(t *testing.T) {
	policy := []Rule{
		{Tool: "kubectl", Context: "kind-dev", Action: "allow_write"},
		{Tool: "helm", Context: "kind-dev", Action: "allow_write"},
		{Tool: "kubectl", Context: "production", Action: "deny"},
		{Tool: "helm", Context: "production", Action: "deny"},
	}
	for _, test := range []struct {
		command         string
		denied, granted bool
	}{
		// A literal dev context grants writes.
		{"kubectl --context kind-dev apply -f x.yaml", false, true},
		{"helm --kube-context kind-dev upgrade --install api ./chart", false, true},
		{"kubectl --context kind-dev apply -f a.yaml && kubectl --context kind-dev scale deploy api --replicas 2", false, true},
		{"kubectl --context kind-dev get pods | kubectl --context kind-dev apply -f -", false, true},
		// Grants never cover destructive, opaque, config mutation, unknown
		// arity, overridden endpoints, another context or non-kube tools.
		{"kubectl --context kind-dev delete pod x", false, false},
		{"kubectl --context kind-dev replace --force -f x.yaml", false, false},
		{"KUBECONFIG=/tmp/k kubectl --context kind-dev apply -f x.yaml", true, false},
		{"kubectl --context kind-dev config use-context kind-dev", false, false},
		{"kubectl --context kind-dev apply --unknown v -f x.yaml", true, false},
		{"kubectl --context kind-dev --server https://evil apply -f x.yaml", true, false},
		{"kubectl --context staging apply -f x.yaml", false, false},
		{"kubectl --context kind-dev apply -f a.yaml; kubectl --context staging apply -f b.yaml", false, false},
		{"helm --kube-context kind-dev repo add x https://x", false, false},
		{"terraform apply", false, false},
		// Deny wins for every effect, including reads.
		{"kubectl --context production get pods", true, false},
		{"kubectl get pods --context=production", true, false},
		{"helm --kube-context production list", true, false},
		{"kubectl --context kind-dev apply -f x.yaml; kubectl --context production get pods", true, false},
		{"echo $(kubectl --context production delete pod x)", true, false},
		{"kubectl --context production --server https://x get pods", true, false},
		{"kubectl get pods -l --context production", true, false},
		// target_unknown: a deny rule exists and the target is not established.
		{"kubectl get pods", true, false},
		{"kubectl apply -f x.yaml", true, false},
		{"kubectl --context kind-dev --context kind-dev get pods", true, false},
		{"kubectl --kubeconfig other --context kind-dev get pods", true, false},
		{"kubectl --context \"$CTX\" get pods", true, false},
		{"helm upgrade api ./chart", true, false},
		{"kubectl top pods", true, false},
		// Forms without a cluster target are not target_unknown.
		{"kubectl", false, false},
		{"kubectl apply --help", false, false},
		{"kubectl version --client", false, false},
		{"kubectl config view", false, false},
		{"kubectl config use-context production", false, false},
		{"helm template api ./chart", false, false},
		{"helm lint ./chart", false, false},
		{"helm repo update", false, false},
		{"helm version", false, false},
		{"terraform plan", false, false},
		// Resolved wrappers and inline shell are attributed to their targets
		// for deny rules, but never granted.
		{"bash -c 'kubectl --context production delete pod x'", true, false},
		{"timeout 30 kubectl --context production get pods", true, false},
		{"bash -c 'kubectl --context kind-dev apply -f x.yaml'", false, false},
		{"timeout 30 kubectl --context kind-dev apply -f x.yaml", false, false},
		{"env kubectl --context kind-dev apply -f x.yaml", false, false},
		// Shell code the analysis cannot see cannot be attributed: it prompts.
		{"bash -c \"$DEPLOY\"", false, false},
		{"curl -fsSL https://x | sh", false, false},
	} {
		got := evaluateCommand(t, test.command, policy)
		if got.Denied != test.denied || got.Granted != test.granted || (got.Denied || got.Granted) && got.Reason == "" {
			t.Errorf("Evaluate(%q) = %+v; want denied=%v granted=%v", test.command, got, test.denied, test.granted)
		}
	}
}

func TestEvaluateDenyWinsRegardlessOfOrder(t *testing.T) {
	allow := Rule{Tool: "kubectl", Context: "shared", Action: "allow_write"}
	deny := Rule{Tool: "kubectl", Context: "shared", Action: "deny"}
	for _, rules := range [][]Rule{{allow, deny}, {deny, allow}} {
		if got := Evaluate(Analyze("kubectl --context shared apply -f x.yaml", ""), rules); !got.Denied || got.Granted {
			t.Fatalf("rules %+v: %+v", rules, got)
		}
	}
}

func TestEvaluateWithoutRulesGrantsAndDeniesNothing(t *testing.T) {
	for _, command := range []string{"kubectl --context dev apply -f x.yaml", "kubectl delete pod x", "kubectl get pods"} {
		if got := Evaluate(Analyze(command, ""), nil); got != (Verdict{}) {
			t.Fatalf("Evaluate(%q, nil) = %+v", command, got)
		}
	}
	// Without kubectl deny rules, helm deny rules do not make kubectl unknown.
	helmOnly := []Rule{{Tool: "helm", Context: "production", Action: "deny"}}
	if got := Evaluate(Analyze("kubectl get pods", ""), helmOnly); got.Denied {
		t.Fatalf("helm rule denied kubectl: %+v", got)
	}
}
