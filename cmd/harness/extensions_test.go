package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestExtensionConfigRequiresExplicitValidLaunch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions.json")
	for _, raw := range []string{`[{"ID":"peer","Argv":["python3","peer.py"],"Capabilities":["read"]}]`, `[{"ID":"peer","Argv":["peer"],"unknown":true}]`, `[{"ID":"peer","Argv":[]}]`, `[{"ID":"peer","Argv":["a"]},{"ID":"peer","Argv":["b"]}]`, `[] []`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		processes, err := loadExtensionProcesses(path, io.Discard)
		if raw == `[{"ID":"peer","Argv":["python3","peer.py"],"Capabilities":["read"]}]` {
			if err != nil || len(processes) != 1 || processes[0].ID != "peer" || processes[0].Stderr != io.Discard {
				t.Fatalf("processes=%+v err=%v", processes, err)
			}
		} else if err == nil {
			t.Fatalf("invalid config accepted: %s", raw)
		}
	}
}
