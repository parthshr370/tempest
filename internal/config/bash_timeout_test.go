package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBashTimeoutPrecedence(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settings, []byte(`{"bash_timeout":"3m"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		path string
		env  string
		flag StringInput
		want time.Duration
	}{
		{name: "default", path: filepath.Join(t.TempDir(), "missing.json"), want: 10 * time.Minute},
		{name: "settings", path: settings, want: 3 * time.Minute},
		{name: "environment", path: settings, env: "2m", want: 2 * time.Minute},
		{name: "flag", path: settings, env: "invalid", flag: StringInput{Value: "1s", Set: true}, want: time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolved, err := (Config{
				SettingsPath: test.path,
				Flags:        FlagValues{BashTimeout: test.flag},
				LookupEnv: func(key string) string {
					if key == "HARNESS_BASH_TIMEOUT" {
						return test.env
					}
					return ""
				},
			}).Resolve()
			if err != nil {
				t.Fatal(err)
			}
			if got := resolved.BashTimeout(); got != test.want {
				t.Fatalf("BashTimeout = %s, want %s", got, test.want)
			}
		})
	}
}

func TestBashTimeoutValidation(t *testing.T) {
	for _, value := range []string{"invalid", "0s", "-1s", "2147484s"} {
		t.Run(value, func(t *testing.T) {
			_, err := (Config{
				SettingsPath: filepath.Join(t.TempDir(), "missing.json"),
				Flags:        FlagValues{BashTimeout: StringInput{Value: value, Set: true}},
				LookupEnv:    func(string) string { return "" },
			}).Resolve()
			if err == nil || !strings.Contains(err.Error(), "bash_timeout") {
				t.Fatalf("invalid timeout %q: %v", value, err)
			}
		})
	}
}
