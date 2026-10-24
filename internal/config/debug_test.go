package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAskDebugAdapterSettings(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settings, []byte(`{"debug_adapters":{"node":{"command":"node","args":["adapter.js"],"launch":{"type":"pwa-node"}},"debugpy":{"command":"/venv/bin/debugpy-adapter"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	resolved, err := (Config{SettingsPath: settings, LookupEnv: func(string) string { return "" }}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	adapters := resolved.DebugAdapters()
	if adapters["dlv"].Command != "dlv" || adapters["debugpy"].Command != "/venv/bin/debugpy-adapter" || adapters["node"].Command != "node" {
		t.Fatalf("adapters=%+v", adapters)
	}
	if string(adapters["debugpy"].Launch["stopOnEntry"]) != "true" {
		t.Fatal("adapter command override erased the default stop-on-entry setting")
	}
	node := adapters["node"]
	node.Args[0] = "changed"
	node.Launch["type"] = json.RawMessage(`"changed"`)
	if fresh := resolved.DebugAdapters(); fresh["node"].Args[0] != "adapter.js" || string(fresh["node"].Launch["type"]) != `"pwa-node"` {
		t.Fatal("adapter settings share mutable state with the caller")
	}
	if err := os.WriteFile(settings, []byte(`{"debug_adapters":{"bad":{"command":"adapter","transport":"invalid"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Config{SettingsPath: settings, LookupEnv: func(string) string { return "" }}).Resolve(); err == nil {
		t.Fatal("invalid transport accepted")
	}
}
