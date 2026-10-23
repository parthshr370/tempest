package hashline

import (
	"strings"
	"testing"
)

func TestParseShapes(t *testing.T) {
	patch, err := Parse("[a b/c.go#1a2b]\nPUT 3.=5:\n+x\nPUT 7*:\n+y\nPUT <2:\n+z\nPUT <1:\n+h\nPUT >4*:\n+w\nPUT >$:\n+t\nCUT 9.=10 @r1\nCUT 12*\nPUT >20 @r1\nPUT >21\nPUT 30.=31 @r1\nPUT 32* @r1\nPUT 6-6:\n+dash\nMV 'd/e f.go'\n")
	if err != nil {
		t.Fatal(err)
	}
	sec := patch.Sections[0]
	if sec.Path != "a b/c.go" || sec.Tag != "1A2B" || sec.Dest != "d/e f.go" {
		t.Fatalf("section %+v", sec)
	}
	want := []struct {
		loc        locKind
		start, end int
		cut        bool
		register   string
		colon      bool
		body       int
	}{
		{locRange, 3, 5, false, "", true, 1},
		{locBlock, 7, 0, false, "", true, 1},
		{locBefore, 2, 0, false, "", true, 1},
		{locHead, 1, 0, false, "", true, 1},
		{locAfterBlock, 4, 0, false, "", true, 1},
		{locTail, 0, 0, false, "", true, 1},
		{locRange, 9, 10, true, "r1", false, 0},
		{locBlock, 12, 0, true, "", false, 0},
		{locAfter, 20, 0, false, "r1", false, 0},
		{locAfter, 21, 0, false, "", false, 0},
		{locRange, 30, 31, false, "r1", false, 0},
		{locBlock, 32, 0, false, "r1", false, 0},
		{locRange, 6, 6, false, "", true, 1},
	}
	if len(sec.Hunks) != len(want) {
		t.Fatalf("%d hunks", len(sec.Hunks))
	}
	for i, w := range want {
		h := sec.Hunks[i]
		if h.Loc != w.loc || h.Start != w.start || h.End != w.end || h.Cut != w.cut || h.Register != w.register || h.Colon != w.colon || len(h.Body) != w.body {
			t.Errorf("hunk %d (%s) = %+v, want %+v", i, h.Header, h, w)
		}
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"no header", "PUT 1.=1:\n+x", "section header first"},
		{"missing tag", "[a.go]\nPUT 1.=1:\n+x", "no snapshot tag"},
		{"full anchor as tag", "[a.go#" + strings.Repeat("ab", 32) + "]\nREM", "not a section tag"},
		{"colon on register put", "[a.go#ABCD]\nPUT >3 @r:", "never takes `:`"},
		{"colonless span put", "[a.go#ABCD]\nPUT 3.=4", "needs a named one"},
		{"empty replace", "[a.go#ABCD]\nPUT 3.=4:\n[b.go#ABCD]\nREM", "To delete lines use `CUT 3.=4`"},
		{"empty insert", "[a.go#ABCD]\nPUT >3:", "promises body rows"},
		{"body under cut", "[a.go#ABCD]\nCUT 1.=2\n+x", "takes no body rows"},
		{"body under paste", "[a.go#ABCD]\nPUT >1 @r\n+x", "takes no `+` rows"},
		{"minus row", "[a.go#ABCD]\nPUT 1.=1:\n-old\n+new", "`-` rows do not exist"},
		{"indented plus", "[a.go#ABCD]\nPUT 1.=1:\n  +x", "must start at column 1"},
		{"orphan body", "[a.go#ABCD]\nREM\n+x", "has no `PUT …:` header above it"},
		{"pasted read row", "[a.go#ABCD]\n12:foo", "looks like a pasted read row"},
		{"reversed range", "[a.go#ABCD]\nCUT 9.=3", "absolute last line"},
		{"line zero", "[a.go#ABCD]\nCUT 0.=3", "start at 1"},
		{"rem with hunks", "[a.go#ABCD]\nREM\nCUT 1.=1", "cannot be combined"},
		{"two file ops", "[a.go#ABCD]\nMV b.go\nREM", "only one file op"},
		{"empty section", "[a.go#ABCD]\n[b.go#ABCD]\nREM", "has no ops"},
		{"bad register", "[a.go#ABCD]\nCUT 1.=1 @", "register names"},
		{"trailing junk", "[a.go#ABCD]\nPUT 1.=1: x", "unexpected"},
		{"unterminated quote", "[a.go#ABCD]\nMV \"x", "unterminated quote"},
		{"no sections", "\n\n", "no sections"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.input)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestParseToleratesCutColon(t *testing.T) {
	patch, err := Parse("[a.go#ABCD]\nCUT 1.=2:\n")
	if err != nil || len(patch.Warnings) != 1 || patch.Sections[0].Hunks[0].Colon {
		t.Fatalf("patch=%+v err=%v", patch, err)
	}
}

func TestSectionPaths(t *testing.T) {
	got := SectionPaths("[a.go#ABCD]\nPUT 1.=1:\n+[x]\n[dir/b c.md#1234]\nREM\n")
	if strings.Join(got, "|") != "a.go|dir/b c.md" {
		t.Fatalf("got %q", got)
	}
}
