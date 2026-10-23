package cloudcmd

import (
	"testing"
)

type effectCase struct {
	command string
	want    Effect
	verb    string // checked against the first invocation when nonempty
}

func checkEffects(t *testing.T, cases []effectCase) {
	t.Helper()
	for _, test := range cases {
		got := Analyze(test.command, "/work")
		if got.Effect != test.want {
			t.Errorf("Analyze(%q).Effect = %s; want %s (reason %q)", test.command, got.Effect, test.want, got.Reason)
			continue
		}
		if !got.Cloud {
			t.Errorf("Analyze(%q).Cloud = false", test.command)
		}
		if test.verb != "" && (len(got.Invocations) == 0 || got.Invocations[0].Verb != test.verb) {
			t.Errorf("Analyze(%q).Invocations = %+v; want verb %q", test.command, got.Invocations, test.verb)
		}
	}
}

func TestCloudVerbTableKubectl(t *testing.T) {
	checkEffects(t, []effectCase{
		// Read forms and their closed options.
		{"kubectl get pods", EffectRead, "get"},
		{"kubectl --context dev get pods", EffectRead, "get"},
		{"kubectl get pods --context=dev", EffectRead, "get"},
		{"kubectl get pods -n kube-system -o yaml", EffectRead, "get"},
		{"kubectl get pods -oyaml -A --show-labels", EffectRead, "get"},
		{"kubectl get pods -n=default -l app=api --field-selector status.phase=Running", EffectRead, "get"},
		{"kubectl get pods -o jsonpath='{.items[*].metadata.name}'", EffectRead, "get"},
		{"kubectl get pods -w", EffectRead, "get"},
		{"kubectl get -f manifest.yaml", EffectRead, "get"},
		{"kubectl describe deploy api -n prod", EffectRead, "describe"},
		{"kubectl logs -f api-123 -c app --tail 50", EffectRead, "logs"},
		{"kubectl logs api -p --since=1h", EffectRead, "logs"},
		{"kubectl explain pods.spec --recursive", EffectRead, "explain"},
		{"kubectl api-resources --namespaced=true -o wide", EffectRead, "api-resources"},
		{"kubectl api-versions", EffectRead, "api-versions"},
		{"kubectl cluster-info", EffectRead, "cluster-info"},
		{"kubectl version --client", EffectRead, "version"},
		{"kubectl version", EffectRead, "version"},
		{"kubectl config view --minify", EffectRead, "config view"},
		{"kubectl config current-context", EffectRead, "config current-context"},
		{"kubectl config get-contexts -o name", EffectRead, "config get-contexts"},
		{"kubectl config get-clusters", EffectRead, "config get-clusters"},
		{"kubectl config get-users", EffectRead, "config get-users"},
		{"kubectl rollout status deploy/api", EffectRead, "rollout status"},
		{"kubectl rollout history deploy/api --revision 3", EffectRead, "rollout history"},
		// Help and bare forms print usage only.
		{"kubectl", EffectRead, ""},
		{"kubectl apply --help", EffectRead, "apply"},
		{"kubectl delete pod x -h", EffectRead, "delete"},
		{"kubectl rollout", EffectRead, "rollout"},
		// -f takes a value, so "-h" is a filename here, not help.
		{"kubectl apply -f -h", EffectWrite, "apply"},
		// Unknown options, subcommands and --raw never inherit read.
		{"kubectl get --raw /api/v1/namespaces", EffectOpaque, "get"},
		{"kubectl config view --raw", EffectOpaque, "config view"},
		{"kubectl get pods --unknown-flag", EffectOpaque, "get"},
		{"kubectl get pods -k overlays/dev", EffectOpaque, "get"},
		{"kubectl --cache-dir /tmp get pods", EffectOpaque, ""},
		{"kubectl cluster-info dump", EffectOpaque, "cluster-info"},
		{"kubectl api-versions extra", EffectOpaque, "api-versions"},
		{"kubectl get pods -- extra", EffectOpaque, "get"},
		{"kubectl top pods", EffectOpaque, "top"},
		{"kubectl auth can-i list pods", EffectOpaque, "auth"},
		{"kubectl kustomize overlays/dev", EffectOpaque, "kustomize"},
		{"kubectl run x --image nginx", EffectOpaque, "run"},
		{"kubectl rollout frobnicate deploy/api", EffectOpaque, "rollout frobnicate"},
		{"kubectl config frobnicate", EffectOpaque, "config frobnicate"},
		// Write forms: prompt by default.
		{"kubectl apply -f deploy.yaml", EffectWrite, "apply"},
		{"kubectl apply -f deploy.yaml --dry-run=client", EffectWrite, "apply"},
		{"kubectl apply -f deploy.yaml --dry-run=server", EffectWrite, "apply"},
		{"kubectl create deployment api --image nginx", EffectWrite, "create"},
		{"kubectl create -f x.yaml --dry-run=client -o yaml", EffectWrite, "create"},
		{"kubectl replace -f deploy.yaml", EffectWrite, "replace"},
		{"kubectl replace --force=false -f deploy.yaml", EffectWrite, "replace"},
		{"kubectl patch deploy api -p '{\"spec\":{}}'", EffectWrite, "patch"},
		{"kubectl edit deploy api", EffectWrite, "edit"},
		{"kubectl label pod api tier=web", EffectWrite, "label"},
		{"kubectl annotate pod api note=x", EffectWrite, "annotate"},
		{"kubectl scale deploy api --replicas 3", EffectWrite, "scale"},
		{"kubectl autoscale deploy api --min 2 --max 5", EffectWrite, "autoscale"},
		{"kubectl set image deploy/api app=nginx:2", EffectWrite, "set"},
		{"kubectl rollout pause deploy/api", EffectWrite, "rollout pause"},
		{"kubectl rollout resume deploy/api", EffectWrite, "rollout resume"},
		{"kubectl exec -it api -- sh -c 'rm -rf /tmp/x'", EffectWrite, "exec"},
		{"kubectl attach api -i", EffectWrite, "attach"},
		{"kubectl cp api:/tmp/x ./x", EffectWrite, "cp"},
		{"kubectl port-forward svc/api 8080:80", EffectWrite, "port-forward"},
		{"kubectl proxy --port 8001", EffectWrite, "proxy"},
		{"kubectl debug api --image busybox", EffectWrite, "debug"},
		{"kubectl diff -f deploy.yaml", EffectWrite, "diff"},
		{"kubectl config use-context prod", EffectWrite, "config use-context"},
		{"kubectl config set-context dev --namespace x", EffectWrite, "config set-context"},
		{"kubectl config delete-context old", EffectWrite, "config delete-context"},
		// Destructive forms: per-call guard approval.
		{"kubectl delete pod api", EffectDestructive, "delete"},
		{"kubectl delete pod api --dry-run=server", EffectDestructive, "delete"},
		{"kubectl drain node-1 --ignore-daemonsets", EffectDestructive, "drain"},
		{"kubectl cordon node-1", EffectDestructive, "cordon"},
		{"kubectl uncordon node-1", EffectDestructive, "uncordon"},
		{"kubectl taint nodes node-1 key=value:NoSchedule", EffectDestructive, "taint"},
		{"kubectl replace --force -f deploy.yaml", EffectDestructive, "replace"},
		{"kubectl replace -f deploy.yaml --force=true", EffectDestructive, "replace"},
		{"kubectl rollout undo deploy/api", EffectDestructive, "rollout undo"},
		{"kubectl rollout restart deploy/api", EffectDestructive, "rollout restart"},
	})
}

func TestCloudVerbTableHelm(t *testing.T) {
	checkEffects(t, []effectCase{
		{"helm list", EffectRead, "list"},
		{"helm ls -A -o json", EffectRead, "list"},
		{"helm status api --show-resources", EffectRead, "status"},
		{"helm history api --max 5", EffectRead, "history"},
		{"helm get all api", EffectRead, "get all"},
		{"helm get values api -a -o yaml", EffectRead, "get values"},
		{"helm get manifest api --revision 2", EffectRead, "get manifest"},
		{"helm get hooks api", EffectRead, "get hooks"},
		{"helm get notes api", EffectRead, "get notes"},
		{"helm get metadata api", EffectRead, "get metadata"},
		{"helm show all ./chart", EffectRead, "show all"},
		{"helm show chart ./chart --version 1.2.3", EffectRead, "show chart"},
		{"helm show values ./chart --jsonpath '{.image}'", EffectRead, "show values"},
		{"helm inspect readme ./chart", EffectRead, "show readme"},
		{"helm version --short", EffectRead, "version"},
		{"helm", EffectRead, ""},
		{"helm upgrade --help", EffectRead, "upgrade"},
		// Unknown get/show subverbs are not matched by prefix.
		{"helm get frobnicate api", EffectOpaque, "get frobnicate"},
		{"helm show crds ./chart", EffectOpaque, "show crds"},
		{"helm getvalues api", EffectOpaque, "getvalues"},
		{"helm repo list", EffectOpaque, "repo list"},
		{"helm dependency list ./chart", EffectOpaque, "dependency list"},
		{"helm plugin install https://x", EffectOpaque, "plugin"},
		{"helm search repo nginx", EffectOpaque, "search"},
		{"helm list --unknown", EffectOpaque, "list"},
		// template: read only under restricted flags.
		{"helm template api ./chart -f values.yaml --set image.tag=1 -n prod", EffectRead, "template"},
		{"helm template api ./chart --dry-run=client --include-crds", EffectRead, "template"},
		{"helm template api ./chart --output-dir out", EffectWrite, "template"},
		{"helm template api ./chart --dependency-update", EffectWrite, "template"},
		{"helm template api ./chart --validate", EffectWrite, "template"},
		{"helm template api ./chart --dry-run=server", EffectWrite, "template"},
		{"helm template api ./chart --post-renderer ./render.sh", EffectOpaque, "template"},
		{"helm template api ./chart --post-renderer=kustomize --post-renderer-args build", EffectOpaque, "template"},
		{"helm template api ./chart --repo https://charts.example", EffectOpaque, "template"},
		{"helm template api ./chart --dry-run=bogus", EffectOpaque, "template"},
		// Writes.
		{"helm install api ./chart", EffectWrite, "install"},
		{"helm upgrade api ./chart -f values.yaml", EffectWrite, "upgrade"},
		{"helm upgrade --install api ./chart", EffectWrite, "upgrade"},
		{"helm --kube-context dev upgrade --install api ./chart", EffectWrite, "upgrade"},
		{"helm upgrade --force=false api ./chart", EffectWrite, "upgrade"},
		{"helm upgrade api ./chart --post-renderer ./render.sh", EffectOpaque, "upgrade"},
		{"helm rollback api 2", EffectWrite, "rollback"},
		{"helm test api", EffectWrite, "test"},
		{"helm lint ./chart", EffectWrite, "lint"},
		{"helm package ./chart", EffectWrite, "package"},
		{"helm pull bitnami/nginx", EffectWrite, "pull"},
		{"helm fetch bitnami/nginx", EffectWrite, "pull"},
		{"helm dependency update ./chart", EffectWrite, "dependency update"},
		{"helm dep build ./chart", EffectWrite, "dependency build"},
		{"helm repo add bitnami https://charts.bitnami.com", EffectWrite, "repo add"},
		{"helm repo update", EffectWrite, "repo update"},
		{"helm repo remove bitnami", EffectWrite, "repo remove"},
		{"helm registry login ghcr.io", EffectWrite, "registry login"},
		{"helm registry logout ghcr.io", EffectWrite, "registry logout"},
		// Destructive.
		{"helm uninstall api", EffectDestructive, "uninstall"},
		{"helm delete api", EffectDestructive, "uninstall"},
		{"helm del api", EffectDestructive, "uninstall"},
		{"helm upgrade --install api ./chart --force", EffectDestructive, "upgrade"},
		{"helm install api ./chart --replace", EffectDestructive, "install"},
	})
}

func TestCloudVerbTableTerraform(t *testing.T) {
	checkEffects(t, []effectCase{
		{"terraform version", EffectRead, "version"},
		{"terraform -version", EffectRead, "version"},
		{"terraform version -json", EffectRead, "version"},
		{"terraform show", EffectRead, "show"},
		{"terraform show -json tf.plan", EffectRead, "show"},
		{"terraform output", EffectRead, "output"},
		{"terraform output -raw db_password", EffectRead, "output"},
		{"terraform output --json", EffectRead, "output"},
		{"terraform state list", EffectRead, "state list"},
		{"terraform state show aws_instance.web", EffectRead, "state show"},
		{"terraform workspace list", EffectRead, "workspace list"},
		{"terraform workspace show", EffectRead, "workspace show"},
		{"terraform fmt -check", EffectRead, "fmt"},
		{"terraform fmt -check -diff -recursive modules", EffectRead, "fmt"},
		{"terraform -chdir=infra show -no-color", EffectRead, "show"},
		{"terraform", EffectRead, ""},
		{"terraform apply -help", EffectRead, ""},
		// Plan and validate may run providers: never read.
		{"terraform plan", EffectWrite, "plan"},
		{"terraform plan -out=tf.plan", EffectWrite, "plan"},
		{"terraform validate", EffectWrite, "validate"},
		{"terraform init", EffectWrite, "init"},
		{"terraform apply", EffectWrite, "apply"},
		{"terraform apply -auto-approve", EffectWrite, "apply"},
		{"terraform import aws_instance.web i-123", EffectWrite, "import"},
		{"terraform refresh", EffectWrite, "refresh"},
		{"terraform fmt", EffectWrite, "fmt"},
		{"terraform fmt -diff", EffectWrite, "fmt"},
		{"terraform fmt -check=false", EffectWrite, "fmt"},
		{"terraform fmt -check -write=true", EffectWrite, "fmt"},
		{"terraform workspace new staging", EffectWrite, "workspace new"},
		{"terraform workspace select staging", EffectWrite, "workspace select"},
		{"terraform providers", EffectWrite, "providers"},
		{"terraform providers lock", EffectWrite, "providers"},
		{"terraform graph", EffectWrite, "graph"},
		{"terraform -chdir=infra plan", EffectWrite, "plan"},
		// Destructive.
		{"terraform destroy", EffectDestructive, "destroy"},
		{"terraform apply -destroy", EffectDestructive, "apply"},
		{"terraform plan -destroy -out=x", EffectDestructive, "plan"},
		{"terraform apply -destroy=false", EffectWrite, "apply"},
		{"terraform taint aws_instance.web", EffectDestructive, "taint"},
		{"terraform untaint aws_instance.web", EffectDestructive, "untaint"},
		{"terraform force-unlock 1234", EffectDestructive, "force-unlock"},
		{"terraform state rm aws_instance.web", EffectDestructive, "state rm"},
		{"terraform state mv a b", EffectDestructive, "state mv"},
		{"terraform state push x.tfstate", EffectDestructive, "state push"},
		{"terraform state replace-provider a b", EffectDestructive, "state replace-provider"},
		{"terraform workspace delete staging", EffectDestructive, "workspace delete"},
		// Opaque: unknown verbs/options, plugin overrides, uncontained -chdir.
		{"terraform console", EffectOpaque, "console"},
		{"terraform state pull", EffectOpaque, "state pull"},
		{"terraform state -lock=false rm x", EffectOpaque, "state"},
		{"terraform output -state=other.tfstate", EffectOpaque, "output"},
		{"terraform show a b", EffectOpaque, "show"},
		{"terraform init -plugin-dir=/opt/plugins", EffectOpaque, ""},
		{"terraform -chdir=../other plan", EffectOpaque, ""},
		{"terraform -chdir=/etc show", EffectOpaque, ""},
		{"terraform -chdir infra show", EffectOpaque, ""},
		{"terraform -chdir=a -chdir=b show", EffectOpaque, ""},
		{"terraform -no-color show", EffectOpaque, ""},
	})
}

func TestCloudVerbTableKustomize(t *testing.T) {
	checkEffects(t, []effectCase{
		{"kustomize version", EffectRead, "version"},
		{"kustomize", EffectRead, ""},
		{"kustomize build --help", EffectRead, ""},
		{"kustomize edit set image api=nginx:2", EffectWrite, "edit"},
		{"kustomize create --autodetect", EffectWrite, "create"},
		{"kustomize cfg tree .", EffectWrite, "cfg"},
		// Build may fetch remotes or run plugins: opaque even when local.
		{"kustomize build overlays/dev", EffectOpaque, "build"},
		{"kustomize build --enable-helm overlays/dev", EffectOpaque, "build"},
		{"kustomize build --enable-alpha-plugins --enable-exec .", EffectOpaque, "build"},
		{"kustomize build --load-restrictor=LoadRestrictionsNone https://github.com/x/y", EffectOpaque, "build"},
		{"kustomize version --short", EffectOpaque, ""},
		{"kustomize localize .", EffectOpaque, "localize"},
	})
}

func TestOtherCloudCLIsAreNotClassified(t *testing.T) {
	for _, command := range []string{
		"aws s3 rm s3://bucket/key --recursive",
		"gcloud container clusters delete prod",
		"az group delete -n prod",
		"kubectl-neat get pods",
		"echo kubectl delete pod x",
		"grep -r 'helm uninstall' docs",
	} {
		got := Analyze(command, "/work")
		if got.Effect != EffectNone || got.Cloud || len(got.Invocations) != 0 {
			t.Errorf("Analyze(%q) = %+v; want unclassified", command, got)
		}
	}
}

func TestTerraformChdirFollowsLiteralCd(t *testing.T) {
	for _, test := range []struct {
		command, cwd string
		want         Effect
	}{
		{"cd infra && terraform -chdir=.. show", "/work", EffectRead},
		{"cd .. && terraform -chdir=work/infra show", "/work", EffectRead},
		{"cd .. && terraform -chdir=other show", "/work", EffectOpaque},
		// The cd may fail or run elsewhere: the directory is then unknown.
		{"cd infra; terraform -chdir=.. show", "/work", EffectOpaque},
		{"cd infra || terraform -chdir=modules show", "/work", EffectOpaque},
		{"cd infra | terraform -chdir=modules show", "/work", EffectOpaque},
		{"cd \"$DIR\" && terraform -chdir=modules show", "/work", EffectOpaque},
		{"cd && terraform -chdir=modules show", "/work", EffectOpaque},
		// No session cwd: relative, non-escaping paths only.
		{"terraform -chdir=infra show", "", EffectRead},
		{"terraform -chdir=../infra show", "", EffectOpaque},
		{"terraform -chdir=/work/infra show", "", EffectOpaque},
		{"terraform -chdir=/work/infra show", "/work", EffectRead},
	} {
		if got := Analyze(test.command, test.cwd); got.Effect != test.want {
			t.Errorf("Analyze(%q, %q) = %s (%s); want %s", test.command, test.cwd, got.Effect, got.Reason, test.want)
		}
	}
}
