package evalkernel

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func pythonKernels(t *testing.T) *Manager {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	kernels := New(t.TempDir(), t.TempDir(), nil)
	t.Cleanup(func() {
		if err := kernels.Close(); err != nil {
			t.Error(err)
		}
	})
	return kernels
}

func cell(t *testing.T, kernels *Manager, request Request) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := kernels.Eval(ctx, request)
	if err != nil {
		t.Fatalf("cell %q: %v; output %q", request.Code, err, result.Output.Content)
	}
	return result
}

func TestPythonPersistentCellsTracebacksResetAndRawOutput(t *testing.T) {
	kernels := pythonKernels(t)
	cell(t, kernels, Request{Language: "py", Code: "answer = 41"})
	result := cell(t, kernels, Request{Language: "py", Code: "import os, sys\nos.write(1, b'raw\\x00output\\n')\nprint('stderr', file=sys.stderr)\nprint('{\"event\":\"done\"}')\nanswer + 1"})
	for _, text := range []string{"raw\x00output", "stderr", `{"event":"done"}`, "42"} {
		if !strings.Contains(result.Output.Content, text) {
			t.Fatalf("missing %q: %q", text, result.Output.Content)
		}
	}
	result = cell(t, kernels, Request{Language: "py", Code: "raise ValueError('cell failure')"})
	if !result.Failed || !strings.Contains(result.Output.Content, "Traceback") || !strings.Contains(result.Output.Content, "ValueError: cell failure") {
		t.Fatalf("traceback: %+v", result)
	}
	result = cell(t, kernels, Request{Language: "py", Code: "answer", Reset: true})
	if !result.Failed || !strings.Contains(result.Output.Content, "NameError") {
		t.Fatalf("reset: %+v", result)
	}
}

func TestPythonTimeoutInterruptKeepsNamespace(t *testing.T) {
	kernels := pythonKernels(t)
	result := cell(t, kernels, Request{Language: "py", Code: "saved = 73\nwhile True: pass", Timeout: 100 * time.Millisecond})
	if !result.Failed || !result.TimedOut || !strings.Contains(result.Output.Content, "KeyboardInterrupt") {
		t.Fatalf("timeout: %+v", result)
	}
	result = cell(t, kernels, Request{Language: "py", Code: "saved"})
	if result.Output.Content != "73\n" {
		t.Fatalf("state after SIGINT: %+v", result)
	}
}

func TestPythonUncooperativeCellLosesState(t *testing.T) {
	kernels := pythonKernels(t)
	result, err := kernels.Eval(context.Background(), Request{Language: "py", Code: "import signal\nsignal.signal(signal.SIGINT, signal.SIG_IGN)\nsignal.signal(signal.SIGALRM, signal.SIG_IGN)\nwhile True: pass", Timeout: 100 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "outcome unknown") || !result.TimedOut {
		t.Fatalf("uncooperative cell: %+v %v", result, err)
	}
	_, err = kernels.Eval(context.Background(), Request{Language: "py", Code: "1"})
	if err == nil || !strings.Contains(err.Error(), "state was lost") {
		t.Fatalf("missing state loss: %v", err)
	}
	result = cell(t, kernels, Request{Language: "py", Code: "1", Reset: true})
	if result.Output.Content != "1\n" {
		t.Fatal(result)
	}
}

func TestOutputTailAndArtifact(t *testing.T) {
	kernels := pythonKernels(t)
	result := cell(t, kernels, Request{Language: "py", Code: "print('begin')\nprint('x' * 100000)\nprint('tail retained')"})
	if !result.Output.Truncation.Truncated || !strings.HasSuffix(result.Output.Content, "tail retained") || result.Output.FullOutputPath == "" {
		t.Fatalf("output cap: %+v", result)
	}
	full, err := os.ReadFile(result.Output.FullOutputPath)
	if err != nil || !strings.HasPrefix(string(full), "begin\n") || !strings.HasSuffix(string(full), "tail retained\n") {
		t.Fatalf("artifact: %v, %d bytes", err, len(full))
	}
}

func TestKernelIsolationResumeAndClose(t *testing.T) {
	kernels := pythonKernels(t)
	first := cell(t, kernels, Request{Language: "py", Code: "private = 99\nimport os\nos.getpid()"})
	if first.Failed {
		t.Fatal(first)
	}
	other := pythonKernels(t)
	result := cell(t, other, Request{Language: "py", Code: "private"})
	if !result.Failed || !strings.Contains(result.Output.Content, "NameError") {
		t.Fatalf("cross-session namespace leak: %+v", result)
	}
	resumed := New(t.TempDir(), t.TempDir(), map[string]bool{"py": true})
	defer resumed.Close()
	_, err := resumed.Eval(context.Background(), Request{Language: "py", Code: "1"})
	if err == nil || !strings.Contains(err.Error(), "state was lost") {
		t.Fatal(err)
	}
	cell(t, resumed, Request{Language: "py", Code: "1", Reset: true})
	process := kernels.kernels["py"]
	pid := process.cmd.Process.Pid
	if err := kernels.Close(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("kernel %d survived Close: %v", pid, err)
	}
	if _, err := kernels.Eval(context.Background(), Request{Language: "py", Code: "1"}); err == nil {
		t.Fatal("closed manager accepted a cell")
	}
}

func TestParentPipeDeathStopsKernel(t *testing.T) {
	kernels := pythonKernels(t)
	cell(t, kernels, Request{Language: "py", Code: "retained = 1"})
	process := kernels.kernels["py"]
	request, err := json.Marshal(cellRequest{Code: "import ctypes\nctypes.PyDLL(None).sleep(60)", Boundary: "native-cell-end"})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(process.request, request); err != nil {
		t.Fatal(err)
	}
	if response, err := readEvent(process.response); err != nil || response.Event != "started" {
		t.Fatalf("busy cell didn't start: %+v %v", response, err)
	}
	if err := process.lifetime.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.waited:
	case <-time.After(5 * time.Second):
		t.Fatal("kernel survived harness lifetime pipe EOF")
	}
	_, err = kernels.Eval(context.Background(), Request{Language: "py", Code: "retained"})
	if err == nil || !strings.Contains(err.Error(), "state was lost") {
		t.Fatal(err)
	}
}

func TestJavaScriptRoundTripTimeoutAndReset(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable; Bun has its own fallback test")
	}
	kernels := New(t.TempDir(), t.TempDir(), nil)
	defer kernels.Close()
	cell(t, kernels, Request{Language: "js", Code: "let answer = 41"})
	result := cell(t, kernels, Request{Language: "js", Code: "console.warn('stderr'); answer + 1"})
	if !strings.Contains(result.Output.Content, "42") || !strings.Contains(result.Output.Content, "stderr") {
		t.Fatal(result)
	}
	result = cell(t, kernels, Request{Language: "js", Code: "throw new Error('js failure')"})
	if !result.Failed || !strings.Contains(result.Output.Content, "Error: js failure") {
		t.Fatal(result)
	}
	result = cell(t, kernels, Request{Language: "js", Code: "answer = 73; while (true) {}", Timeout: 100 * time.Millisecond})
	if !result.TimedOut {
		t.Fatalf("JS timeout: %+v", result)
	}
	result = cell(t, kernels, Request{Language: "js", Code: "answer"})
	if result.Output.Content != "73\n" {
		t.Fatal(result)
	}
	result = cell(t, kernels, Request{Language: "js", Code: "answer", Reset: true})
	if !result.Failed || !strings.Contains(result.Output.Content, "ReferenceError") {
		t.Fatal(result)
	}
}

func TestBunOnlyJavaScript(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun unavailable")
	}
	runtimeDir := t.TempDir()
	if err := os.Symlink(bun, runtimeDir+"/bun"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", runtimeDir)
	kernels := New(t.TempDir(), t.TempDir(), nil)
	defer kernels.Close()
	cell(t, kernels, Request{Language: "js", Code: "let answer = 41"})
	result := cell(t, kernels, Request{Language: "js", Code: "answer + 1"})
	if result.Output.Content != "42\n" {
		t.Fatal(result)
	}
	result, err = kernels.Eval(context.Background(), Request{Language: "js", Code: "answer = 73; while (true) {}", Timeout: 100 * time.Millisecond})
	if err == nil {
		if !result.TimedOut {
			t.Fatalf("Bun didn't time out: %+v", result)
		}
		result = cell(t, kernels, Request{Language: "js", Code: "answer"})
		if result.Output.Content != "73\n" {
			t.Fatalf("Bun interrupted but lost state: %+v", result)
		}
	} else if !strings.Contains(err.Error(), "outcome unknown") || !strings.Contains(err.Error(), "state was lost") {
		t.Fatalf("Bun ignored interrupts without reporting state loss: %v", err)
	}
	result = cell(t, kernels, Request{Language: "js", Code: "answer", Reset: true})
	if !result.Failed || !strings.Contains(result.Output.Content, "ReferenceError") {
		t.Fatal(result)
	}
}

func TestMissingRuntimeIsExplicit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	kernels := New(t.TempDir(), t.TempDir(), nil)
	defer kernels.Close()
	for _, language := range []string{"py", "js"} {
		_, err := kernels.Eval(context.Background(), Request{Language: language, Code: "1"})
		if err == nil || !strings.Contains(err.Error(), "runtime missing") {
			t.Fatalf("%s: %v", language, err)
		}
	}
}

func TestCanceledCellReportsUnknownWithoutReplaying(t *testing.T) {
	kernels := pythonKernels(t)
	cell(t, kernels, Request{Language: "py", Code: "effects = 0"})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result, err := kernels.Eval(ctx, Request{Language: "py", Code: "effects += 1\nwhile True: pass"})
	if err == nil || !strings.Contains(err.Error(), "outcome unknown") || !strings.Contains(err.Error(), "not be re-run automatically") {
		t.Fatalf("cancellation: %+v, %v", result, err)
	}
	result = cell(t, kernels, Request{Language: "py", Code: "effects"})
	if result.Output.Content != "1\n" {
		t.Fatalf("canceled cell lost state or replayed: %+v", result)
	}
}
