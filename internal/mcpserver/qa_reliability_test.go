package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest"
)

type qaHalfCloseStream struct {
	reader *io.PipeReader
	writer *io.PipeWriter
}

func (stream qaHalfCloseStream) Read(buffer []byte) (int, error)  { return stream.reader.Read(buffer) }
func (stream qaHalfCloseStream) Write(buffer []byte) (int, error) { return stream.writer.Write(buffer) }
func (stream qaHalfCloseStream) Close() error {
	returnError := stream.reader.Close()
	if closeError := stream.writer.Close(); returnError == nil {
		returnError = closeError
	}
	return returnError
}

func TestMCPServerEOFDrainsAcceptedCallReply(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.AppendResponses(faux.Respond(faux.ToolCall("mcp__qa__wait", json.RawMessage(`{}`), "wait")))
	serverOptions := options(t, provider)
	serverOptions.ShutdownTimeout = time.Second
	started := make(chan struct{})
	serverOptions.DefaultNew.Tools = []tempest.Tool{{
		Tool: tempest.ToolDefinition{Name: "mcp__qa__wait", Parameters: json.RawMessage(`{"type":"object"}`)},
		Execute: func(ctx context.Context, _ string, _ json.RawMessage, _ tempest.ToolUpdateCallback) (tempest.ToolResult, error) {
			close(started)
			<-ctx.Done()
			return tempest.ToolResult{}, ctx.Err()
		},
	}}
	serverOptions.Stack.PermissionPolicy.Overrides = map[string]permission.Decision{"mcp__qa__wait": permission.DecideAllow}
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer inputWriter.Close()
	defer outputReader.Close()
	serverDone := make(chan error, 1)
	go func() { serverDone <- Serve(ctx, qaHalfCloseStream{inputReader, outputWriter}, serverOptions) }()
	defer func() {
		cancel()
		select {
		case serveError := <-serverDone:
			if serveError != nil {
				t.Error(serveError)
			}
		case <-time.After(3 * time.Second):
			t.Error("server did not join after EOF")
		}
	}()
	if _, err := fmt.Fprintln(inputWriter, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(outputReader)
	if !scanner.Scan() {
		t.Fatal("initialize response missing")
	}
	if _, err := fmt.Fprintln(inputWriter, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"+`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"tempest_run","arguments":{"prompt":"wait"}}}`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("tool did not start")
	}
	if err := inputWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if !scanner.Scan() {
		t.Fatalf("stdin EOF discarded accepted noncancelled tools/call reply while stdout remained writable: %v", scanner.Err())
	}
	var reply struct {
		ID     int        `json:"id"`
		Result toolResult `json:"result"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.ID != 2 || reply.Result.StructuredContent == nil || reply.Result.StructuredContent.StopReason != "aborted" {
		t.Fatalf("expected aborted result for accepted call, got %s", scanner.Bytes())
	}
}

func TestElicitationMessageBoundsAndFields(t *testing.T) {
	ask := tempest.Ask{
		SessionID: "session", ToolCallID: "call", Tool: "bash",
		Title: "Create workspace", RawInput: json.RawMessage(`{"command":"mkdir workspace"}`),
		Reason: "Command modifies the filesystem",
	}
	message := approvalMessage(ask)
	for _, field := range []string{ask.SessionID, ask.ToolCallID, ask.Tool, ask.Title, string(ask.RawInput), ask.Reason} {
		if !strings.Contains(message, field) {
			t.Errorf("approval message omitted %q: %s", field, message)
		}
	}
	long := strings.Repeat("界", 2048)
	ask.SessionID, ask.ToolCallID, ask.Tool = long, long, long
	ask.Title, ask.RawInput, ask.Reason = long, json.RawMessage(long), long
	message = approvalMessage(ask)
	const messageLimit = 2 << 10
	if len(message) > messageLimit || !utf8.ValidString(message) {
		t.Fatalf("approval message must fit %d bytes and remain UTF-8: bytes=%d valid=%t", messageLimit, len(message), utf8.ValidString(message))
	}
	for _, label := range []string{"Title:", "Arguments:", "Guard reason:"} {
		if !strings.Contains(message, label) {
			t.Errorf("long approval lost %s", label)
		}
	}
}

func TestMCPElicitationIncludesRequestedOperation(t *testing.T) {
	provider := faux.New(faux.Options{})
	const command = "mkdir approval-visible-operation"
	arguments, err := json.Marshal(struct {
		Command string `json:"command"`
	}{Command: command})
	if err != nil {
		t.Fatal(err)
	}
	provider.AppendResponses(faux.Respond(faux.ToolCall("bash", arguments, "approval-call")), faux.Respond(faux.Text("done")))
	asked := make(chan string, 1)
	connection := connect(t, options(t, provider), "2025-11-25", json.RawMessage(`{"elicitation":{}}`), func(_ context.Context, method string, raw json.RawMessage) (any, *rpc.Error) {
		if method == "elicitation/create" {
			var request struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(raw, &request); err != nil {
				t.Error(err)
			}
			asked <- request.Message
			return json.RawMessage(`{"action":"decline"}`), nil
		}
		return nil, nil
	})
	result, err := connection.run(RunInput{Prompt: "request approval"}, nil)
	if err != nil || result.IsError {
		t.Fatalf("run failed before approval: %v", err)
	}
	select {
	case message := <-asked:
		if !strings.Contains(message, command) {
			t.Error("approval message omitted the requested bash command; the client cannot distinguish safe and destructive operations")
		}
	default:
		t.Error("expected elicitation request")
	}
}

func TestMCPServerShutdownReleasesGoroutines(t *testing.T) {
	const connectionCycles = 8
	const settleLimit = 2 * time.Second
	baseline := runtime.NumGoroutine()
	for range connectionCycles {
		provider := faux.New(faux.Options{})
		provider.AppendResponses(faux.Respond(faux.Text("done")))
		connection := connect(t, options(t, provider), "2025-11-25", json.RawMessage(`{}`), nil)
		result, err := connection.run(RunInput{Prompt: "close all workers"}, nil)
		if err != nil || result.IsError {
			t.Fatalf("run before close failed: %v", err)
		}
		connection.stop(t)
	}
	deadline := time.Now().Add(settleLimit)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	remaining := runtime.NumGoroutine()
	if remaining > baseline {
		stacks := make([]byte, 1<<20)
		size := runtime.Stack(stacks, true)
		t.Fatalf("goroutines after %d completed connection lifetimes: baseline=%d remaining=%d\n%s", connectionCycles, baseline, remaining, stacks[:size])
	}
	t.Logf("joined %d session/connection lifetimes: baseline goroutines=%d, after close=%d", connectionCycles, baseline, remaining)
}

var _ io.ReadWriteCloser = qaHalfCloseStream{}
