package cloudcmd

import "testing"

func TestInlineInterpreterCloudTargetsAreUnknown(t *testing.T) {
	rules := []Rule{{Tool: ToolKubectl, Context: "production", Action: ActionDeny}}
	for _, code := range []string{
		`import subprocess; subprocess.run(['kubectl', '--context', 'production', 'delete', 'pod', 'api'])`,
		`require('child_process').execSync('KUBECTL --context dev get pods')`,
		`command = 'ku''bectl'`,
	} {
		analysis := AnalyzeInline(code)
		if analysis.Effect != EffectOpaque || !Evaluate(analysis, rules).Denied {
			t.Fatalf("inline code escaped cloud deny: %+v", analysis)
		}
	}
	for _, command := range []string{
		`python3 -c "import subprocess; subprocess.run(['kubectl', 'get', 'pods'])"`,
		`node -e "require('child_process').execSync('kubectl get pods')"`,
		`bun -e "require('child_process').execSync('kubectl get pods')"`,
	} {
		analysis := Analyze(command, t.TempDir())
		if analysis.Effect != EffectOpaque || !Evaluate(analysis, rules).Denied {
			t.Fatalf("bash inline code escaped cloud deny: %s: %+v", command, analysis)
		}
	}
	for _, code := range []string{"1 + 1", "print('kubectl-plugin')", "print('kubectl/templates')"} {
		if analysis := AnalyzeInline(code); analysis.Effect != EffectNone {
			t.Fatalf("non-cloud cell is opaque: %s: %+v", code, analysis)
		}
	}
}
