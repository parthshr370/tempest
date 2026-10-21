package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/rpc"
	"go.harness.dev/harness/tempest"
)

func TestUpdatePreservesRawNumbersAndExtensionMetadata(t *testing.T) {
	a, b := net.Pipe()
	conn := rpc.NewStream(a)
	defer conn.Close()
	defer b.Close()
	go conn.Serve(nil)
	client := NewClient(conn, nil, nil)
	update := tempest.Update{Seq: 42, SessionID: "s", Kind: "tool_call", Update: json.RawMessage(`{"sessionUpdate":"tool_call","toolCallId":"c","rawInput":{"number":9007199254740993},"_meta":{"tempest":{"existing":true}}}`)}
	if err := client.sendUpdate(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(b).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(line), `9007199254740993`) || !strings.Contains(string(line), `"seq":42`) || !strings.Contains(string(line), `"existing":true`) {
		t.Fatalf("lossy update: %s", line)
	}
}
