package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func configMap(name string, data string) string {
	return "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: " + name + "}\n" + data
}

// manyLines builds a data block whose canonical patch is roughly
// lines*width bytes, each value repeating fill.
func manyLines(lines, width int, fill string) string {
	var data strings.Builder
	data.WriteString("data:\n")
	for i := range lines {
		fmt.Fprintf(&data, "  k%05d: '%s'\n", i, strings.Repeat(fill, width))
	}
	return data.String()
}

func compareDefault(t *testing.T, before, after fs.FS) (Result, error) {
	t.Helper()
	return Compare(context.Background(), before, after, defaultOptions)
}

// deliveredSize is the result's size as a tool sends it: JSON text content
// (itself JSON-encoded as a string) plus the same JSON as details.
func deliveredSize(t *testing.T, result Result) int {
	t.Helper()
	encoded, err := result.JSON()
	if err != nil {
		t.Fatal(err)
	}
	quoted, err := json.Marshal(string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	return len(encoded) + len(quoted)
}

func TestPatchTruncatedAtWholeLines(t *testing.T) {
	result, err := compareDefault(t, files(), files("big.yaml", configMap("big", manyLines(2000, 20, "v"))))
	if err != nil {
		t.Fatal(err)
	}
	entry := result.Resources[0]
	if !entry.PatchTruncated || !result.Truncated || result.OmittedResources != 0 {
		t.Fatalf("flags: patch_truncated=%v truncated=%v omitted=%d", entry.PatchTruncated, result.Truncated, result.OmittedResources)
	}
	if len(entry.Patch) > maxPatchBytes || !strings.HasPrefix(entry.Patch, patchMarker) || !strings.HasSuffix(entry.Patch, patchTruncatedMarker) {
		t.Fatalf("bounded patch shape: %d bytes, start %q, end %q", len(entry.Patch), entry.Patch[:40], entry.Patch[len(entry.Patch)-40:])
	}
	body := strings.TrimSuffix(strings.TrimPrefix(entry.Patch, patchMarker), patchTruncatedMarker)
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "+  k") || len(last) != len("+  k00000: ")+20 {
		t.Fatalf("patch was not cut at a whole line: %q", last)
	}
}

func TestMaxResourcesCountsOmittedAndKeepsIdentityOrder(t *testing.T) {
	after := files()
	for _, name := range []string{"e", "c", "a", "d", "b"} {
		after[name+".yaml"] = &fstest.MapFile{Data: []byte(configMap(name, ""))}
	}
	options := defaultOptions
	options.MaxResources = 2
	result, err := Compare(context.Background(), files(), after, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Added != 5 || len(result.Resources) != 2 || result.OmittedResources != 3 || !result.Truncated {
		t.Fatalf("summary %+v listed %d omitted %d truncated %v", result.Summary, len(result.Resources), result.OmittedResources, result.Truncated)
	}
	if result.Resources[0].Identity.Name != "a" || result.Resources[1].Identity.Name != "b" {
		t.Fatalf("not the first identities: %+v", result.Resources)
	}
}

func TestOutputBudgetIsExactForDeliveredEncoding(t *testing.T) {
	after := files()
	for i := range 60 {
		// Quotes, backslashes and HTML characters make the delivered size
		// differ from the raw size, which the budget must account for.
		after[fmt.Sprintf("r%02d.yaml", i)] = &fstest.MapFile{Data: []byte(configMap(fmt.Sprintf("r%02d", i), manyLines(60, 10, `"\<`)))}
	}
	options := defaultOptions
	options.MaxResources = MaxResources
	result, err := Compare(context.Background(), files(), after, options)
	if err != nil {
		t.Fatal(err)
	}
	size := deliveredSize(t, result)
	if size > MaxOutputBytes {
		t.Fatalf("delivered %d bytes, budget %d", size, MaxOutputBytes)
	}
	if len(result.Resources) == 0 || result.OmittedResources != 60-len(result.Resources) || !result.Truncated || result.Summary.Added != 60 {
		t.Fatalf("listed %d omitted %d truncated %v summary %+v", len(result.Resources), result.OmittedResources, result.Truncated, result.Summary)
	}
	// One more resource must not have fit: the prefix is the longest one.
	extended := result
	extended.Resources = append(append([]Resource(nil), result.Resources...), describeAdded(t, after, len(result.Resources)))
	extended.OmittedResources--
	if deliveredSize(t, extended) <= MaxOutputBytes {
		t.Fatalf("a further resource would still fit in %d bytes", MaxOutputBytes)
	}
}

// describeAdded renders the index-th added resource of after as Compare would.
func describeAdded(t *testing.T, after fstest.MapFS, index int) Resource {
	t.Helper()
	loaded, err := loadSide(context.Background(), "after", after)
	if err != nil {
		t.Fatal(err)
	}
	identities := sortedIdentities(nil, loaded)
	entry, err := describe(nil, loaded[identities[index]], defaultOptions)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestErrorsDoNotRevealSecretValues(t *testing.T) {
	const secretValue = "hunter2-TOP-SECRET"
	for name, document := range map[string]string{
		"syntax":        "apiVersion: v1\nkind: Secret\nmetadata: {name: s}\ndata:\n  password: " + secretValue + "\n  broken: [\n",
		"duplicate key": "apiVersion: v1\nkind: Secret\nmetadata: {name: s}\ndata:\n  password: " + secretValue + "\n  password: " + secretValue + "\n",
		"custom tag":    "apiVersion: v1\nkind: Secret\nmetadata: {name: s}\ndata:\n  password: !vault " + secretValue + "\n",
		"bad integer":   "apiVersion: v1\nkind: Secret\nmetadata: {name: s}\ndata:\n  password: !!int " + secretValue + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := compareDefault(t, files("secret.yaml", document), files())
			if err == nil {
				t.Fatal("malformed Secret accepted")
			}
			if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("error reveals the secret: %v", err)
			}
			var manifestErr *Error
			if !errors.As(err, &manifestErr) || manifestErr.Side != "before" || manifestErr.Path != "secret.yaml" || manifestErr.Document != 1 {
				t.Fatalf("error does not locate the input: %#v", err)
			}
		})
	}
}

func TestSecretDataHiddenFromPatchesAndDetails(t *testing.T) {
	before := files("s.yaml", "apiVersion: v1\nkind: Secret\nmetadata: {name: s}\ndata: {password: b2xkLXZhbHVl}\nstringData: plain-old\n")
	after := files("s.yaml", "apiVersion: v1\nkind: Secret\nmetadata: {name: s}\ndata: {password: bmV3LXZhbHVl, added: bmV3}\nstringData: plain-new\n")
	result, err := compareDefault(t, before, after)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := result.JSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"b2xkLXZhbHVl", "bmV3LXZhbHVl", "bmV3", "plain-old", "plain-new"} {
		if strings.Contains(string(encoded), leaked) {
			t.Fatalf("redacted output contains %q: %s", leaked, encoded)
		}
	}
	entry := result.Resources[0]
	if !entry.SecretDataChanged || !strings.Contains(entry.Patch, "+  added: <redacted>") {
		t.Fatalf("redacted patch should still show the new key: %+v", entry)
	}
	// A non-Secret keeps its data visible: only core v1 Secrets are redacted.
	result, err = compareDefault(t, files("c.yaml", configMap("c", "data: {password: old}\n")), files("c.yaml", configMap("c", "data: {password: new}\n")))
	if err != nil {
		t.Fatal(err)
	}
	if entry := result.Resources[0]; entry.SecretDataChanged || !strings.Contains(entry.Patch, "+  password: new") {
		t.Fatalf("ConfigMap data must stay visible: %+v", entry)
	}
}

func TestSymlinksAreRejectedNotSkipped(t *testing.T) {
	link := func(target string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(target), Mode: fs.ModeSymlink}
	}
	tree := fstest.MapFS{
		"real/cm.yaml":    {Data: []byte(configMap("c", ""))},
		"dir/cm.yaml":     {Data: []byte(configMap("c", ""))},
		"dir/linked.yaml": link("../real/cm.yaml"),
		"linkdir":         link("real"),
		"real/sub":        link("../dir"),
	}
	for name, selected := range map[string]string{"entry in walked directory": "dir", "selected symlink": "linkdir", "symlinked subdirectory": "real", "symlink below selection": "linkdir/cm.yaml"} {
		t.Run(name, func(t *testing.T) {
			_, err := compareDefault(t, View(tree, selected), files())
			if err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("symlink not rejected: %v", err)
			}
		})
	}
}

func TestCancellationStopsComparison(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Compare(ctx, files("a/x.yaml", configMap("x", "")), files(), defaultOptions)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestInputBudgets(t *testing.T) {
	aliasBomb := configMap("bomb", "data:\n  a: &a [x, x, x, x, x, x, x, x, x, x]\n  b: &b [*a, *a, *a, *a, *a, *a, *a, *a, *a, *a]\n  c: &c [*b, *b, *b, *b, *b, *b, *b, *b, *b, *b]\n  d: &d [*c, *c, *c, *c, *c, *c, *c, *c, *c, *c]\n  e: &e [*d, *d, *d, *d, *d, *d, *d, *d, *d, *d]\n  f: [*e, *e, *e]\n")
	deep := configMap("deep", "spec: "+strings.Repeat("[", maxNesting+1)+strings.Repeat("]", maxNesting+1)+"\n")
	var manyResources strings.Builder
	for i := range maxSideResources + 1 {
		fmt.Fprintf(&manyResources, "---\n%s", configMap(fmt.Sprintf("c%d", i), ""))
	}
	manyFiles := fstest.MapFS{}
	for i := range maxSideFiles + 1 {
		manyFiles[fmt.Sprintf("f%04d.yaml", i)] = &fstest.MapFile{}
	}
	largeFiles := fstest.MapFS{}
	chunk := "# " + strings.Repeat("x", maxFileBytes-3) + "\n"
	for i := range maxSideBytes/maxFileBytes + 1 {
		largeFiles[fmt.Sprintf("big%d.yaml", i)] = &fstest.MapFile{Data: []byte(chunk)}
	}
	for name, test := range map[string]struct {
		before fs.FS
		want   string
	}{
		"alias expansion": {files("bomb.yaml", aliasBomb), "YAML nodes including alias expansion"},
		"nesting":         {files("deep.yaml", deep), "nesting exceeds"},
		"resources":       {files("many.yaml", manyResources.String()), "more than 2000 resources"},
		"files":           {manyFiles, "more than 2048 files"},
		"file size":       {files("huge.yaml", strings.Repeat("#", maxFileBytes+1)), "file exceeds"},
		"total size":      {largeFiles, "input exceeds"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := compareDefault(t, test.before, files())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestOptionRanges(t *testing.T) {
	for _, options := range []Options{{ContextLines: -1, MaxResources: 1}, {ContextLines: 11, MaxResources: 1}, {MaxResources: 0}, {MaxResources: 201}} {
		if _, err := Compare(context.Background(), files(), files(), options); err == nil {
			t.Fatalf("accepted %+v", options)
		}
	}
}
