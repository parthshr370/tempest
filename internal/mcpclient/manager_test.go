package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/internal/agent"
	"go.harness.dev/harness/internal/mcpconfig"
)

var fixtureBinary string

func TestMain(suite *testing.M) {
	directory, err := os.MkdirTemp("", "tempest-mcp-fixture-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fixtureBinary = filepath.Join(directory, "server")
	// The parent test command already holds the machine-wide heavy lock.
	command := exec.Command("go", "build", "-race", "-o", fixtureBinary, "./testdata/server")
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	if err = command.Run(); err != nil {
		_ = os.RemoveAll(directory)
		os.Exit(1)
	}
	code := suite.Run()
	_ = os.RemoveAll(directory)
	os.Exit(code)
}
func openFixture(t *testing.T, mode string) (*Manager, string) {
	t.Helper()
	directory := t.TempDir()
	ledger := filepath.Join(directory, "ledger")
	configs := []mcpconfig.Server{{Name: "fixture", Transport: "stdio", Command: fixtureBinary, Args: []string{"-mode", mode, "-ledger", ledger}, StartupTimeout: "10s", CallTimeout: "5s"}}
	manager, err := Open(context.Background(), directory, filepath.Join(directory, "output"), configs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return manager, ledger
}
func fixtureTool(t *testing.T, manager *Manager, name string) agent.AgentTool {
	t.Helper()
	for _, tool := range manager.Tools() {
		if tool.Name == "mcp__fixture__"+name {
			return tool
		}
	}
	t.Fatalf("tool %s absent", name)
	return agent.AgentTool{}
}
func ledgerDispatches(t *testing.T, path, name string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var entry struct {
			Method string `json:"method"`
			Name   string `json:"name"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil {
			t.Fatalf("ledger entry: %s", line)
		}
		if entry.Method == "dispatch" && entry.Name == name {
			count++
		}
	}
	return count
}
func TestMCPFixtureBinary(t *testing.T) {
	manager, ledger := openFixture(t, "stderr")
	tool := fixtureTool(t, manager, "echo")
	var updates atomic.Int64
	result, err := tool.Execute(context.Background(), "call", json.RawMessage(`{"value":"hello"}`), func(agent.AgentToolResult) { updates.Add(1) })
	if err != nil || result.IsError || len(result.Content) == 0 || result.Content[0].Text != `{"value":"hello"}` {
		t.Fatalf("echo: %#v %v", result, err)
	}
	if ledgerDispatches(t, ledger, "echo") != 1 {
		t.Fatal("call count != 1")
	}
	raw, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"method":"roots"`) {
		t.Fatal("roots callback not observed")
	}
	_ = updates.Load()
}
func TestMCPVersionNegotiation(t *testing.T) {
	for _, mode := range []string{"normal", "june"} {
		t.Run(mode, func(t *testing.T) {
			manager, _ := openFixture(t, mode)
			if len(manager.Tools()) != 8 {
				t.Fatalf("tools %d", len(manager.Tools()))
			}
		})
	}
	_, err := Open(context.Background(), t.TempDir(), t.TempDir(), []mcpconfig.Server{{Name: "modern", Transport: "stdio", Command: fixtureBinary, Args: []string{"-mode", "modern"}}}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "mcp_version") || !strings.Contains(err.Error(), "2026-07-28") || !strings.Contains(err.Error(), "2025-06-18") {
		t.Fatalf("version diagnostic: %v", err)
	}
}
func TestMCPDiscoveryAndSchema(t *testing.T) {
	manager, _ := openFixture(t, "normal")
	tool := fixtureTool(t, manager, "echo")
	for _, keyword := range []string{`"$defs"`, `"$ref"`, `"oneOf"`, `"default"`} {
		if !strings.Contains(string(tool.Parameters), keyword) {
			t.Fatalf("schema keyword lost: %s", tool.Parameters)
		}
	}
	original := string(tool.Parameters)
	tool.Parameters[0] = ' '
	if string(fixtureTool(t, manager, "echo").Parameters) != original {
		t.Fatal("caller mutated catalog")
	}
	for _, mode := range []string{"duplicate", "repeated-cursor", "invalid-name", "external-ref"} {
		t.Run(mode, func(t *testing.T) {
			_, err := Open(context.Background(), t.TempDir(), t.TempDir(), []mcpconfig.Server{{Name: "fixture", Transport: "stdio", Command: fixtureBinary, Args: []string{"-mode", mode}}}, nil, nil)
			if err == nil {
				t.Fatal("accepted invalid discovery")
			}
		})
	}
}
func TestMCPListChanged(t *testing.T) {
	manager, _ := openFixture(t, "normal")
	old := fixtureTool(t, manager, "echo")
	if result, err := fixtureTool(t, manager, "change").Execute(context.Background(), "change", json.RawMessage(`{}`), nil); err != nil || result.IsError {
		t.Fatalf("change: %#v %v", result, err)
	}
	// The notification handler schedules refresh synchronously on the reader. A ping
	// behind those notifications proves that all bursts have been admitted.
	manager.servers[0].mu.Lock()
	client := manager.servers[0].client
	manager.servers[0].mu.Unlock()
	if _, err := client.Call(context.Background(), "ping", struct{}{}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fixtureTool(t, manager, "echo").Parameters), "changed") || strings.Contains(string(old.Parameters), "changed") {
		t.Fatal("catalog wasn't immutable across refresh")
	}
	_ = fixtureTool(t, manager, "added")
}
func TestMCPCancelCrashRestart(t *testing.T) {
	manager, ledger := openFixture(t, "normal")
	tool := fixtureTool(t, manager, "block")
	ctx, cancel := context.WithCancel(context.Background())
	settled := make(chan agent.AgentToolResult, 1)
	entered := make(chan struct{})
	var once sync.Once
	go func() {
		result, _ := tool.Execute(ctx, "blocked", json.RawMessage(`{}`), func(agent.AgentToolResult) { once.Do(func() { close(entered) }) })
		settled <- result
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("call not dispatched")
	}
	cancel()
	select {
	case result := <-settled:
		if !result.IsError || !strings.Contains(result.Content[0].Text, "outcome may be unknown") {
			t.Fatalf("uncertain cancellation: %#v", result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel didn't settle")
	}
	if ledgerDispatches(t, ledger, "block") != 1 {
		t.Fatal("cancel replayed call")
	}
	if err := manager.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if result, err := fixtureTool(t, manager, "count").Execute(context.Background(), "next", json.RawMessage(`{}`), nil); err != nil || result.IsError {
		t.Fatalf("restart: %#v %v", result, err)
	}
	if result, _ := fixtureTool(t, manager, "crash").Execute(context.Background(), "crash", json.RawMessage(`{}`), nil); !result.IsError || !strings.Contains(result.Content[0].Text, "outcome may be unknown") {
		t.Fatalf("crash: %#v", result)
	}
	if err := manager.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ledgerDispatches(t, ledger, "crash") != 1 {
		t.Fatal("crashed call replayed")
	}
}
func TestMCPResultsAndLimits(t *testing.T) {
	manager, _ := openFixture(t, "normal")
	result, err := fixtureTool(t, manager, "mixed").Execute(context.Background(), "mixed", json.RawMessage(`{}`), nil)
	if err != nil || !result.IsError || len(result.Content) < 7 || result.Content[0].Text != "first" || result.Content[1].Type != "image" {
		t.Fatalf("mixed mapping: %#v %v", result, err)
	}
	var details resultDetails
	if err = json.Unmarshal(result.Details, &details); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(details.MCP.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"type":"unknown"`) {
		t.Fatal("unknown item lost")
	}
	info, err := os.Stat(details.MCP.OutputPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("spill permissions: %v %v", info, err)
	}
	result, err = fixtureTool(t, manager, "large").Execute(context.Background(), "large", json.RawMessage(`{}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("large result: %#v %v", result, err)
	}
	encoded, _ := json.Marshal(result.Content)
	if len(encoded) > maxInlineContent || len(result.Details) > maxDetails {
		t.Fatal("inline limits exceeded")
	}
	if json.Unmarshal(result.Details, &details) != nil || !details.MCP.Truncated || details.MCP.OutputPath == "" {
		t.Fatalf("unmarked truncation: %s", result.Details)
	}
}
