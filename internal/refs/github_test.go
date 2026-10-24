package refs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitHubURLParsing(t *testing.T) {
	for _, raw := range []string{"issue://", "pr://owner/repo?state=merged&limit=2", "issue://12", "pr://owner/repo/12", "pr://12/diff", "pr://owner/repo/12/diff/2", "pr://12/diff/all", "issue://owner/repo/12?comments=0"} {
		if !IsURL(raw) {
			t.Errorf("not recognized: %s", raw)
		}
		ref, err := Parse(raw)
		if err != nil || ref.String() != raw {
			t.Errorf("parse %s: %+v %v", raw, ref, err)
		}
	}
	for _, raw := range []string{"issue://0", "pr://-1", "pr://owner//12", "pr://owner/repo/12/../secret", "issue://12/diff", "pr://12/diff/0", "pr://12/anything", "pr://12?limit=2", "pr://?limit=0", "issue://?state=merged", "pr://12?comments=maybe", "pr://12#fragment", "pr://owner/repo?unknown=x"} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}
func TestGitHubShortURLSessionCwd(t *testing.T) {
	bin, cwd := t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("GH_EXPECT_CWD", cwd)
	script := `#!/bin/sh
set -eu
[ "$PWD" -ef "$GH_EXPECT_CWD" ]
case "$*" in
 'issue view 12 --json number,title,state,body,url,author,labels,createdAt,updatedAt,comments') printf '{"number":12,"title":"session cwd"}';;
 'pr list --json number,title,state,url,author,labels,updatedAt --limit 2 --state merged --repo owner/repo') printf '[{"number":12}]';;
 *) printf 'bad argv %s' "$*" >&2;exit 82;;esac
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	resource, err := ReadGitHub(t.Context(), cwd, "issue://12")
	if err != nil || !resource.Rendered || !strings.Contains(string(resource.Data), "session cwd") {
		t.Fatalf("read: %+v %v", resource, err)
	}
	if _, err := ReadGitHub(t.Context(), cwd, "pr://owner/repo?state=merged&limit=2"); err != nil {
		t.Fatal(err)
	}
}
