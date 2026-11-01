package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/mcpconfig"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Split(b.buf.String(), "\n")
}

func TestMCPServerLogMessagesReachDiagnosticLog(t *testing.T) {
	var logs lockedBuffer
	manager, _ := openFixture(t, "normal", func(_ *mcpconfig.Server, options *Options) {
		options.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	if _, _, err := manager.ReadResource(context.Background(), "mcp://fixture/fixture://log"); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"warning": {"level=WARN", `msg="MCP server log"`, "server=fixture", "mcp_level=warning", "logger=fixture-disk", `disk low`},
		"debug":   {"level=DEBUG", "mcp_level=debug", `data="debug detail"`},
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(want) > 0 {
		for name, parts := range want {
			for _, line := range logs.lines() {
				matched := true
				for _, part := range parts {
					matched = matched && strings.Contains(line, part)
				}
				if matched {
					delete(want, name)
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing %v in log:\n%s", want, strings.Join(logs.lines(), "\n"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMCPProgressWithoutMessageBecomesToolProgress(t *testing.T) {
	manager, _ := openFixture(t, "normal")
	updates := make(chan string, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settled := make(chan struct{})
	go func() {
		defer close(settled)
		_, _ = fixtureTool(t, manager, "block").Execute(ctx, "progress", json.RawMessage(`{}`), func(partial agent.AgentToolResult) {
			updates <- partial.Content[0].Text
		})
	}()
	deadline := time.After(5 * time.Second)
	for found := false; !found; {
		select {
		case text := <-updates:
			found = text == "progress 2/3"
		case <-deadline:
			t.Fatal("bare progress notification produced no tool progress")
		}
	}
	cancel()
	<-settled
}
