package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest"
)

func TestMCPResultSpillPreservesFullTextAndBoundsJSON(t *testing.T) {
	for _, text := range []string{strings.Repeat("界", 50000), strings.Repeat("\x00", 120000)} {
		f := faux.New(faux.Options{MinTokenSize: 200000, MaxTokenSize: 200000})
		f.AppendResponses(faux.Respond(faux.Text(text)))
		opts := options(t, f)
		c := connect(t, opts, "2025-11-25", map[string]any{}, nil)
		result, err := c.run(RunInput{Prompt: "large output"}, nil)
		if err != nil || result.IsError || result.StructuredContent == nil {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		out := result.StructuredContent
		if !out.Truncated || out.OutputPath == "" || !utf8.ValidString(out.Text) || !strings.Contains(out.Text, "truncated") {
			t.Fatalf("spill metadata=%+v", out)
		}
		full, err := os.ReadFile(out.OutputPath)
		if err != nil || string(full) != text {
			t.Fatalf("full spill mismatch, err=%v", err)
		}
		info, err := os.Stat(out.OutputPath)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("spill file mode=%v err=%v", info, err)
		}
		encoded, _ := json.Marshal(result)
		if len(encoded) > (1<<20)-4096 {
			t.Fatalf("encoded result=%d", len(encoded))
		}
	}
}

func TestMCPResultSpillFailureStillHasDurableRun(t *testing.T) {
	f := faux.New(faux.Options{MinTokenSize: 200000, MaxTokenSize: 200000})
	f.AppendResponses(faux.Respond(faux.Text(strings.Repeat("x", 140000))))
	opts := options(t, f)
	sess, err := tempest.Open(context.Background(), tempest.Options{SessionDir: opts.SessionDir, Cwd: opts.Cwd, New: &opts.DefaultNew, Stack: opts.Stack})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close(context.Background())
	run, err := sess.Begin(context.Background(), tempest.Prompt{Text: "go"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait()
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "not-a-directory")
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	response := makeResult(file.Name(), sess, run, result)
	if !response.IsError || response.StructuredContent == nil || response.StructuredContent.RunID != result.RunID || response.StructuredContent.OutputPath != "" {
		t.Fatalf("spill failure lost run: %+v", response)
	}
}

func TestMCPHandshakeRequiredAndToolSchema(t *testing.T) {
	s := &server{}
	for _, method := range []string{"tools/list", "tools/call"} {
		_, err := s.handle(context.Background(), method, json.RawMessage(`{}`))
		if err == nil || err.Code != -32002 {
			t.Fatalf("preinitialize %s error=%v", method, err)
		}
	}
	var tool struct {
		Name        string `json:"name"`
		InputSchema struct {
			AdditionalProperties bool     `json:"additionalProperties"`
			Required             []string `json:"required"`
		} `json:"inputSchema"`
		OutputSchema json.RawMessage `json:"outputSchema"`
	}
	if json.Unmarshal(toolDefinition, &tool) != nil || tool.Name != "tempest_run" || tool.InputSchema.AdditionalProperties || len(tool.InputSchema.Required) != 1 || tool.OutputSchema != nil {
		t.Fatalf("tool schema=%s", toolDefinition)
	}
	for _, raw := range []string{`{"prompt":"go","budget":{"bad":1}}`, `{"prompt":"go","budget":{"maxTurns":1.5}}`, `{"prompt":"go","unknown":true}`, `{"prompt":"go","budget":null}`, `{"prompt":"go","budget":{"maxTurns":null}}`, `{"prompt":null}`} {
		var in RunInput
		if decodeRunInput(json.RawMessage(raw), &in) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestMCPSessionsIndependentPermissions(t *testing.T) {
	f := faux.New(faux.Options{})
	opts := options(t, f)
	for range 2 {
		f.AppendResponses(faux.Respond(faux.ToolCall("bash", json.RawMessage(`{"command":"mkdir -p approval-test"}`), "same-call")), faux.Respond(faux.Text("done")))
	}
	var askCount int
	c := connect(t, opts, "2025-11-25", map[string]any{"elicitation": map[string]any{}}, func(_ context.Context, method string, _ json.RawMessage) (any, *rpc.Error) {
		if method == "elicitation/create" {
			askCount++
			return map[string]any{"action": "accept", "content": map[string]bool{"allow": true}}, nil
		}
		return nil, nil
	})
	first, err := c.run(RunInput{Prompt: "first"}, nil)
	if err != nil || first.IsError {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := c.run(RunInput{Prompt: "second", Cwd: t.TempDir()}, nil)
	if err != nil || second.IsError {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	if first.StructuredContent.SessionID == second.StructuredContent.SessionID || askCount != 2 {
		t.Fatalf("sessions shared permissions: asks=%d", askCount)
	}
}

func TestMCPSignalContextCancelsPendingElicitation(t *testing.T) {
	f := faux.New(faux.Options{})
	opts := options(t, f)
	f.AppendResponses(faux.Respond(faux.ToolCall("bash", json.RawMessage(`{"command":"mkdir -p must-not-exist"}`), "pending")))
	asked := make(chan struct{})
	c := connect(t, opts, "2025-11-25", map[string]any{"elicitation": map[string]any{}}, func(ctx context.Context, method string, _ json.RawMessage) (any, *rpc.Error) {
		if method == "elicitation/create" {
			close(asked)
			<-ctx.Done()
		}
		return nil, nil
	})
	result := make(chan error, 1)
	go func() { _, err := c.run(RunInput{Prompt: "wait"}, nil); result <- err }()
	<-asked
	c.cancel()
	select {
	case <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown failed to retire pending elicitation")
	}
	// connect's cleanup joins Serve and all session resources.
}

func TestMCPFormCapabilityValidation(t *testing.T) {
	for _, test := range []struct {
		revision, raw string
		want          bool
	}{
		{"2025-11-25", `{}`, true}, {"2025-11-25", `{"form":{}}`, true}, {"2025-11-25", `{"url":{}}`, false}, {"2025-11-25", `null`, false}, {"2025-11-25", `{"form":null}`, false}, {"2025-06-18", `{}`, true},
	} {
		if got := supportsForm(test.revision, []byte(test.raw)); got != test.want {
			t.Fatalf("form(%s,%s)=%v", test.revision, test.raw, got)
		}
	}
	if !bytes.Contains(toolDefinition, []byte(`"additionalProperties":false`)) {
		t.Fatal("input schema missing constraints")
	}
}
