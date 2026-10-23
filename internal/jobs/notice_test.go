package jobs

import (
	"encoding/json"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/work"
)

func TestCompletionNoticeIsReadableTextWithRefsAndStaysBounded(t *testing.T) {
	exit := 2
	bash := work.Job{ID: "job_b", Kind: "bash", State: "failed", Reason: "exit_nonzero", ExitCode: &exit,
		ResultText: strings.Repeat("early line\n", 200) + "FINAL ERROR LINE",
		Output:     &work.Artifact{Path: "/sessions/s.jsonl.d/jobs/job_b/output.log", SHA256: "ab", Bytes: 2216}}
	text, err := completionNotice(bash)
	if err != nil {
		t.Fatal(err)
	}
	if json.Valid([]byte(text)) {
		t.Fatalf("notice is raw JSON: %s", text)
	}
	for _, want := range []string{"job_b", "bash", "failed", "exit code 2", "FINAL ERROR LINE", bash.Output.Path, `logs {"job_id":"job_b"}`} {
		if !strings.Contains(text, want) {
			t.Fatalf("notice lacks %q:\n%s", want, text)
		}
	}
	if again, _ := completionNotice(bash); again != text {
		t.Fatal("notice is not deterministic; recovery would conflict on the completion key")
	}

	task := work.Job{ID: "job_t", Kind: "task", State: "completed", ResultText: strings.Repeat("\x01", 4096),
		Value:  json.RawMessage(`"` + strings.Repeat("<", 60<<10) + `"`),
		Result: &work.Artifact{Path: "/p/" + strings.Repeat("r", 1500) + "/result.json", Bytes: 9},
		Patch:  &work.Artifact{Path: "/p/" + strings.Repeat("q", 1500) + "/changes.patch", Bytes: 9}}
	text, err = completionNotice(task)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(text)
	if len(encoded)+noticeItemOverhead > maxCompletionBytes {
		t.Fatalf("encoded notice %d bytes exceeds cap", len(encoded))
	}
	if !strings.Contains(text, task.Result.Path) || !strings.Contains(text, task.Patch.Path) || !strings.Contains(text, `wait {"job_ids":["job_t"]}`) {
		t.Fatalf("notice dropped a reference:\n%s", text)
	}
}
