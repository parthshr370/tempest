package dap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDAPFraming(t *testing.T) {
	var wire bytes.Buffer
	original := message{Seq: 7, Type: "request", Command: "evaluate", Arguments: json.RawMessage(`{"expression":"λ"}`)}
	if err := writeMessage(&wire, original); err != nil {
		t.Fatal(err)
	}
	decoded, err := readMessage(bufio.NewReader(&wire))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Command != original.Command || !bytes.Equal(decoded.Arguments, original.Arguments) {
		t.Fatalf("message=%+v", decoded)
	}
	for _, invalid := range []string{"Content-Length: -1\r\n\r\n", "Content-Length: 16777217\r\n\r\n", "Content-Length: 2\r\nContent-Length: 2\r\n\r\n{}", "Other: 2\r\n\r\n{}", "Content-Length: 3\r\n\r\n{}"} {
		if _, err := readMessage(bufio.NewReader(strings.NewReader(invalid))); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestDAPFakeAdapterSequence(t *testing.T) {
	for _, startAction := range []string{"launch", "attach"} {
		t.Run(startAction, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			directory := t.TempDir()
			sequence := filepath.Join(directory, "sequence")
			var permits atomic.Int32
			debugger, err := New(directory, directory, map[string]Adapter{"fake": {Command: executable, Args: []string{"-test.run=^TestDAPAdapterProcess$", "--", "dap-helper", sequence}}}, func(context.Context) (func(), error) { permits.Add(1); return func() { permits.Add(-1) }, nil })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := debugger.Close(); err != nil {
					t.Error(err)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if _, err := debugger.Start(ctx, startAction, "fake", "program", map[string]json.RawMessage{"program": json.RawMessage(`"program"`)}); err != nil {
				t.Fatal(err)
			}
			for _, command := range []string{"setBreakpoints", "threads", "continue", "next", "stepIn", "stepOut", "stackTrace", "scopes", "variables", "evaluate"} {
				if _, err := debugger.Request(ctx, command, json.RawMessage(`{}`)); err != nil {
					t.Fatalf("%s: %v", command, err)
				}
			}
			snapshot, err := debugger.Output()
			if err != nil {
				t.Fatal(err)
			}
			if !snapshot.Truncation.Truncated || snapshot.FullOutputPath == "" {
				t.Fatalf("output wasn't capped: %+v", snapshot)
			}
			spilled, err := os.ReadFile(snapshot.FullOutputPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(spilled) != 40001 {
				t.Fatalf("spill bytes=%d", len(spilled))
			}
			if err := debugger.Terminate(ctx); err != nil {
				t.Fatal(err)
			}
			if permits.Load() != 0 {
				t.Fatalf("leaked process permit: %d", permits.Load())
			}
			recorded, err := os.ReadFile(sequence)
			if err != nil {
				t.Fatal(err)
			}
			want := "initialize\n" + startAction + "\nconfigurationDone\nsetBreakpoints\nthreads\ncontinue\nnext\nstepIn\nstepOut\nstackTrace\nscopes\nvariables\nevaluate\ndisconnect\n"
			if string(recorded) != want {
				t.Fatalf("sequence=%s", recorded)
			}
			if _, err := debugger.Request(ctx, "threads", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "gone after resume") {
				t.Fatalf("missing lost-session diagnostic: %v", err)
			}
		})
	}
}

// slowDetachDelay outlasts the old fixed teardown budget (disconnectTimeout
// plus disconnectGrace) and stays well inside detachTimeout.
const slowDetachDelay = time.Second

// An attach adapter that hasn't confirmed its detach still traces the target,
// so cleanup must let it finish: one answers disconnect late, the other never
// answers and detaches only when its client connection closes.
func TestDAPCleanupWaitsForUnconfirmedDetach(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"slow", "on-eof"} {
		t.Run(mode, func(t *testing.T) {
			directory := t.TempDir()
			sequence := filepath.Join(directory, "sequence")
			session, err := New(directory, directory, map[string]Adapter{"fake": {Command: executable, Args: []string{"-test.run=^TestDAPAdapterProcess$", "--", "dap-helper", sequence}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if _, err := session.Start(ctx, "attach", "fake", "", map[string]json.RawMessage{"detach": json.RawMessage(`"` + mode + `"`)}); err != nil {
				t.Fatal(err)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			recorded, err := os.ReadFile(sequence)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(string(recorded), "disconnect\ndetached\n") {
				t.Fatalf("adapter killed before its detach finished; sequence=%s", recorded)
			}
		})
	}
}

func TestDAPAdapterProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "dap-helper" {
		return
	}
	logFile, err := os.OpenFile(os.Args[len(os.Args)-1], os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(2)
	}
	defer logFile.Close()
	reader := bufio.NewReader(os.Stdin)
	launchSeq := 0
	launchCommand := ""
	var options struct {
		Detach string `json:"detach"`
	}
	send := func(response message) {
		if err := writeMessage(os.Stdout, response); err != nil {
			os.Exit(3)
		}
	}
	for {
		request, err := readMessage(reader)
		if err == io.EOF {
			return
		}
		if err != nil {
			os.Exit(4)
		}
		if _, err := fmt.Fprintln(logFile, request.Command); err != nil {
			os.Exit(5)
		}
		response := message{Seq: request.Seq + 100, Type: "response", RequestSeq: request.Seq, Command: request.Command, Success: true, Body: json.RawMessage(`{}`)}
		switch request.Command {
		case "initialize":
			response.Body = json.RawMessage(`{"supportsConfigurationDoneRequest":true}`)
		case "launch", "attach":
			if bytes.Contains(request.Arguments, []byte(`"fail":true`)) {
				response.Success = false
				response.Message = "operation not permitted"
				send(response)
				continue
			}
			if err := json.Unmarshal(request.Arguments, &options); err != nil {
				os.Exit(6)
			}
			launchSeq = request.Seq
			launchCommand = request.Command
			send(message{Type: "event", Event: "initialized"})
			continue
		case "configurationDone":
			send(response)
			send(message{Type: "response", RequestSeq: launchSeq, Command: launchCommand, Success: true, Body: json.RawMessage(`{}`)})
			send(message{Type: "event", Event: "stopped", Body: json.RawMessage(`{"threadId":1}`)})
			output, _ := json.Marshal(struct {
				Output string `json:"output"`
			}{strings.Repeat("x", 40000) + "\n"})
			send(message{Type: "event", Event: "output", Body: output})
			continue
		case "continue", "next", "stepIn", "stepOut", "pause":
			send(message{Type: "event", Event: "stopped", Body: json.RawMessage(`{"threadId":1}`)})
		case "disconnect":
			switch options.Detach {
			case "slow":
				time.Sleep(slowDetachDelay)
			case "on-eof":
				// Never answer; the detach completes when the client connection closes.
				for {
					if _, err := readMessage(reader); err != nil {
						break
					}
				}
			}
			if options.Detach != "" {
				if _, err := fmt.Fprintln(logFile, "detached"); err != nil {
					os.Exit(5)
				}
			}
			if options.Detach != "on-eof" {
				send(response)
			}
			return
		}
		send(response)
	}
}

func TestDAPRealDelve(t *testing.T) {
	if _, err := exec.LookPath("dlv"); err != nil {
		t.Skip("dlv is not installed")
	}
	directory := t.TempDir()
	debugger, err := New(directory, directory, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := debugger.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	process, err := debugger.spawn(ctx, Defaults()["dlv"], false, nil)
	if err != nil {
		t.Fatal(err)
	}
	debugger.active = process
	capabilities, err := process.client.request(ctx, "initialize", json.RawMessage(`{"adapterID":"go","clientID":"tempest","pathFormat":"path","linesStartAt1":true,"columnsStartAt1":true}`))
	if err != nil || !bytes.Contains(capabilities, []byte("supportsConfigurationDoneRequest")) {
		t.Fatalf("Delve initialize: %s, %v", capabilities, err)
	}
	if err := debugger.Terminate(ctx); err != nil {
		t.Fatal(err)
	}
}
