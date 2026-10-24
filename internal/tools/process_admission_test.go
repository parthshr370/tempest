package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
)

func TestBashProcessAdmissionReleasesOnStartFailureAndExit(t *testing.T) {
	var active atomic.Int32
	denied := errors.New("host full")
	admit := func(context.Context) (func(), error) {
		if !active.CompareAndSwap(0, 1) {
			return nil, denied
		}
		return func() { active.Add(-1) }, nil
	}
	release, err := admit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	tool := newBashTool(t.TempDir(), ToolsOptions{AdmitProcess: admit})
	if _, err := tool.Execute(t.Context(), "call", json.RawMessage(`{"command":"true"}`), nil); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	release()
	for _, shell := range []string{"/does-not-exist", "/bin/bash"} {
		tool := newBashTool(t.TempDir(), ToolsOptions{ShellPath: shell, AdmitProcess: admit})
		_, err := tool.Execute(context.Background(), "call", json.RawMessage(`{"command":"true"}`), nil)
		if shell == "/bin/bash" && err != nil {
			t.Fatal(err)
		}
		if active.Load() != 0 {
			t.Fatal("process permit leaked")
		}
	}
}
