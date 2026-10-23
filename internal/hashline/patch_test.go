package hashline

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

// memFS is an in-memory workspace keyed by path.
type memFS map[string]string

func (m memFS) Key(path string) (string, error) { return path, nil }

func (m memFS) Read(path string) ([]byte, error) {
	text, ok := m[path]
	if !ok {
		return nil, fmt.Errorf("open %s: %w", path, fs.ErrNotExist)
	}
	return []byte(text), nil
}

// apply parses and prepares input, then commits it to files the way the
// tool does, returning per-file previews.
func apply(t *testing.T, store *Store, files memFS, input string) (map[string]string, error) {
	t.Helper()
	patch, err := Parse(input)
	if err != nil {
		return nil, err
	}
	plan, err := Prepare(patch, files, store)
	if err != nil {
		return nil, err
	}
	previews := map[string]string{}
	for _, change := range plan.Changes {
		switch change.Op {
		case OpRemove:
			delete(files, change.Path)
		case OpMove:
			delete(files, change.Path)
			files[change.Dest] = string(change.Encoded)
		default:
			files[change.Path] = string(change.Encoded)
		}
		tag, preview := store.Committed(change)
		previews[change.Path] = "#" + tag + "\n" + preview
	}
	store.CommitRegisters(plan)
	return previews, nil
}

// readAll records a whole-file read of path and returns its tag.
func readAll(store *Store, files memFS, path string) string {
	text := Decode([]byte(files[path])).Text
	var lines []int
	for i := range Lines(text) {
		lines = append(lines, i+1)
	}
	return store.Observe(path, text, lines)
}

const numbered = "one\ntwo\nthree\nfour\nfive\n"

func TestApplyOps(t *testing.T) {
	tests := []struct {
		name  string
		file  string
		hunks string
		want  string
	}{
		{"replace range", numbered, "PUT 2.=3:\n+TWO\n+THREE\n+extra", "one\nTWO\nTHREE\nextra\nfour\nfive\n"},
		{"replace one line", numbered, "PUT 4.=4:\n+FOUR", "one\ntwo\nthree\nFOUR\nfive\n"},
		{"single-number shorthand", numbered, "PUT 4:\n+FOUR", "one\ntwo\nthree\nFOUR\nfive\n"},
		{"insert before", numbered, "PUT <3:\n+2.5", "one\ntwo\n2.5\nthree\nfour\nfive\n"},
		{"insert after", numbered, "PUT >3:\n+3.5", "one\ntwo\nthree\n3.5\nfour\nfive\n"},
		{"head", numbered, "PUT <1:\n+zero", "zero\none\ntwo\nthree\nfour\nfive\n"},
		{"tail", numbered, "PUT >$:\n+six", "one\ntwo\nthree\nfour\nfive\nsix\n"},
		{"blank body row", numbered, "PUT >1:\n+", "one\n\ntwo\nthree\nfour\nfive\n"},
		{"literal plus and minus rows", numbered, "PUT >1:\n+- item\n++x", "one\n- item\n+x\ntwo\nthree\nfour\nfive\n"},
		{"cut range", numbered, "CUT 2.=4", "one\nfive\n"},
		{"anonymous move", numbered, "CUT 1.=2\nPUT >4", "three\nfour\none\ntwo\nfive\n"},
		{"named move", numbered, "CUT 4.=5 @tail\nPUT <1 @tail", "four\nfive\none\ntwo\nthree\n"},
		{"span paste", numbered, "CUT 1.=1 @a\nPUT 5.=5 @a", "two\nthree\nfour\none\n"},
		{"numbers are original across hunks", numbered, "PUT <1:\n+a\n+b\nPUT 5.=5:\n+FIVE\nCUT 2.=2", "a\nb\none\nthree\nfour\nFIVE\n"},
		{"same gap keeps patch order, replacement last", numbered, "PUT 3.=3:\n+THREE\nPUT >2:\n+after-two", "one\ntwo\nafter-two\nTHREE\nfour\nfive\n"},
		{"interior blank rows kept", numbered, "PUT >1:\n+a\n\n+b", "one\na\n\nb\ntwo\nthree\nfour\nfive\n"},
		{"empty file head", "", "PUT <1:\n+first", "first\n"},
		{"no trailing newline kept", "a\nb", "PUT 2.=2:\n+B", "a\nB"},
		{"delete everything", "a\nb\n", "CUT 1.=2", ""},
		{"insert at the edge of a cut", numbered, "CUT 2.=3\nPUT >3:\n+x", "one\nx\nfour\nfive\n"},
		{"envelope", numbered, "PUT 1.=1:\n+ONE\n*** End Patch\nPUT 2.=2:\n+ignored", "ONE\ntwo\nthree\nfour\nfive\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := memFS{"f.txt": tt.file}
			store := NewStore()
			tag := readAll(store, files, "f.txt")
			input := "*** Begin Patch\n[f.txt#" + tag + "]\n" + tt.hunks + "\n"
			if _, err := apply(t, store, files, input); err != nil {
				t.Fatal(err)
			}
			if got := files["f.txt"]; got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyFileOps(t *testing.T) {
	files := memFS{"a.txt": "x\ny\n", "b.txt": "keep\n"}
	store := NewStore()
	tagA, tagB := readAll(store, files, "a.txt"), readAll(store, files, "b.txt")
	if _, err := apply(t, store, files, fmt.Sprintf("[a.txt#%s]\nPUT 1.=1:\n+X\nMV \"dir/new a.txt\"\n[b.txt#%s]\nREM\n", tagA, tagB)); err != nil {
		t.Fatal(err)
	}
	if _, ok := files["b.txt"]; ok {
		t.Fatal("REM left b.txt")
	}
	if _, ok := files["a.txt"]; ok {
		t.Fatal("MV left a.txt")
	}
	if got := files["dir/new a.txt"]; got != "X\ny\n" {
		t.Fatalf("moved content %q", got)
	}
}

func TestRegistersFlowAcrossSectionsAndCalls(t *testing.T) {
	files := memFS{"a.go": numbered, "b.go": "b1\nb2\n"}
	store := NewStore()
	tagA, tagB := readAll(store, files, "a.go"), readAll(store, files, "b.go")
	// A CUT in one section feeds a PUT in the next.
	if _, err := apply(t, store, files, fmt.Sprintf("[a.go#%s]\nCUT 2.=3 @mid\n[b.go#%s]\nPUT >1 @mid\n", tagA, tagB)); err != nil {
		t.Fatal(err)
	}
	if files["a.go"] != "one\nfour\nfive\n" || files["b.go"] != "b1\ntwo\nthree\nb2\n" {
		t.Fatalf("after move: a=%q b=%q", files["a.go"], files["b.go"])
	}
	// The named register persists into the next call.
	tagB = readAll(store, files, "b.go")
	if _, err := apply(t, store, files, fmt.Sprintf("[b.go#%s]\nPUT >$ @mid\n", tagB)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(files["b.go"], "b2\ntwo\nthree\n") {
		t.Fatalf("persisted paste: %q", files["b.go"])
	}
	// The anonymous register does not.
	tagA = readAll(store, files, "a.go")
	_, err := apply(t, store, files, fmt.Sprintf("[a.go#%s]\nCUT 1.=1\n", tagA))
	if err != nil {
		t.Fatal(err)
	}
	tagA = readAll(store, files, "a.go")
	_, err = apply(t, store, files, fmt.Sprintf("[a.go#%s]\nPUT >1\n", tagA))
	if err == nil || !strings.Contains(err.Error(), "nothing to paste") {
		t.Fatalf("anonymous register crossed calls: %v", err)
	}
	// A failed call does not publish its cuts.
	before := files["a.go"]
	_, err = apply(t, store, files, fmt.Sprintf("[a.go#%s]\nCUT 1.=1 @lost\nPUT 9.=9:\n+x\n", tagA))
	if err == nil || files["a.go"] != before {
		t.Fatalf("failed call: err=%v file=%q", err, files["a.go"])
	}
	_, err = apply(t, store, files, fmt.Sprintf("[a.go#%s]\nPUT >1 @lost\n", tagA))
	if err == nil || !strings.Contains(err.Error(), "@lost, which is empty") || !strings.Contains(err.Error(), "@mid") {
		t.Fatalf("register from failed call leaked or message lacks available names: %v", err)
	}
}

func TestPatchRejections(t *testing.T) {
	tests := []struct {
		name  string
		hunks string
		want  string
	}{
		{"overlap", "PUT 2.=3:\n+x\nCUT 3.=4", "already targets"},
		{"duplicate range", "PUT 2.=2:\n+x\nPUT 2.=2:\n+y", "already targets"},
		{"insert inside removed range", "CUT 2.=4\nPUT >3:\n+x", "both removed by the hunk"},
		{"beyond end", "PUT 9.=9:\n+x", "has 5 lines"},
		{"range end beyond end", "CUT 4.=7", "has 5 lines"},
		{"ambiguous anonymous paste", "CUT 1.=1\nCUT 2.=2\nPUT >5", "ambiguous"},
		{"empty anonymous paste", "PUT >5", "nothing to paste"},
		{"empty named span paste", "PUT 1.=1 @none", "@none, which is empty"},
		{"no change", "PUT 1.=1:\n+one", "unchanged"},
		{"block on unsupported language", "PUT 1*:\n+x", "needs a block resolver"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := memFS{"f.txt": numbered}
			store := NewStore()
			tag := readAll(store, files, "f.txt")
			_, err := apply(t, store, files, "[f.txt#"+tag+"]\n"+tt.hunks+"\n")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
			if files["f.txt"] != numbered {
				t.Fatalf("rejected patch changed the file: %q", files["f.txt"])
			}
		})
	}
}

func TestPreflightIsAtomic(t *testing.T) {
	files := memFS{"a.txt": numbered, "b.txt": numbered}
	store := NewStore()
	tagA, tagB := readAll(store, files, "a.txt"), readAll(store, files, "b.txt")
	patch, err := Parse(fmt.Sprintf("[a.txt#%s]\nPUT 1.=1:\n+changed\n[b.txt#%s]\nPUT 40.=40:\n+bad\n", tagA, tagB))
	if err != nil {
		t.Fatal(err)
	}
	if plan, err := Prepare(patch, files, store); err == nil || plan != nil {
		t.Fatalf("Prepare accepted a patch with an invalid section: %v", err)
	}
	// Same file named twice, by two sections.
	_, err = apply(t, store, files, fmt.Sprintf("[a.txt#%s]\nPUT 1.=1:\n+x\n[a.txt#%s]\nPUT 2.=2:\n+y\n", tagA, tagA))
	if err == nil || !strings.Contains(err.Error(), "name the same file") {
		t.Fatalf("duplicate sections: %v", err)
	}
	// MV never overwrites.
	_, err = apply(t, store, files, fmt.Sprintf("[a.txt#%s]\nMV b.txt\n", tagA))
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("MV onto an existing file: %v", err)
	}
	if files["a.txt"] != numbered || files["b.txt"] != numbered {
		t.Fatal("a rejected patch changed files")
	}
	files["bin.dat"] = "a\x00b\n"
	_, err = apply(t, store, files, "[bin.dat#"+TagText("a\x00b\n")+"]\nPUT 1.=1:\n+x\n")
	if err == nil || !strings.Contains(err.Error(), "binary file") || files["bin.dat"] != "a\x00b\n" {
		t.Fatalf("binary file: %v", err)
	}
	_, err = apply(t, store, files, "[missing.txt#ABCD]\nPUT 1.=1:\n+x\n")
	if err == nil || !strings.Contains(err.Error(), "create new files with write") {
		t.Fatalf("missing file: %v", err)
	}
}

func TestTags(t *testing.T) {
	t.Run("stale after change", func(t *testing.T) {
		files := memFS{"f.txt": numbered}
		store := NewStore()
		tag := readAll(store, files, "f.txt")
		files["f.txt"] = "changed\n" + numbered
		_, err := apply(t, store, files, "[f.txt#"+tag+"]\nPUT 2.=2:\n+x\n")
		var stale *StaleTagError
		if !errors.As(err, &stale) || !stale.Known || stale.Current != TagText(files["f.txt"]) {
			t.Fatalf("err = %#v", err)
		}
		if !strings.Contains(err.Error(), "*2:one") {
			t.Fatalf("stale error lacks current context: %v", err)
		}
		// The rows the error displayed are editable under the current tag;
		// other lines are not.
		if _, err := apply(t, store, files, "[f.txt#"+stale.Current+"]\nPUT 2.=2:\n+ONE\n"); err != nil {
			t.Fatalf("edit of displayed context row: %v", err)
		}
	})
	t.Run("unknown tag", func(t *testing.T) {
		files := memFS{"f.txt": numbered}
		_, err := apply(t, NewStore(), files, "[f.txt#0000]\nPUT 1.=1:\n+x\n")
		var stale *StaleTagError
		if TagText(numbered) == "0000" || !errors.As(err, &stale) || stale.Known {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unrecorded tag matching live content is accepted", func(t *testing.T) {
		files := memFS{"f.txt": numbered}
		store := NewStore()
		if _, err := apply(t, store, files, "[f.txt#"+TagText(numbered)+"]\nPUT 5.=5:\n+FIVE\n"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("lowercase tag", func(t *testing.T) {
		files := memFS{"f.txt": numbered}
		store := NewStore()
		tag := strings.ToLower(readAll(store, files, "f.txt"))
		if _, err := apply(t, store, files, "[f.txt#"+tag+"]\nPUT 5.=5:\n+FIVE\n"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("16-bit collision is rejected", func(t *testing.T) {
		recorded := numbered
		tag := TagText(recorded)
		collision := ""
		for i := 0; collision == ""; i++ {
			if candidate := fmt.Sprintf("collide %d\n", i); TagText(candidate) == tag {
				collision = candidate
			}
		}
		files := memFS{"f.txt": recorded}
		store := NewStore()
		readAll(store, files, "f.txt")
		files["f.txt"] = collision
		_, err := apply(t, store, files, "[f.txt#"+tag+"]\nPUT 1.=1:\n+x\n")
		var stale *StaleTagError
		if !errors.As(err, &stale) || !stale.Known {
			t.Fatalf("collision accepted or misreported: %v", err)
		}
	})
	t.Run("edit result chains", func(t *testing.T) {
		files := memFS{"f.txt": numbered}
		store := NewStore()
		tag := readAll(store, files, "f.txt")
		previews, err := apply(t, store, files, "[f.txt#"+tag+"]\nPUT >1:\n+inserted\n")
		if err != nil {
			t.Fatal(err)
		}
		next := strings.TrimPrefix(strings.SplitN(previews["f.txt"], "\n", 2)[0], "#")
		if next != TagText(files["f.txt"]) || !strings.Contains(previews["f.txt"], "2:inserted") {
			t.Fatalf("preview %q", previews["f.txt"])
		}
		if _, err := apply(t, store, files, "[f.txt#"+next+"]\nPUT 6.=6:\n+FIVE\n"); err != nil {
			t.Fatal(err)
		}
		if files["f.txt"] != "one\ninserted\ntwo\nthree\nfour\nFIVE\n" {
			t.Fatalf("chained edit: %q", files["f.txt"])
		}
	})
}

func TestSeenLines(t *testing.T) {
	files := memFS{"f.txt": numbered}
	store := NewStore()
	tag := store.Observe("f.txt", numbered, []int{1, 2, 3})
	_, err := apply(t, store, files, "[f.txt#"+tag+"]\nPUT 2.=5:\n+x\n")
	var unseen *UnseenLinesError
	if !errors.As(err, &unseen) || fmt.Sprint(unseen.Lines) != "[4 5]" || len(unseen.Revealed) != 2 || unseen.Revealed[0] != "4:four" {
		t.Fatalf("err = %#v", err)
	}
	// The rejection displayed lines 4-5, so the same patch now applies.
	if _, err := apply(t, store, files, "[f.txt#"+tag+"]\nPUT 2.=5:\n+x\n"); err != nil {
		t.Fatal(err)
	}
	if files["f.txt"] != "one\nx\n" {
		t.Fatalf("got %q", files["f.txt"])
	}
	// Seen state carries across an edit: unseen lines stay unseen under
	// the new tag, written lines are known.
	big := strings.Repeat("line\n", 100)
	files["g.txt"] = big
	tag = store.Observe("g.txt", big, []int{1, 2})
	previews, err := apply(t, store, files, "[g.txt#"+tag+"]\nPUT 1.=1:\n+first\n")
	if err != nil {
		t.Fatal(err)
	}
	next := strings.TrimPrefix(strings.SplitN(previews["g.txt"], "\n", 2)[0], "#")
	if _, err := apply(t, store, files, "[g.txt#"+next+"]\nPUT 1.=1:\n+again\n"); err != nil {
		t.Fatalf("written line not editable: %v", err)
	}
	_, err = apply(t, store, files, "[g.txt#"+TagText(files["g.txt"])+"]\nCUT 50.=99\n")
	if !errors.As(err, &unseen) || len(unseen.Revealed) != 0 {
		t.Fatalf("over-cap unseen range: %v", err)
	}
	// Too many lines to reveal: a retry is still rejected.
	_, err = apply(t, store, files, "[g.txt#"+TagText(files["g.txt"])+"]\nCUT 50.=99\n")
	if !errors.As(err, &unseen) {
		t.Fatalf("retry without a read applied: %v", err)
	}
	// An outline (no raw rows) leaves every line unseen.
	files["h.go"] = "package h\n\nfunc F() {\n}\n"
	tag = store.Observe("h.go", files["h.go"], nil)
	_, err = apply(t, store, files, "[h.go#"+tag+"]\nPUT 3*:\n+func F() { return }\n")
	if !errors.As(err, &unseen) {
		t.Fatalf("edit after outline only: %v", err)
	}
}

func TestLineEndingsAndBOM(t *testing.T) {
	files := memFS{"w.txt": "\ufeffone\r\ntwo\r\nthree\r\n"}
	store := NewStore()
	tag := readAll(store, files, "w.txt")
	if tag != TagText("one\ntwo\nthree\n") {
		t.Fatal("tag depends on BOM or CRLF")
	}
	if _, err := apply(t, store, files, "[w.txt#"+tag+"]\nPUT 2.=2:\n+TWO\n+2b\n"); err != nil {
		t.Fatal(err)
	}
	if got := files["w.txt"]; got != "\ufeffone\r\nTWO\r\n2b\r\nthree\r\n" {
		t.Fatalf("got %q", got)
	}
}

func TestStoreBounds(t *testing.T) {
	store := NewStore()
	for i := range maxStorePaths + 10 {
		store.Observe(fmt.Sprintf("p%d", i), "x\n", []int{1})
	}
	if len(store.paths) != maxStorePaths {
		t.Fatalf("paths = %d", len(store.paths))
	}
	if _, ok := store.paths["p0"]; ok {
		t.Fatal("oldest path survived eviction")
	}
	for i := range maxVersionsPerPath + 3 {
		store.Observe("v", fmt.Sprintf("%d\n", i), nil)
	}
	if n := len(store.paths["v"].versions); n != maxVersionsPerPath {
		t.Fatalf("versions = %d", n)
	}
}

func TestPreview(t *testing.T) {
	oldLines := Lines(strings.Repeat("x\n", 3) + "a\nb\n" + strings.Repeat("y\n", 10))
	newLines := Lines(strings.Repeat("x\n", 3) + "A\n" + strings.Repeat("y\n", 10))
	rows, displayed := formatPreview(newLines, lineOrigins(oldLines, newLines), len(oldLines))
	if rows != "3:x\n4:A\n5:y" || fmt.Sprint(displayed) != "[3 4 5]" {
		t.Fatalf("rows %q displayed %v", rows, displayed)
	}
	long := Lines(strings.Repeat("n\n", 20))
	rows, displayed = formatPreview(long, lineOrigins(nil, long), 0)
	if strings.Contains(rows, "...") || len(displayed) != 20 {
		t.Fatalf("a changed run under the cap must show in full, got %q", rows)
	}
	huge := Lines(strings.Repeat("n\n", previewRowsCap+20))
	rows, _ = formatPreview(huge, lineOrigins(nil, huge), 0)
	if want := fmt.Sprintf("1:n\n2:n\n...\n%d:n\n%d:n", previewRowsCap+19, previewRowsCap+20); rows != want {
		t.Fatalf("a run over the cap must elide to its edges, got %q", rows)
	}
	rows, _ = formatPreview(Lines("a\nb\n"), lineOrigins(Lines("a\nb\nc\n"), Lines("a\nb\n")), 3)
	if rows != "2:b" {
		t.Fatalf("trailing deletion %q", rows)
	}
}
