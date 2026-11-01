package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func backgroundPoll() *cancelPoll {
	return &cancelPoll{ctx: context.Background()}
}

// rootIgnoreSet holds one root .gitignore with the rules.
func rootIgnoreSet(ctx context.Context, rules string) *ignoreSet {
	set := newIgnoreSet(ctx, ".gitignore")
	set.stack = append(set.stack, ignoreDirectory{lists: []*ignoreList{parseIgnoreList(rules, "")}})
	return set
}

// The port gives git's own answers on git's wildmatch test vectors.
func TestWildmatchMatchesGitTestVectors(t *testing.T) {
	for _, vector := range wildmatchVectors {
		if got := wildmatch(backgroundPoll(), vector.pattern, vector.text, true); got != vector.pathname {
			t.Errorf("wildmatch(%q, %q, WM_PATHNAME) = %v, git says %v", vector.pattern, vector.text, got, vector.pathname)
		}
		if got := wildmatch(backgroundPoll(), vector.pattern, vector.text, false); got != vector.pathmatch {
			t.Errorf("wildmatch(%q, %q, 0) = %v, git says %v", vector.pattern, vector.text, got, vector.pathmatch)
		}
	}
}

// git's wildmatch retries every zero-length `**/` whatever the abort code,
// which is exponential in consecutive `**/` runs: git 2.54 ls-files did not
// finish 40 of them over a 20-level path within a minute. Memoising keeps the
// answer and makes it polynomial.
func TestWildmatchConsecutiveDoubleStarsStayPolynomial(t *testing.T) {
	const runs, depth = 5000, 30
	text := strings.Repeat("d/", depth) + "file"
	start := time.Now()
	if wildmatch(backgroundPoll(), strings.Repeat("**/", runs)+"nomatch", text, true) {
		t.Fatal("pattern without the basename matched")
	}
	if !wildmatch(backgroundPoll(), strings.Repeat("**/", runs)+"file", text, true) {
		t.Fatal("pattern ending in the basename did not match")
	}
	if elapsed := time.Since(start); elapsed > workspaceListingTimeout {
		t.Fatalf("matching took %v", elapsed)
	}
}

// A match notices its context ending and reports the path excluded, so a
// walk lists nothing more once it is out of time.
func TestIgnoreSetStopsMatchingWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	set := rootIgnoreSet(ctx, strings.Repeat("*q*z\n", cancelPollInterval*2))
	if !set.excluded("file.go", false) || !set.poll.done {
		t.Fatal("matching went on after the context ended")
	}
}

func TestParseIgnoreListFollowsGitLineRules(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		content string
		want    []string
	}{
		{name: "bom crlf and missing final newline", content: "\ufeffa\r\nb", want: []string{"a", "b"}},
		{name: "comments and blank lines", content: "#c\n\n \\#h\n", want: []string{" \\#h"}},
		{name: "unescaped trailing spaces", content: "x  \n", want: []string{"x"}},
		{name: "escaped trailing space", content: "x\\ \n", want: []string{"x\\ "}},
		{name: "escaped backslash then space", content: "x\\\\ \n", want: []string{"x\\\\"}},
		{name: "NUL ends the pattern", content: "ab\x00cd\n", want: []string{"ab"}},
		{name: "empty patterns dropped", content: "!\n/\n\r\n", want: nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var got []string
			for _, pattern := range parseIgnoreList(testCase.content, "").patterns {
				got = append(got, pattern.pattern)
			}
			if strings.Join(got, "|") != strings.Join(testCase.want, "|") {
				t.Fatalf("patterns %q, want %q", got, testCase.want)
			}
		})
	}
}
