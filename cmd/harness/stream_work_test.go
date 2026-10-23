package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest"
)

func TestStreamJSONIncludesSupervisedWorkBeforeUnchangedReport(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.ToolCall("bash", json.RawMessage(`{"command":"printf background","async":true}`), "async_bash")), faux.Respond(faux.Text("waiting")), faux.Respond(faux.Text("finished")))
	owner := streamSession(t, provider, provider.StreamSimple, tempest.Budget{})
	var stdout bytes.Buffer
	outcome, err := executeOneShot(t.Context(), owner, tempest.Prompt{Text: "background command"}, "stream-json", &stdout, io.Discard, 512)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.StopReason != "completed" {
		t.Fatalf("outcome=%+v", outcome)
	}
	scanner := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
	var lines [][]byte
	for scanner.Scan() {
		lines = append(lines, bytes.Clone(scanner.Bytes()))
	}
	if err = scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) < 2 {
		t.Fatal("stream has no update/report seam")
	}
	sawJob, sawQueue := false, false
	for _, line := range lines[:len(lines)-1] {
		var envelope struct {
			Seq    uint64          `json:"seq"`
			Update json.RawMessage `json:"update"`
		}
		if err = json.Unmarshal(line, &envelope); err != nil || envelope.Seq == 0 {
			t.Fatalf("invalid envelope %s %v", line, err)
		}
		sawJob = sawJob || bytes.Contains(envelope.Update, []byte(`"job":`))
		sawQueue = sawQueue || bytes.Contains(envelope.Update, []byte(`"item":`))
	}
	final := lines[len(lines)-1]
	if !sawJob || !sawQueue || !bytes.Contains(final, []byte(`"stop_reason":"completed"`)) || bytes.Contains(final, []byte(`"seq":`)) {
		t.Fatalf("work/report stream=%s", stdout.Bytes())
	}
}
