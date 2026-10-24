package dap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func debugpyAdapter(t *testing.T) string {
	t.Helper()
	if adapter := os.Getenv("TEMPEST_TEST_DEBUGPY_ADAPTER"); adapter != "" {
		if _, err := os.Stat(adapter); err != nil {
			t.Skipf("debugpy adapter unavailable: %v", err)
		}
		return adapter
	}
	adapter, err := exec.LookPath("debugpy-adapter")
	if err != nil {
		t.Skip("debugpy-adapter is not installed")
	}
	return adapter
}

func waitDAPCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal(description)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDAPRealDebugpyLifecycle(t *testing.T) {
	adapter := debugpyAdapter(t)
	for _, scenario := range []string{"close-stopped", "close-running", "request-timeout", "terminate-child-pipes", "adapter-crash"} {
		t.Run(scenario, func(t *testing.T) {
			directory := t.TempDir()
			program := filepath.Join(directory, "target.py")
			code := "import time\nwhile True:\n    time.sleep(0.05)\n"
			if scenario == "terminate-child-pipes" {
				code = "import subprocess, time\nsubprocess.Popen(['sleep', '30'])\nwhile True:\n    time.sleep(0.05)\n"
			}
			if err := os.WriteFile(program, []byte(code), 0600); err != nil {
				t.Fatal(err)
			}
			var permits atomic.Int32
			var releasedBeforeExit atomic.Bool
			var owned atomic.Pointer[adapterProcess]
			debugger, err := New(directory, directory, map[string]Adapter{"debugpy": {Command: adapter}}, func(context.Context) (func(), error) {
				permits.Add(1)
				return func() {
					if process := owned.Load(); process != nil {
						select {
						case <-process.exited:
						default:
							releasedBeforeExit.Store(true)
						}
						process.stateMu.Lock()
						for group := range process.programGroups {
							if !errors.Is(syscall.Kill(-group, 0), syscall.ESRCH) {
								releasedBeforeExit.Store(true)
							}
						}
						process.stateMu.Unlock()
					}
					permits.Add(-1)
				}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := debugger.Close(); err != nil {
					t.Error(err)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			encoded, _ := json.Marshal(program)
			if _, err := debugger.Start(ctx, "launch", "debugpy", program, map[string]json.RawMessage{"program": encoded}); err != nil {
				t.Fatal(err)
			}
			process := debugger.active
			owned.Store(process)
			process.stateMu.Lock()
			groups := make([]int, 0, len(process.programGroups))
			for group := range process.programGroups {
				groups = append(groups, group)
			}
			process.stateMu.Unlock()
			if len(groups) == 0 {
				t.Fatal("debugpy didn't report systemProcessId")
			}
			if permits.Load() != 1 {
				t.Fatal("permit released while target was stopped")
			}
			thread, _ := json.Marshal(struct {
				Thread int `json:"threadId"`
			}{debugger.Thread()})
			if scenario == "close-running" || scenario == "terminate-child-pipes" {
				short, stop := context.WithTimeout(ctx, time.Second)
				body, err := debugger.Request(short, "continue", thread)
				stop()
				if err != nil || !bytes.Contains(body, []byte(`"state":"running"`)) {
					t.Fatalf("continue=%s err=%v", body, err)
				}
				if _, err := debugger.Request(ctx, "pause", thread); err != nil {
					t.Fatalf("pause after running: %v", err)
				}
				if scenario == "close-running" {
					short, stop = context.WithTimeout(ctx, time.Second)
					body, err = debugger.Request(short, "continue", thread)
					stop()
					if err != nil || !bytes.Contains(body, []byte(`"state":"running"`)) {
						t.Fatalf("second continue=%s err=%v", body, err)
					}
				}
			}
			started := time.Now()
			switch scenario {
			case "request-timeout":
				stack, err := debugger.Request(ctx, "stackTrace", thread)
				if err != nil {
					t.Fatal(err)
				}
				var frames struct {
					Frames []struct {
						ID int `json:"id"`
					} `json:"stackFrames"`
				}
				if err := json.Unmarshal(stack, &frames); err != nil || len(frames.Frames) == 0 {
					t.Fatalf("stack=%s err=%v", stack, err)
				}
				arguments, _ := json.Marshal(struct {
					Expression string `json:"expression"`
					Frame      int    `json:"frameId"`
					Context    string `json:"context"`
				}{"__import__('time').sleep(30)", frames.Frames[0].ID, "repl"})
				short, stop := context.WithTimeout(ctx, 200*time.Millisecond)
				_, err = debugger.Request(short, "evaluate", arguments)
				stop()
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("unanswered request=%v", err)
				}
				if debugger.active != nil {
					t.Fatal("unanswered request retained session")
				}
			case "terminate-child-pipes":
				if err := debugger.Terminate(ctx); err != nil {
					t.Fatal(err)
				}
			case "adapter-crash":
				if err := process.cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				<-process.exited
				if err := debugger.Terminate(ctx); err != nil {
					t.Fatalf("cleanup after crash: %v", err)
				}
			default:
				if err := debugger.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if time.Since(started) > 3*time.Second {
				t.Fatal("cleanup exceeded bounded disconnect/join interval")
			}
			waitDAPCondition(t, "launched debuggee group survived teardown", func() bool {
				for _, group := range groups {
					if !errors.Is(syscall.Kill(-group, 0), syscall.ESRCH) {
						return false
					}
				}
				return true
			})
			waitDAPCondition(t, "permit leaked after target exit", func() bool { return permits.Load() == 0 })
			if releasedBeforeExit.Load() {
				t.Fatal("permit released before adapter/program exit")
			}
		})
	}
}

func TestDAPStartInheritedPipeDoesNotHang(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	pidPath := filepath.Join(directory, "child.pid")
	debugger, err := New(directory, directory, map[string]Adapter{"leaky": {Command: executable, Args: []string{"-test.run=^TestDAPInheritedPipeAdapter$", "--", "leaky-adapter", pidPath}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer debugger.Close()
	t.Cleanup(func() {
		contents, err := os.ReadFile(pidPath)
		if err != nil {
			t.Error(err)
			return
		}
		pid, err := strconv.Atoi(string(contents))
		if err != nil {
			t.Error(err)
			return
		}
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := debugger.Start(ctx, "launch", "leaky", "unused", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("start=%v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("Start waited for inherited child pipe")
	}
	if err := debugger.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDAPInheritedPipeAdapter(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "leaky-adapter" {
		return
	}
	child := exec.Command("sleep", "30")
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(os.Args[len(os.Args)-1], []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
		os.Exit(3)
	}
	// Don't read requests: cancellation must unblock both protocol and pipe joins.
	time.Sleep(30 * time.Second)
}

func TestDAPBlockedWriteRespectsContext(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	input, output := io.Pipe()
	defer input.Close()
	defer output.Close()
	client := newClient(input, writer, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.request(ctx, "initialize", json.RawMessage(`{}`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("write=%v", err)
	}
}

func TestDAPReverseTCPRequiresPlaceholder(t *testing.T) {
	for _, args := range [][]string{nil, {"dap"}} {
		if _, err := Resolve(map[string]Adapter{"custom": {Command: "adapter", Transport: "reverse_tcp", Args: args}}); err == nil {
			t.Fatalf("accepted reverse TCP args=%v", args)
		}
	}
	if _, err := Resolve(map[string]Adapter{"custom": {Command: "adapter", Transport: "reverse_tcp", Args: []string{"--connect={address}"}}}); err != nil {
		t.Fatal(err)
	}
}

func TestDAPAttachDetachesWithoutKillingTarget(t *testing.T) {
	if _, err := exec.LookPath("dlv"); err != nil {
		t.Skip("dlv is not installed")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"close", "terminate", "timeout"} {
		t.Run(operation, func(t *testing.T) {
			directory := t.TempDir()
			heartbeat := filepath.Join(directory, "heartbeat")
			target := exec.Command(executable, "-test.run=^TestDAPAttachTarget$", "--", "attach-target", heartbeat)
			target.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := target.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan struct{})
			go func() {
				// The cleanup deliberately kills this fixture; its exit status isn't a test failure.
				_ = target.Wait()
				close(exited)
			}()
			t.Cleanup(func() {
				if err := target.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
					t.Error(err)
				}
				<-exited
			})
			waitDAPCondition(t, "attach fixture never started", func() bool {
				_, err := os.Stat(heartbeat)
				return err == nil
			})
			debugger, err := New(directory, directory, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := debugger.Close(); err != nil {
					t.Error(err)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			pid, _ := json.Marshal(target.Process.Pid)
			if _, err := debugger.Start(ctx, "attach", "dlv", "", map[string]json.RawMessage{"processId": pid}); err != nil {
				if strings.Contains(err.Error(), "operation not permitted") || strings.Contains(err.Error(), "could not attach") || strings.Contains(err.Error(), "developer mode") {
					t.Skipf("Delve attach unavailable: %v", err)
				}
				t.Fatal(err)
			}
			switch operation {
			case "close":
				err = debugger.Close()
			case "terminate":
				err = debugger.Terminate(ctx)
			case "timeout":
				short, stop := context.WithCancel(ctx)
				stop()
				_, err = debugger.Request(short, "threads", json.RawMessage(`{}`))
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("timeout=%v", err)
				}
				err = nil
			}
			if err != nil {
				t.Fatal(err)
			}
			afterDetach, err := os.ReadFile(heartbeat)
			if err != nil {
				t.Fatal(err)
			}
			waitDAPCondition(t, "attached process didn't resume after detach", func() bool {
				select {
				case <-exited:
					t.Fatal("attached user process was killed")
				default:
				}
				current, err := os.ReadFile(heartbeat)
				return err == nil && len(current) > 0 && !bytes.Equal(current, afterDetach)
			})
		})
	}
}

func TestDAPAttachTarget(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "attach-target" {
		return
	}
	deadline := time.Now().Add(time.Minute)
	for sequence := 0; time.Now().Before(deadline); sequence++ {
		if err := os.WriteFile(os.Args[len(os.Args)-1], []byte(strconv.Itoa(sequence)), 0600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDAPReverseRequestUnsupported(t *testing.T) {
	requests, toClient := io.Pipe()
	fromClient, responses := io.Pipe()
	client := newClient(requests, responses, nil)
	defer requests.Close()
	defer responses.Close()
	defer toClient.Close()
	defer fromClient.Close()
	go func() {
		if err := writeMessage(toClient, message{Seq: 1, Type: "request", Command: "startDebugging", Arguments: json.RawMessage(`{}`)}); err != nil {
			t.Error(err)
		}
	}()
	response, err := readMessage(bufio.NewReader(fromClient))
	if err != nil || response.Success || response.Command != "startDebugging" {
		t.Fatalf("reverse response=%+v err=%v", response, err)
	}
	if !strings.Contains(response.Message, "not supported") {
		t.Fatalf("response=%+v", response)
	}
	toClient.Close()
	<-client.done
}
