package dap

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDAPRealDebugpy(t *testing.T) {
	adapter := os.Getenv("TEMPEST_TEST_DEBUGPY_ADAPTER")
	if adapter == "" {
		var err error
		adapter, err = exec.LookPath("debugpy-adapter")
		if err != nil {
			t.Skip("debugpy-adapter is not installed")
		}
	}
	directory := t.TempDir()
	program := filepath.Join(directory, "target.py")
	if err := os.WriteFile(program, []byte("value = 40\nvalue += 2\nprint(value)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	debugger, err := New(directory, directory, map[string]Adapter{"debugpy": {Command: adapter, Launch: map[string]json.RawMessage{"stopOnEntry": json.RawMessage(`true`), "justMyCode": json.RawMessage(`false`)}}}, nil)
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
	encodedProgram, err := json.Marshal(program)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := debugger.Start(ctx, "launch", "debugpy", program, map[string]json.RawMessage{"program": encodedProgram, "console": json.RawMessage(`"internalConsole"`)}); err != nil {
		t.Fatal(err)
	}
	breakpoints, err := json.Marshal(struct {
		Source struct {
			Path string `json:"path"`
		} `json:"source"`
		Breakpoints []struct {
			Line int `json:"line"`
		} `json:"breakpoints"`
	}{struct {
		Path string `json:"path"`
	}{program}, []struct {
		Line int `json:"line"`
	}{{3}}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := debugger.Request(ctx, "setBreakpoints", breakpoints)
	var points struct {
		Breakpoints []struct {
			Verified bool `json:"verified"`
		} `json:"breakpoints"`
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(response, &points); err != nil || len(points.Breakpoints) != 1 || !points.Breakpoints[0].Verified {
		t.Fatalf("breakpoints=%s err=%v", response, err)
	}
	threadArguments := func() json.RawMessage {
		encoded, err := json.Marshal(struct {
			ThreadID int `json:"threadId"`
		}{debugger.Thread()})
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	if response, err := debugger.Request(ctx, "continue", threadArguments()); err != nil || !bytes.Contains(response, []byte(`"state":"stopped"`)) {
		t.Fatalf("continue=%s err=%v", response, err)
	}
	stack, err := debugger.Request(ctx, "stackTrace", threadArguments())
	if err != nil {
		t.Fatal(err)
	}
	var frames struct {
		Frames []struct {
			ID   int `json:"id"`
			Line int `json:"line"`
		} `json:"stackFrames"`
	}
	if err := json.Unmarshal(stack, &frames); err != nil {
		t.Fatal(err)
	}
	if len(frames.Frames) == 0 || frames.Frames[0].Line != 3 {
		t.Fatalf("stack=%s", stack)
	}
	frame := frames.Frames[0].ID
	frameArguments, err := json.Marshal(struct {
		FrameID int `json:"frameId"`
	}{frame})
	if err != nil {
		t.Fatal(err)
	}
	scopes, err := debugger.Request(ctx, "scopes", frameArguments)
	if err != nil {
		t.Fatal(err)
	}
	var scopeList struct {
		Scopes []struct {
			Reference int `json:"variablesReference"`
		} `json:"scopes"`
	}
	if err := json.Unmarshal(scopes, &scopeList); err != nil || len(scopeList.Scopes) == 0 {
		t.Fatalf("scopes=%s err=%v", scopes, err)
	}
	reference, err := json.Marshal(struct {
		Reference int `json:"variablesReference"`
	}{scopeList.Scopes[0].Reference})
	if err != nil {
		t.Fatal(err)
	}
	variables, err := debugger.Request(ctx, "variables", reference)
	if err != nil {
		t.Fatal(err)
	}
	var locals struct {
		Variables []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"variables"`
	}
	if err := json.Unmarshal(variables, &locals); err != nil {
		t.Fatal(err)
	}
	foundValue := false
	for _, variable := range locals.Variables {
		foundValue = foundValue || (variable.Name == "value" && variable.Value == "42")
	}
	if !foundValue {
		t.Fatalf("variables=%s", variables)
	}
	evaluation, err := json.Marshal(struct {
		Expression string `json:"expression"`
		FrameID    int    `json:"frameId"`
		Context    string `json:"context"`
	}{"value + 1", frame, "watch"})
	if err != nil {
		t.Fatal(err)
	}
	value, err := debugger.Request(ctx, "evaluate", evaluation)
	if err != nil {
		t.Fatal(err)
	}
	var evaluated struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(value, &evaluated); err != nil || evaluated.Result != "43" {
		t.Fatalf("evaluate=%s err=%v", value, err)
	}
	if _, err := debugger.Request(ctx, "continue", threadArguments()); err != nil {
		t.Fatal(err)
	}
	if err := debugger.Terminate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("debugpy target: breakpoint line 3, stack, scopes, value=42, evaluate=43, continued and terminated")
}
