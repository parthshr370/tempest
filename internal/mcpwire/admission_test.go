package mcpwire

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"go.harness.dev/harness/internal/mcpconfig"
)

func TestStdioProcessAdmissionBlocksAndReleasesStartFailure(t *testing.T) {
	denied := errors.New("host process limit")
	cfg := mcpconfig.Server{Name: "fixture", Transport: "stdio", Command: "/no/such/tempest-mcp"}
	if _, err := dialStdio(context.Background(), cfg, t.TempDir(), Handlers{AdmitProcess: func(context.Context) (func(), error) { return nil, denied }}, credentialRedactor{}); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	var held atomic.Int32
	if _, err := dialStdio(context.Background(), cfg, t.TempDir(), Handlers{AdmitProcess: func(context.Context) (func(), error) { held.Add(1); return func() { held.Add(-1) }, nil }}, credentialRedactor{}); err == nil || held.Load() != 0 {
		t.Fatalf("start failure leaked process slot: held=%d err=%v", held.Load(), err)
	}
}
