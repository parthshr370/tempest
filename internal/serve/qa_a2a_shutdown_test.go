package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/permission"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/tempest"
)

func TestA2AShutdownDeliversTerminalStatusBeforeClosingStream(t *testing.T) {
	provider := faux.New(faux.Options{})
	provider.SetResponses(faux.Respond(faux.ToolCall("work", json.RawMessage(`{}`), "blocked")))
	entered := make(chan struct{})
	tool := tempest.Tool{Tool: types.Tool{Name: "work", Parameters: json.RawMessage(`{"type":"object"}`)}, Execute: func(ctx context.Context, _ string, _ json.RawMessage, _ agent.AgentToolUpdateCallback) (agent.AgentToolResult, error) {
		close(entered)
		<-ctx.Done()
		return agent.AgentToolResult{}, ctx.Err()
	}}
	server := interopServer(t, provider, []tempest.Tool{tool}, permission.ModeBypass)
	listenResult := make(chan error, 1)
	go func() { listenResult <- server.ListenAndServe("127.0.0.1:0") }()
	deadline := time.Now().Add(5 * time.Second)
	address := ""
	for address == "" {
		server.mu.Lock()
		if server.listener != nil {
			address = server.listener.Addr().String()
		}
		server.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("listener did not bind")
		}
		time.Sleep(time.Millisecond)
	}
	request := a2aRequest("SendStreamingMessage", sendParams("run", "", false))
	request.URL.Host = address
	request.RequestURI = ""
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(first, `"task":`) || !strings.Contains(first, "TASK_STATE_WORKING") {
		t.Fatalf("initial snapshot: %s, error: %v", first, err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not enter blocking tool")
	}
	if err := server.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	remaining, readErr := io.ReadAll(reader)
	if !strings.Contains(string(remaining), "TASK_STATE_CANCELED") {
		t.Fatalf("shutdown closed healthy SSE before delivering terminal task status: remaining=%q, read error=%v", remaining, readErr)
	}
	if err := <-listenResult; err != nil {
		t.Fatal(err)
	}
}
