package manifest

import (
	"context"
	"encoding/json"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

var updateGoldens = flag.Bool("update", false, "rewrite manifest golden files")

// defaultOptions are the tool's documented defaults.
var defaultOptions = Options{ContextLines: 3, MaxResources: 50, RedactSecrets: true}

// goldenCase compares two in-memory trees. Each side is the root of its
// filesystem unless a selection is named.
type goldenCase struct {
	name                  string
	before, after         fstest.MapFS
	beforePath, afterPath string
	options               *Options
}

// files builds a filesystem from name/content pairs.
func files(pairs ...string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for i := 0; i+1 < len(pairs); i += 2 {
		fsys[pairs[i]] = &fstest.MapFile{Data: []byte(pairs[i+1])}
	}
	return fsys
}

func selectView(fsys fstest.MapFS, selected string) fs.FS {
	if selected == "" {
		return fsys
	}
	return View(fsys, selected)
}

// render is the golden form of a comparison: indented JSON or the error.
func render(t *testing.T, test goldenCase) string {
	t.Helper()
	options := defaultOptions
	if test.options != nil {
		options = *test.options
	}
	result, err := Compare(context.Background(), selectView(test.before, test.beforePath), selectView(test.after, test.afterPath), options)
	if err != nil {
		return "error: " + err.Error() + "\n"
	}
	rendered, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(rendered) + "\n"
}

// checkGolden compares a rendering with testdata/golden/<name>.golden.
func checkGolden(t *testing.T, name, rendered string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".golden")
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(rendered), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(want) != rendered {
		t.Fatalf("%s mismatch\n--- want\n%s\n--- got\n%s", name, want, rendered)
	}
}

const deployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 2
  template:
    spec:
      containers:
      - name: api
        image: api:1
      - name: sidecar
        image: proxy:1
`

const service = `apiVersion: v1
kind: Service
metadata: {name: api, namespace: default}
spec: {ports: [{port: 80}]}
`

// TestManifestGoldenDecode covers decoding, identity and validation.
func TestManifestGoldenDecode(t *testing.T) {
	empty := files()
	for _, test := range []goldenCase{
		{name: "decode_multidoc_cross_file", before: empty, after: files(
			"b.yaml", "# leading comment\n---\n"+service+"---\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: z}\n",
			"a/x.json", `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a"}}`,
			"notes.txt", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: ignored}\n",
		)},
		{name: "decode_nested_core_lists", before: empty, after: files("list.yaml", `apiVersion: v1
kind: List
metadata: {resourceVersion: "1"}
items:
- apiVersion: v1
  kind: ConfigMap
  metadata: {name: one}
- apiVersion: v1
  kind: List
  items:
  - apiVersion: v1
    kind: ConfigMap
    metadata: {name: two}
`)},
		{name: "decode_crd_list_not_expanded", before: empty, after: files("crd.yaml", `apiVersion: example.com/v1
kind: WidgetList
metadata: {name: widgets}
items:
- apiVersion: example.com/v1
  kind: Widget
  metadata: {name: hidden}
`)},
		{name: "decode_empty_inputs", before: files("empty.yaml", "", "comments.yml", "# nothing\n", "nulls.yaml", "---\n---\n"), after: empty},
		{name: "decode_namespace_absent_vs_default", before: files("a.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\n"), after: files("a.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c, namespace: default}\n")},
		{name: "decode_version_change_is_add_remove", before: files("d.yaml", "apiVersion: apps/v1beta1\nkind: Deployment\nmetadata: {name: api}\n"), after: files("d.yaml", "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: api}\n")},
		{name: "decode_scalar_types", before: files("s.yaml", `apiVersion: v1
kind: ConfigMap
metadata: {name: types}
data:
  quoted_int: "1"
  int: 1
  octal: 0o17
  hex: 0x0F
  big: 123456789012345678901234567890
  float: 1.50
  bool: true
  null_value: null
  when: 2001-12-14
`), after: files("s.yaml", `apiVersion: v1
kind: ConfigMap
metadata: {name: types}
data:
  quoted_int: 1
  int: 1
  octal: 15
  hex: 15
  big: 123456789012345678901234567891
  float: 1.5
  bool: "true"
  null_value: ~
  when: 2001-12-14
`)},
		{name: "decode_aliases_and_merge_keys", before: files("m.yaml", `apiVersion: v1
kind: ConfigMap
metadata: {name: merged}
data:
  base: &base {a: "1", b: "2"}
  extra: &extra {b: "from-extra", c: "3"}
  combined:
    <<: [*base, *extra]
    a: explicit
  copy: *base
`), after: files("m.yaml", `apiVersion: v1
kind: ConfigMap
metadata: {name: merged}
data:
  base: {a: "1", b: "2"}
  extra: {b: "from-extra", c: "3"}
  combined: {a: explicit, b: "2", c: "3"}
  copy: {b: "2", a: "1"}
`)},
		{name: "decode_duplicate_resource_across_files", before: files("one.yaml", service, "two/again.yaml", "---\n"+service), after: empty},
		{name: "decode_duplicate_key", before: files("dup.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: d}\ndata: {k: a, k: b}\n"), after: empty},
		{name: "decode_generate_name", before: files("gen.yaml", "apiVersion: v1\nkind: Pod\nmetadata: {generateName: job-}\n"), after: empty},
		{name: "decode_missing_kind", before: files("bad.yaml", "apiVersion: v1\nmetadata: {name: x}\n"), after: empty},
		{name: "decode_non_mapping_document", before: files("seq.yaml", "- a\n- b\n"), after: empty},
		{name: "decode_namespace_not_string", before: files("ns.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x, namespace: 5}\n"), after: empty},
		{name: "decode_custom_tag", before: files("tag.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\ndata: {k: !Ref thing}\n"), after: empty},
		{name: "decode_non_string_key", before: files("key.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\ndata: {1: one}\n"), after: empty},
		{name: "decode_non_finite_float", before: files("inf.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\nspec: {limit: .inf}\n"), after: empty},
		{name: "decode_cyclic_alias", before: files("cycle.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\ndata: &loop [*loop]\n"), after: empty},
		{name: "decode_malformed_yaml", before: empty, after: files("broken.yaml", service+"---\nkey: [unclosed\n")},
		{name: "decode_selected_file_any_extension", before: files("render.out", service, "other.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: other}\n"), beforePath: "render.out", after: files("dir/svc.yaml", service), afterPath: "dir"},
	} {
		t.Run(test.name, func(t *testing.T) { checkGolden(t, test.name, render(t, test)) })
	}
}

// TestManifestGoldenDiff covers classification, patches and redaction.
func TestManifestGoldenDiff(t *testing.T) {
	reordered := `# a different comment and key order
kind: Deployment
apiVersion: apps/v1
spec:
  template:
    spec:
      containers:
      - {image: "api:1", name: api}
      - image: proxy:1
        name: sidecar
  replicas: 2
metadata: {name: api}
`
	swapped := strings.Replace(deployment, "      - name: api\n        image: api:1\n      - name: sidecar\n        image: proxy:1\n", "      - name: sidecar\n        image: proxy:1\n      - name: api\n        image: api:1\n", 1)
	secret := func(password, mode string) string {
		return "apiVersion: v1\nkind: Secret\nmetadata: {name: creds, labels: {mode: " + mode + "}}\ndata: {password: " + password + "}\nstringData: {token: plain-" + password + "}\n"
	}
	unredacted := Options{ContextLines: 1, MaxResources: 50, RedactSecrets: false}
	for _, test := range []goldenCase{
		{name: "diff_key_order_and_comments_equal", before: files("d.yaml", deployment), after: files("d.yaml", reordered)},
		{name: "diff_list_reorder_is_change", before: files("d.yaml", deployment), after: files("d.yaml", swapped)},
		{name: "diff_counts_and_sources", before: files("old/deploy.yaml", deployment, "old/svc.yaml", service, "old/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: gone}\n"), beforePath: "old",
			after: files("new/all.yaml", strings.Replace(deployment, "replicas: 2", "replicas: 3\n  paused: false\n  status: null", 1)+"---\n"+service+"---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: fresh}\ndata: {}\n"), afterPath: "new"},
		{name: "diff_secret_only_change_redacted", before: files("s.yaml", secret("c2VjcmV0MQ==", "a")), after: files("s.yaml", secret("c2VjcmV0Mg==", "a"))},
		{name: "diff_secret_redacted_with_other_change", before: files("s.yaml", secret("c2VjcmV0MQ==", "a")), after: files("s.yaml", secret("c2VjcmV0Mg==", "b"))},
		{name: "diff_secret_unredacted", before: files("s.yaml", secret("c2VjcmV0MQ==", "a")), after: files("s.yaml", secret("c2VjcmV0Mg==", "a")), options: &unredacted},
		{name: "diff_context_zero", before: files("d.yaml", deployment), after: files("d.yaml", strings.Replace(deployment, "replicas: 2", "replicas: 5", 1)), options: &Options{ContextLines: 0, MaxResources: 50, RedactSecrets: true}},
	} {
		t.Run(test.name, func(t *testing.T) { checkGolden(t, test.name, render(t, test)) })
	}
}
