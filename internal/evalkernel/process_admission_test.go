package evalkernel

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestKernelPermitCoversLifetimeAndStartFailure(t *testing.T) {
	var active atomic.Int32
	admit := func(context.Context) (func(), error) { active.Add(1); return func() { active.Add(-1) }, nil }
	if _, err := startKernel(t.Context(), "/does-not-exist", "py", admit); err == nil {
		t.Fatal("invalid cwd accepted")
	}
	if active.Load() != 0 {
		t.Fatal("failed startup retained permit")
	}
	manager := New(t.TempDir(), t.TempDir(), nil, admit)
	defer manager.Close()
	if _, err := manager.Eval(t.Context(), Request{Language: "py", Code: "value = 1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Eval(t.Context(), Request{Language: "py", Code: "value += 1"}); err != nil {
		t.Fatal(err)
	}
	if active.Load() != 1 {
		t.Fatal("permit must cover kernel, not one cell")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if active.Load() != 0 {
		t.Fatal("closed kernel retained permit")
	}
}
