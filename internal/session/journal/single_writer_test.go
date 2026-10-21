package journal

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// Two processes can resume the same session at once (two `-continue` runs, or
// two clients of a future server). Both stores append to one file, seq values
// repeat, and the reduction interleaves both runs into a transcript with
// results under the wrong assistant turn and "result lost" claims for calls
// whose results exist. Reproduced on the real binary with
// context/stress/two_writers.sh. A journal must have one writer: a second
// open for appending has to be refused while the first is live.
func TestSecondWriterIsRefusedWhileFirstIsOpen(t *testing.T) {
	first := newTestStore(t, t.TempDir())
	t.Cleanup(func() { closeStore(t, first) })
	if err := first.AppendMessage(testContext, userMessage("hi")); err != nil {
		t.Fatal(err)
	}

	second, _, err := OpenForResume(testContext, first.Path(), Options{})
	if err == nil {
		closeStore(t, second)
		t.Fatal("OpenForResume succeeded while another store is appending to the same journal")
	}
	if !errors.Is(err, ErrSessionLocked) || !strings.Contains(err.Error(), first.Path()) {
		t.Fatalf("second writer error = %v, want ErrSessionLocked naming the journal", err)
	}
	if _, err := Load(testContext, first.Path()); err != nil {
		t.Fatalf("read-only load while writer owns the journal: %v", err)
	}
	closeStore(t, first)
	resumed, _, err := OpenForResume(testContext, first.Path(), Options{})
	if err != nil {
		t.Fatalf("open after first writer closed: %v", err)
	}
	defer closeStore(t, resumed)
	if next, _, err := OpenForResume(testContext, first.Path(), Options{}); !errors.Is(err, ErrSessionLocked) {
		if next != nil {
			closeStore(t, next)
		}
		t.Fatalf("second resumed writer error = %v, want ErrSessionLocked", err)
	}
}

func TestLockedJournalIsNotRepairedBySecondWriter(t *testing.T) {
	first := newTestStore(t, t.TempDir())
	defer closeStore(t, first)
	contents, err := os.ReadFile(first.Path())
	if err != nil {
		t.Fatal(err)
	}
	contents = append(contents, []byte(`{"type":"message"`)...)
	if _, err := first.file.Write([]byte(`{"type":"message"`)); err != nil {
		t.Fatal(err)
	}
	if second, _, err := OpenForResume(testContext, first.Path(), Options{}); !errors.Is(err, ErrSessionLocked) {
		if second != nil {
			closeStore(t, second)
		}
		t.Fatalf("second writer error = %v, want ErrSessionLocked", err)
	}
	after, err := os.ReadFile(first.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(contents) {
		t.Fatal("refused opener changed the locked writer's torn tail")
	}
}
