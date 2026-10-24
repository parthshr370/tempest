package evalkernel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func javascriptRuntime(t *testing.T, runtime string) *Manager {
	t.Helper()
	executable, err := exec.LookPath(runtime)
	if err != nil {
		t.Skip(runtime + " unavailable")
	}
	dir := t.TempDir()
	if err := os.Symlink(executable, filepath.Join(dir, runtime)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	kernels := New(t.TempDir(), t.TempDir(), nil)
	t.Cleanup(func() { _ = kernels.Close() })
	return kernels
}

func TestJavaScriptReliability(t *testing.T) {
	for _, runtime := range []string{"node", "bun"} {
		t.Run(runtime, func(t *testing.T) {
			kernels := javascriptRuntime(t, runtime)
			for _, thrown := range []string{"null", "undefined", "Symbol('odd')", "Object.create(null)", "({code: '', name: ''})", "new Proxy({}, {get() { throw null; }})"} {
				result := cell(t, kernels, Request{Language: "js", Code: "throw " + thrown})
				if !result.Failed || result.Output.Content == "" {
					t.Fatalf("throw %s: %+v", thrown, result)
				}
			}
			cell(t, kernels, Request{Language: "js", Code: "var fs = require('node:fs'); var nativeWrite = fs.writeSync; var retries = 0; fs.writeSync = (...args) => { if (retries++ < 2) { const error = new Error('retry output'); error.code = retries === 1 ? 'EAGAIN' : 'EINTR'; throw error; } return nativeWrite(...args); }; void 0"})
			result := cell(t, kernels, Request{Language: "js", Code: "process.stdout.write('x'.repeat(4000000)); console.warn('q'.repeat(131072)); void 0"})
			if result.Failed || !result.Output.Truncation.Truncated || result.Output.FullOutputPath == "" {
				t.Fatalf("large output failed or lost truncation metadata: %+v", result)
			}
			full, err := os.ReadFile(result.Output.FullOutputPath)
			if err != nil || string(full) != strings.Repeat("x", 4000000)+strings.Repeat("q", 131072)+"\n" {
				t.Fatalf("large output: got %d bytes; err=%v", len(full), err)
			}
			result = cell(t, kernels, Request{Language: "js", Code: "console.warn('still writing'); void 0"})
			if result.Output.Content != "still writing\n" {
				t.Fatalf("console stayed broken: %+v", result)
			}
			cell(t, kernels, Request{Language: "js", Code: "let timerFired = false; let microtaskRan = false; setTimeout(() => { timerFired = true; }, 10); Promise.resolve().then(() => { microtaskRan = true; }); void 0"})
			time.Sleep(100 * time.Millisecond)
			result = cell(t, kernels, Request{Language: "js", Code: "JSON.stringify({timerFired, microtaskRan})"})
			if !strings.Contains(result.Output.Content, `"timerFired":true,"microtaskRan":true`) {
				t.Fatalf("callbacks didn't run between cells: %+v", result)
			}
			for _, code := range []string{"process.exit(23)", "require('node:fs').writeSync = () => { throw new Error('output write failed'); }; throw null"} {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				result, err = kernels.Eval(ctx, Request{Language: "js", Code: code})
				cancel()
				if err == nil || !strings.Contains(err.Error(), "outcome unknown") || !result.Failed {
					t.Fatalf("fatal driver failure didn't settle: %+v %v", result, err)
				}
				cell(t, kernels, Request{Language: "js", Code: "1", Reset: true})
			}
		})
	}
}

func TestJavaScriptWatchdogWhileBusy(t *testing.T) {
	for _, runtime := range []string{"node", "bun"} {
		t.Run(runtime, func(t *testing.T) {
			kernels := javascriptRuntime(t, runtime)
			cell(t, kernels, Request{Language: "js", Code: "1"})
			process := kernels.kernels["js"]
			payload := []byte(`{"code":"while (true) {}","boundary":"end"}`)
			if err := writeFrame(process.request, payload); err != nil {
				t.Fatal(err)
			}
			if response, err := readEvent(process.response); err != nil || response.Event != "started" {
				t.Fatalf("busy cell: %+v %v", response, err)
			}
			if err := process.lifetime.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-process.waited:
			case <-time.After(5 * time.Second):
				t.Fatal("busy runtime survived harness lifetime EOF")
			}
		})
	}
}

func TestPythonBrokenTransportSettlesWithoutCellDeadline(t *testing.T) {
	for _, code := range []string{
		"import os\nos.dup2(os.open(os.devnull, os.O_WRONLY), 1)",
		"import os\nos.system('/bin/sleep 60 &')\nos._exit(1)",
	} {
		t.Run(code, func(t *testing.T) {
			kernels := pythonKernels(t)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			result, err := kernels.Eval(ctx, Request{Language: "py", Code: code})
			if err == nil || !strings.Contains(err.Error(), "outcome unknown") || !result.Failed || ctx.Err() != nil {
				t.Fatalf("broken transport didn't settle before context: %+v %v", result, err)
			}
			if _, err := kernels.Eval(t.Context(), Request{Language: "py", Code: "1"}); err == nil {
				t.Fatal("broken transport preserved a usable kernel")
			}
		})
	}
}

func TestPythonDriverImportsAreIsolated(t *testing.T) {
	kernels := pythonKernels(t)
	for _, name := range []string{"ast", "json", "struct", "signal", "traceback"} {
		if err := os.WriteFile(filepath.Join(kernels.cwd, name+".py"), []byte("raise RuntimeError('workspace module ran during startup')"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(kernels.cwd, "workspace_module.py"), []byte("answer = 42"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := cell(t, kernels, Request{Language: "py", Code: "import workspace_module\nworkspace_module.answer"})
	if result.Output.Content != "42\n" {
		t.Fatalf("isolated driver lost cell workspace imports: %+v", result)
	}
	result = cell(t, kernels, Request{Language: "py", Code: "import os\n(os.get_inheritable(3), os.get_inheritable(4))"})
	if result.Output.Content != "(False, False)\n" {
		t.Fatalf("protocol descriptors are inheritable: %+v", result)
	}
}

func TestTimeoutDoesNotInterruptEarlierSubprocess(t *testing.T) {
	kernels := pythonKernels(t)
	result := cell(t, kernels, Request{Language: "py", Code: "import subprocess\nserver = subprocess.Popen(['/bin/sleep', '60'])\nserver.pid"})
	pid, err := strconv.Atoi(strings.TrimSpace(result.Output.Content))
	if err != nil {
		t.Fatal(err)
	}
	result = cell(t, kernels, Request{Language: "py", Code: "while True: pass", Timeout: 50 * time.Millisecond})
	if !result.TimedOut || syscall.Kill(pid, 0) != nil {
		t.Fatalf("timeout killed an earlier cell's subprocess: %+v", result)
	}
	// Force host SIGINT too, not just the Python driver's own deadline.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := kernels.Eval(ctx, Request{Language: "py", Code: "while True: pass"}); err == nil || syscall.Kill(pid, 0) != nil {
		t.Fatalf("cancellation killed an earlier cell's subprocess: %v", err)
	}
}

func TestMarkStateLostStopsNamespaces(t *testing.T) {
	kernels := pythonKernels(t)
	cell(t, kernels, Request{Language: "py", Code: "abandoned = 42"})
	process := kernels.kernels["py"]
	kernels.MarkStateLost()
	select {
	case <-process.waited:
	default:
		t.Fatal("rewind invalidation returned before kernel exit")
	}
	if _, err := kernels.Eval(t.Context(), Request{Language: "py", Code: "abandoned"}); err == nil {
		t.Fatal("abandoned namespace survived invalidation")
	}
	result := cell(t, kernels, Request{Language: "py", Code: "abandoned", Reset: true})
	if !result.Failed || !strings.Contains(result.Output.Content, "NameError") {
		t.Fatal(result)
	}
	cell(t, kernels, Request{Language: "js", Code: "1"})
}

func TestStartupCancellationDoesNotHoldManagerLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "python3"), []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	kernels := New(t.TempDir(), t.TempDir(), nil)
	defer kernels.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := kernels.Eval(ctx, Request{Language: "py", Code: "1"})
		finished <- err
	}()
	deadline := time.After(5 * time.Second)
	for {
		kernels.mu.Lock()
		starting := len(kernels.starting) > 0
		kernels.mu.Unlock()
		if starting {
			break
		}
		select {
		case <-deadline:
			t.Fatal("startup did not begin")
		case <-time.After(time.Millisecond):
		}
	}
	closed := make(chan error, 1)
	go func() { closed <- kernels.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for runtime startup under the manager lock")
	}
	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
			t.Fatalf("startup ignored cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startup didn't settle after cancellation")
	}
}

func TestStartupFailureIncludesOutput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "python3"), []byte("#!/bin/sh\nprintf 'runtime startup diagnostic' >&2\nexit 23\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	kernels := New(t.TempDir(), t.TempDir(), nil)
	defer kernels.Close()
	if _, err := kernels.Eval(t.Context(), Request{Language: "py", Code: "1"}); err == nil || !strings.Contains(err.Error(), "runtime startup diagnostic") {
		t.Fatalf("startup failure hid its output: %v", err)
	}
}

func TestPythonDeadlineIsReportedEvenWhenCaught(t *testing.T) {
	kernels := pythonKernels(t)
	result := cell(t, kernels, Request{Language: "py", Code: "try:\n    while True: pass\nexcept KeyboardInterrupt:\n    print('caught deadline')", Timeout: 20 * time.Millisecond})
	if !result.TimedOut || !result.Failed || !strings.Contains(result.Output.Content, "caught deadline") {
		t.Fatalf("caught deadline was reported as success: %+v", result)
	}
	for range 20 {
		result = cell(t, kernels, Request{Language: "py", Code: "1", Timeout: 20 * time.Millisecond})
		if result.TimedOut || result.Failed {
			t.Fatalf("late host timer misclassified a completed cell: %+v", result)
		}
	}
}
