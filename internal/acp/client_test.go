package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestOpenRejectsCwdThatIsNotAnExistingDirectory(t *testing.T) {
	client := NewClient(nil, nil, nil)
	if _, e := client.Handle(t.Context(), "initialize", json.RawMessage(`{"protocolVersion":1}`)); e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, cwd := range map[string]string{"missing": filepath.Join(t.TempDir(), "does-not-exist"), "file": file} {
		for _, method := range []string{"session/new", "session/load", "session/resume"} {
			t.Run(name+"/"+method, func(t *testing.T) {
				_, e := client.Handle(t.Context(), method, json.RawMessage(`{"sessionId":"s","cwd":`+strconv.Quote(cwd)+`}`))
				if e == nil || e.Code != -32602 || !strings.Contains(e.Message, cwd) {
					t.Fatalf("error = %+v", e)
				}
			})
		}
	}
}
