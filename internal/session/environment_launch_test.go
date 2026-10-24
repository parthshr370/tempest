package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/tempest/environment"
)

func TestConfiguredEnvironmentRoutesBoundedDaemonStderr(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	directory := t.TempDir()
	marker := "daemon-stderr-marker:"
	script := "printf '%s' '" + marker + strings.Repeat("x", 80<<10) + "' >&2; exit 1"
	settings, err := json.Marshal(map[string]any{"environments": map[string]any{"diagnostics": map[string]any{"argv": []string{"/bin/sh", "-c", script}}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "settings.json")
	if err := os.WriteFile(path, settings, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := (config.Config{SettingsPath: path, SkipProject: true, LookupEnv: func(string) string { return "" }}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	capture, err := os.Create(filepath.Join(directory, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	previous := os.Stderr
	os.Stderr = capture
	defer func() { os.Stderr = previous }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	lease, err := configuredEnvironmentResolver(cfg)(ctx, environment.Reference{ID: "diagnostics", Cwd: "."})
	os.Stderr = previous
	if err == nil {
		if lease != nil {
			_ = lease.Close()
		}
		t.Fatal("daemon exit must fail its handshake")
	}
	output, err := os.ReadFile(capture.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(output) != 64<<10 || !strings.HasPrefix(string(output), marker) {
		t.Fatalf("daemon diagnostics: bytes=%d prefix=%q", len(output), string(output[:min(len(output), len(marker))]))
	}
}
