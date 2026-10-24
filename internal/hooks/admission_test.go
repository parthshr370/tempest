package hooks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestCommandHookProcessAdmissionOwnsActualLifetime(t *testing.T) {
	cwd := t.TempDir()
	runner, err := New(commandSettings("PreToolUse", "touch ran"), Input{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("host process limit")
	runner.AdmitProcess = func(context.Context) (func(), error) { return nil, denied }
	var starts atomic.Int32
	ctx := WithCommandStart(context.Background(), func() error {
		starts.Add(1)
		return nil
	})
	if _, err = runner.Run(ctx, Input{Event: "PreToolUse", ToolName: "write"}); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(cwd, "ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("denied shell ran", err)
	}
	if starts.Load() != 0 {
		t.Fatal("denied permit recorded a started command")
	}
	var held atomic.Int32
	runner.AdmitProcess = func(context.Context) (func(), error) { held.Add(1); return func() { held.Add(-1) }, nil }
	if _, err = runner.Run(ctx, Input{Event: "PreToolUse", ToolName: "write"}); err != nil {
		t.Fatal(err)
	}
	if held.Load() != 0 {
		t.Fatal("completed shell retained host slot")
	}
	if starts.Load() != 1 {
		t.Fatal("admitted command didn't record its intent")
	}
	if _, err = os.Stat(filepath.Join(cwd, "ran")); err != nil {
		t.Fatal(err)
	}
}
