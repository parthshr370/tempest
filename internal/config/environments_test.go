package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoteEnvironmentConfigurationSkipsHostProject(t *testing.T) {
	host := t.TempDir()
	if err := os.Mkdir(filepath.Join(host, ProjectConfigDirName), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, ProjectConfigDirName, "settings.json"), []byte("not valid project JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settings, []byte(`{"environment":"remote","environments":{"remote":{"argv":["ssh","operator-host","tempest","env","-id","remote","-cwd","/srv/project"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	resolved, err := (Config{WorkingDir: host, SettingsPath: settings, LookupEnv: func(string) string { return "" }}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Environment() != "remote" || resolved.Environments()["remote"].Argv[0] != "ssh" {
		t.Fatal("operator environment was not selected")
	}
	processes := resolved.Environments()
	process := processes["remote"]
	process.Argv[0] = "changed"
	if resolved.Environments()["remote"].Argv[0] != "ssh" {
		t.Fatal("runtime environment configuration aliases caller memory")
	}
}

func TestEnvironmentConfigurationRejectsUnregisteredAndShellLaunch(t *testing.T) {
	for _, test := range []struct{ name, environment, command string }{
		{"unregistered", "remote", ""},
		{"shell-text", "remote", "ssh host tempest env"},
		{"empty-argv", "remote", "[]"},
		{"missing-name", "", `["tempest","env"]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := (Config{SettingsPath: filepath.Join(t.TempDir(), "absent"), LookupEnv: func(string) string { return "" }, Flags: FlagValues{Environment: StringInput{Set: true, Value: test.environment}, EnvironmentCommand: StringInput{Set: true, Value: test.command}}}).Resolve()
			if err == nil {
				t.Fatal("invalid environment launch accepted")
			}
		})
	}
}
