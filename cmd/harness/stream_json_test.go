package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/stream"
	types "go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest"
)

func streamSession(t *testing.T, f *faux.Faux, fn agent.StreamFn, budget tempest.Budget) *tempest.Session {
	t.Helper()
	s, err := tempest.Open(context.Background(), tempest.Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &tempest.NewSession{Budget: budget}, Stack: &tempest.StackConfig{Model: f.Model(), StreamFn: fn}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func TestStreamJSONOrderAndImmediateDelta(t *testing.T) {
	f := faux.New(faux.Options{MinTokenSize: 1, MaxTokenSize: 1})
	f.AppendResponses(faux.Respond(faux.Text("first"), faux.ToolCall("ls", json.RawMessage(`{}`), "list")), faux.Respond(faux.Text("final")))
	release := make(chan struct{})
	var once sync.Once
	fn := func(ctx context.Context, m types.Model, c types.Context, o *types.SimpleStreamOptions) *stream.AssistantStream {
		src := f.StreamSimple(ctx, m, c, o)
		dst := stream.NewAssistantStream(m.API, m.Provider, m.ID)
		go func() {
			defer dst.End()
			for ev := range src.Events() {
				dst.Push(ev)
				if ev.Type == types.EvTextDelta {
					once.Do(func() {
						select {
						case <-release:
						case <-ctx.Done():
						}
					})
				}
			}
		}()
		return dst
	}
	s := streamSession(t, f, fn, tempest.Budget{})
	_, captured, stop := s.Watch(context.Background(), tempest.WatchOptions{Buffer: 256})
	defer stop()
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := executeOneShot(context.Background(), s, "go", "stream-json", writer, io.Discard, 256)
		_ = writer.Close()
		done <- err
	}()
	scanner := bufio.NewScanner(reader)
	var lines [][]byte
	for scanner.Scan() {
		line := bytes.Clone(scanner.Bytes())
		lines = append(lines, line)
		if bytes.Contains(line, []byte(`"agent_message_chunk"`)) {
			close(release)
			break
		}
	}
	if len(lines) == 0 || !bytes.Contains(lines[len(lines)-1], []byte(`"agent_message_chunk"`)) {
		t.Fatal("no live text delta before provider release")
	}
	for scanner.Scan() {
		lines = append(lines, bytes.Clone(scanner.Bytes()))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if scanner.Err() != nil {
		t.Fatal(scanner.Err())
	}
	var seq uint64
	for i, line := range lines[:len(lines)-1] {
		var update tempest.Update
		if err := json.Unmarshal(line, &update); err != nil {
			t.Fatal(err)
		}
		if update.Seq != seq+1 {
			t.Fatalf("line %d seq=%d after %d", i, update.Seq, seq)
		}
		seq = update.Seq
		expected := <-captured
		want, _ := json.Marshal(expected)
		if !bytes.Equal(line, want) {
			t.Fatalf("event differs from Watch: %s != %s", line, want)
		}
	}
	if !bytes.Contains(lines[len(lines)-2], []byte(`"status":"finished"`)) {
		t.Fatal("terminal update wasn't immediately before report")
	}
	var report runReportOutput
	if err := json.Unmarshal(lines[len(lines)-1], &report); err != nil {
		t.Fatal(err)
	}
	if report.Text != "final" || report.StopReason != "completed" || report.SessionID != s.ID() {
		t.Fatalf("report=%+v", report)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken writer") }

func TestStreamJSONWriterFailureCancelsAndJoins(t *testing.T) {
	f := faux.New(faux.Options{TokensPerSecond: 1})
	f.AppendResponses(faux.Respond(faux.Text(strings.Repeat("long response ", 100))))
	s := streamSession(t, f, f.StreamSimple, tempest.Budget{})
	outcome, err := executeOneShot(context.Background(), s, "go", "stream-json", failingWriter{}, io.Discard, 256)
	if err == nil || !strings.Contains(err.Error(), "broken writer") {
		t.Fatalf("error=%v", err)
	}
	if outcome.StopReason != agent.StopAborted {
		t.Fatalf("run wasn't joined and aborted: %+v", outcome)
	}
}

type pausedWriter struct {
	bytes.Buffer
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *pausedWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.release })
	return w.Buffer.Write(data)
}

func TestStreamJSONGapKeepsHonestTerminalReport(t *testing.T) {
	f := faux.New(faux.Options{MinTokenSize: 1, MaxTokenSize: 1})
	f.AppendResponses(faux.Respond(faux.Text(strings.Repeat("x", 10000))))
	s := streamSession(t, f, f.StreamSimple, tempest.Budget{})
	_, observe, stop := s.Watch(context.Background(), tempest.WatchOptions{Buffer: 20000})
	defer stop()
	writer := &pausedWriter{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := executeOneShot(context.Background(), s, "go", "stream-json", writer, io.Discard, 1)
		done <- err
	}()
	<-writer.entered
	// The independent observer sees completion while stdout stays blocked.
	// The stream's one-slot queue must overflow before we release its writer.
	for update := range observe {
		if update.Kind == "tempest/run" && bytes.Contains(update.Update, []byte(`"status":"finished"`)) {
			break
		}
	}
	close(writer.release)
	select {
	case err := <-done:
		if !errors.Is(err, errStreamGap) {
			t.Fatalf("gap error=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gap hung")
	}
	lines := bytes.Split(bytes.TrimSpace(writer.Bytes()), []byte("\n"))
	gap := false
	for _, line := range lines[:len(lines)-1] {
		var update tempest.Update
		if json.Unmarshal(line, &update) != nil {
			t.Fatalf("invalid event %s", line)
		}
		if bytes.Contains(update.Update, []byte(`"lastSeq"`)) {
			gap = true
		}
	}
	if !gap {
		t.Fatalf("actual gap missing: %s", writer.Bytes())
	}
	var report runReportOutput
	if json.Unmarshal(lines[len(lines)-1], &report) != nil || report.StopReason != "completed" {
		t.Fatalf("terminal report missing: %s", writer.Bytes())
	}
}

func TestStreamJSONStopsShareJSONReport(t *testing.T) {
	for _, test := range []struct {
		name    string
		budget  tempest.Budget
		steps   []faux.ResponseStep
		cancel  bool
		unknown bool
	}{
		{name: "completed", steps: []faux.ResponseStep{faux.Respond(faux.Text("answer"))}},
		{name: "empty", steps: []faux.ResponseStep{faux.Respond()}},
		{name: "error"},
		{name: "max_turns", budget: tempest.Budget{MaxTurns: 1}, steps: []faux.ResponseStep{faux.Respond(faux.ToolCall("ls", json.RawMessage(`{}`), "l"))}},
		{name: "max_cost", budget: tempest.Budget{MaxCostUSD: 0.000000001}, steps: []faux.ResponseStep{faux.Respond(faux.ToolCall("ls", json.RawMessage(`{}`), "l"))}},
		{name: "max_wall_time", budget: tempest.Budget{MaxWallTime: time.Nanosecond}, steps: []faux.ResponseStep{faux.Respond(faux.ToolCall("ls", json.RawMessage(`{}`), "l"))}},
		{name: "aborted", cancel: true, steps: []faux.ResponseStep{faux.Respond(faux.Text("answer"))}},
		{name: "unknown", unknown: true, steps: []faux.ResponseStep{faux.Respond(faux.Text("answer"))}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := faux.New(faux.Options{Models: []faux.ModelDef{{ID: "faux", Cost: types.ModelCost{Input: 1, Output: 1}}}})
			f.AppendResponses(test.steps...)
			model := f.Model()
			if test.unknown {
				model.Cost.Unknown = true
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fn := f.StreamSimple
			if test.cancel {
				fn = func(callCtx context.Context, m types.Model, c types.Context, o *types.SimpleStreamOptions) *stream.AssistantStream {
					cancel()
					return f.StreamSimple(callCtx, m, c, o)
				}
			}
			s, err := tempest.Open(context.Background(), tempest.Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), New: &tempest.NewSession{Budget: test.budget}, Stack: &tempest.StackConfig{Model: model, StreamFn: fn}})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background())
			var output bytes.Buffer
			outcome, err := executeOneShot(ctx, s, "go", "stream-json", &output, io.Discard, 256)
			if err != nil {
				t.Fatal(err)
			}
			wantStop := test.name
			if wantStop == "empty" || wantStop == "unknown" {
				wantStop = "completed"
			}
			if string(outcome.StopReason) != wantStop {
				t.Fatalf("stop=%s want %s", outcome.StopReason, wantStop)
			}
			lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n"))
			var actual map[string]any
			if json.Unmarshal(lines[len(lines)-1], &actual) != nil {
				t.Fatal("missing report")
			}
			// The ordinary JSON renderer uses the same terminal capture and builder.
			var pretty bytes.Buffer
			last := s.Messages()
			writeRunOutput(&pretty, io.Discard, "json", outcome, last)
			var expected map[string]any
			_ = json.Unmarshal(pretty.Bytes(), &expected)
			a, _ := json.Marshal(actual)
			b, _ := json.Marshal(expected)
			if !bytes.Equal(a, b) {
				t.Fatalf("stream=%s json=%s", a, b)
			}
			if test.unknown && actual["cost_usd"] != nil {
				t.Fatalf("unknown cost=%v", actual["cost_usd"])
			}
		})
	}
}

func TestStreamJSONResumeDoesNotReplayHistory(t *testing.T) {
	f := faux.New(faux.Options{})
	f.AppendResponses(faux.Respond(faux.Text("old assistant text")), faux.Respond(faux.Text("new assistant text")))
	dir, cwd := t.TempDir(), t.TempDir()
	options := tempest.Options{SessionDir: dir, Cwd: cwd, New: &tempest.NewSession{}, Stack: &tempest.StackConfig{Model: f.Model(), StreamFn: f.StreamSimple}}
	sess, err := openSession(context.Background(), options, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Prompt(context.Background(), tempest.Prompt{Text: "old user text"}); err != nil {
		t.Fatal(err)
	}
	id := sess.ID()
	if err := sess.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess, err = openSession(context.Background(), options, id, false)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close(context.Background())
	before := sess.Snapshot().Seq
	var stdout bytes.Buffer
	if _, err := executeOneShot(context.Background(), sess, "new user text", "stream-json", &stdout, io.Discard, 256); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte("\n"))
	var first tempest.Update
	if json.Unmarshal(lines[0], &first) != nil || first.Seq != before+1 {
		t.Fatalf("first event=%s snapshot seq=%d", lines[0], before)
	}
	if bytes.Contains(stdout.Bytes(), []byte("old assistant text")) || bytes.Contains(stdout.Bytes(), []byte("old user text")) {
		t.Fatal("resumed stream replayed history")
	}
}

func TestInvalidOutputFormatPrecedesRuntimeSetup(t *testing.T) {
	t.Setenv("HARNESS_MCP_CONFIG", "/must-not-load-mcp-config.json")
	var stdout, stderr bytes.Buffer
	if code := runOneShot([]string{"-p", "go", "-output-format", "invalid"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit=%d", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "unsupported output format") || strings.Contains(stderr.String(), "mcp_config") {
		t.Fatalf("stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
}
