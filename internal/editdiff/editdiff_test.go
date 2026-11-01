package editdiff

import (
	"strings"
	"testing"
)

func TestContentAnchor(t *testing.T) {
	if got := ContentAnchor([]byte("abc")); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("ContentAnchor = %q", got)
	}
	if got := len(ContentAnchor([]byte("abc"))); got != AnchorLength {
		t.Fatalf("ContentAnchor length = %d, want %d", got, AnchorLength)
	}
	if ContentAnchor([]byte("abc")) == ContentAnchor([]byte("abd")) {
		t.Fatal("ContentAnchor should change with content")
	}
}

func TestLineEndingHelpers(t *testing.T) {
	if got := DetectLineEnding("a\r\nb\n"); got != CRLF {
		t.Fatalf("DetectLineEnding CRLF = %q", got)
	}
	if got := DetectLineEnding("a\nb\r\n"); got != LF {
		t.Fatalf("DetectLineEnding LF = %q", got)
	}
	if got := NormalizeToLF("a\r\nb\rc"); got != "a\nb\nc" {
		t.Fatalf("NormalizeToLF = %q", got)
	}
	if got := RestoreLineEndings("a\nb", CRLF); got != "a\r\nb" {
		t.Fatalf("RestoreLineEndings = %q", got)
	}
}

func TestNormalizeForFuzzyMatch(t *testing.T) {
	input := "hello\u00a0world  \n\u201csmart\u201d \u2014 dash\t"
	got := NormalizeForFuzzyMatch(input)
	want := "hello world\n\"smart\" - dash"
	if got != want {
		t.Fatalf("NormalizeForFuzzyMatch = %q, want %q", got, want)
	}
}

func TestFuzzyFindText(t *testing.T) {
	exact := FuzzyFindText("abc abc", "bc")
	if !exact.Found || exact.Index != 1 || exact.MatchLength != 2 || exact.UsedFuzzyMatch || exact.ContentForReplacement != "abc abc" {
		t.Fatalf("exact = %+v", exact)
	}
	fuzzy := FuzzyFindText("const x = \u201cold\u201d;", `const x = "old";`)
	if !fuzzy.Found || !fuzzy.UsedFuzzyMatch || fuzzy.ContentForReplacement != `const x = "old";` {
		t.Fatalf("fuzzy = %+v", fuzzy)
	}
	missing := FuzzyFindText("abc", "z")
	if missing.Found || missing.Index != -1 || missing.ContentForReplacement != "abc" {
		t.Fatalf("missing = %+v", missing)
	}
}

func TestStripBOM(t *testing.T) {
	bom, text := StripBOM(BOM + "abc")
	if bom != BOM || text != "abc" {
		t.Fatalf("StripBOM = %q %q", bom, text)
	}
	bom, text = StripBOM("abc")
	if bom != "" || text != "abc" {
		t.Fatalf("StripBOM no BOM = %q %q", bom, text)
	}
}

func TestApplyEditsToNormalizedContent(t *testing.T) {
	got, err := ApplyEditsToNormalizedContent("one\ntwo\nthree", []Edit{{OldText: "one", NewText: "1"}, {OldText: "three", NewText: "3"}}, "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseContent != "one\ntwo\nthree" || got.NewContent != "1\ntwo\n3" {
		t.Fatalf("ApplyEditsToNormalizedContent = %+v", got)
	}
}

func TestApplyEditsToNormalizedContentFuzzyPreservesUnchangedLines(t *testing.T) {
	content := "keep trailing spaces  \nconst x = \u201cold\u201d;\n"
	got, err := ApplyEditsToNormalizedContent(content, []Edit{{OldText: `const x = "old";`, NewText: `const x = "new";`}}, "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := "keep trailing spaces  \n" + `const x = "new";` + "\n"
	if got.NewContent != want {
		t.Fatalf("fuzzy new content = %q, want %q", got.NewContent, want)
	}
}

func TestApplyEditsToNormalizedContentErrors(t *testing.T) {
	cases := []struct {
		name    string
		content string
		edits   []Edit
		want    string
	}{
		{"empty", "abc", []Edit{{OldText: "", NewText: "x"}}, "old_text must not be empty in file.txt."},
		{"missing", "abc", []Edit{{OldText: "z", NewText: "x"}}, "Could not find the exact text in file.txt. The old text must match exactly including all whitespace and newlines."},
		{"duplicate", "abc abc", []Edit{{OldText: "abc", NewText: "x"}}, "Found 2 occurrences of the text in file.txt; old_text must match exactly one place. To change one of them, extend old_text with text from its line or a neighboring line until it is unique; to change several, send one edit per occurrence, each unique. Occurrences (LINE:TEXT, with one line of context):\n1:abc abc\n--\n1:abc abc"},
		{"overlap", "abcdef", []Edit{{OldText: "abc", NewText: "x"}, {OldText: "bcd", NewText: "y"}}, "edits[0] and edits[1] overlap in file.txt. Merge them into one edit or target disjoint regions."},
		{"nochange", "abc", []Edit{{OldText: "abc", NewText: "abc"}}, "No changes made to file.txt. The replacement produced identical content. This might indicate an issue with special characters or the text not existing as expected."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ApplyEditsToNormalizedContent(tc.content, tc.edits, "file.txt")
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestApplyEditsToNormalizedContentRejectsMixedFuzzyDuplicate(t *testing.T) {
	content := "const keep = “old”;\nconst x = “old”;\nconst x = “old”;\n"
	_, err := ApplyEditsToNormalizedContent(content, []Edit{
		{OldText: `const keep = "old";`, NewText: `const keep = "new";`},
		{OldText: "const x = “old”;", NewText: `const x = "new";`},
	}, "file.txt")
	if err == nil || !strings.Contains(err.Error(), "Found 2 occurrences") {
		t.Fatalf("mixed fuzzy duplicate err = %v", err)
	}
}

// TestAmbiguousEditListsOccurrences checks that an old_text matching
// several places is rejected with every match's line number and its
// neighbors, which is enough to write unique edits in one retry.
func TestAmbiguousEditListsOccurrences(t *testing.T) {
	content := "package orders\n\n// ComputeTotal returns the total.\nfunc ComputeTotal(lines []Line) int {\n\treturn sum(lines)\n}\n\nfunc twice() {\n\tx()\n\ty()\n}\n\nfunc again() {\n\tx()\n\ty()\n}\n"
	_, err := ApplyEditsToNormalizedContent(content, []Edit{{OldText: "ComputeTotal", NewText: "SumValues"}}, "orders/sum.go")
	want := "\n2:\n3:// ComputeTotal returns the total.\n4:func ComputeTotal(lines []Line) int {\n--\n3:// ComputeTotal returns the total.\n4:func ComputeTotal(lines []Line) int {\n5:\treturn sum(lines)"
	if err == nil || !strings.HasPrefix(err.Error(), "Found 2 occurrences of the text in orders/sum.go") || !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("ambiguous err = %v", err)
	}
	// A multi-line old_text: later matches are numbered past earlier ones.
	_, err = ApplyEditsToNormalizedContent(content, []Edit{{OldText: "\tx()\n\ty()\n", NewText: "\tz()\n"}}, "orders/sum.go")
	want = "\n8:func twice() {\n9:\tx()\n10:\ty()\n11:}\n--\n13:func again() {\n14:\tx()\n15:\ty()\n16:}"
	if err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("multi-line ambiguous err = %v", err)
	}
	// One retry built from the listed lines succeeds.
	got, err := ApplyEditsToNormalizedContent(content, []Edit{
		{OldText: "// ComputeTotal returns", NewText: "// SumValues returns"},
		{OldText: "func ComputeTotal(", NewText: "func SumValues("},
	}, "orders/sum.go")
	if err != nil || strings.Contains(got.NewContent, "ComputeTotal") {
		t.Fatalf("retry = %q, %v", got.NewContent, err)
	}
}

func TestApplyReplacementsPreservingUnchangedLines(t *testing.T) {
	got, err := ApplyReplacementsPreservingUnchangedLines("a  \nb\nc\n", "a\nb\nc\n", []textReplacement{{matchIndex: 2, matchLength: 1, newText: "B"}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "a  \nB\nc\n" {
		t.Fatalf("preserve unchanged = %q", got)
	}
}

func TestGenerateDiffs(t *testing.T) {
	patch := GenerateUnifiedPatch("file.txt", "a\nb\n", "a\nB\n", 3)
	if !strings.Contains(patch, "--- file.txt") || !strings.Contains(patch, "+++ file.txt") || !strings.Contains(patch, "-b") || !strings.Contains(patch, "+B") {
		t.Fatalf("patch = %q", patch)
	}

	diff := GenerateDiffString("a\nb\nc", "a\nB\nc", 1)
	if diff.FirstChangedLine == nil || *diff.FirstChangedLine != 2 {
		t.Fatalf("first changed line = %#v", diff.FirstChangedLine)
	}
	for _, want := range []string{" 1 a", "-2 b", "+2 B", " 3 c"} {
		if !strings.Contains(diff.Diff, want) {
			t.Fatalf("diff %q missing %q", diff.Diff, want)
		}
	}
}
