package lsp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeConfig(t *testing.T) {
	t.Run("nil servers select built-in defaults", func(t *testing.T) {
		cfg, err := Normalize(Config{Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"go": "gopls", "yaml": "yaml-language-server --stdio", "terraform": "terraform-ls serve", "helm": "helm_ls serve"}
		if len(cfg.Servers) != 4 || !cfg.Enabled {
			t.Fatalf("defaults: %+v", cfg)
		}
		for _, server := range cfg.Servers {
			if got := strings.Join(append([]string{server.Command}, server.Args...), " "); got != want[server.Name] {
				t.Fatalf("%s = %q, want %q", server.Name, got, want[server.Name])
			}
		}
	})
	t.Run("explicit empty list means no servers", func(t *testing.T) {
		cfg, err := Normalize(Config{Servers: []ServerConfig{}})
		if err != nil || cfg.Servers == nil || len(cfg.Servers) != 0 {
			t.Fatalf("%+v %v", cfg, err)
		}
	})
	t.Run("supplied list replaces defaults and is deep copied", func(t *testing.T) {
		input := Config{Servers: []ServerConfig{{Name: "go", Command: "/opt/gopls", Args: []string{"-rpc.trace"}, Env: map[string]string{"GOFLAGS": "-mod=mod"}, Settings: json.RawMessage(` { "gopls" : { "ui" : 1 } } `)}}}
		cfg, err := Normalize(input)
		if err != nil {
			t.Fatal(err)
		}
		input.Servers[0].Args[0] = "mutated"
		input.Servers[0].Env["GOFLAGS"] = "mutated"
		input.Servers[0].Settings[3] = 'X'
		got := cfg.Servers[0]
		if len(cfg.Servers) != 1 || got.Args[0] != "-rpc.trace" || got.Env["GOFLAGS"] != "-mod=mod" || string(got.Settings) != `{"gopls":{"ui":1}}` {
			t.Fatalf("not an independent normalized copy: %+v", cfg)
		}
	})
	secret := "s3cr3t-value"
	for name, cfg := range map[string]Config{
		"unknown route":         {Servers: []ServerConfig{{Name: "python", Command: "pylsp"}}},
		"duplicate name":        {Servers: []ServerConfig{{Name: "go", Command: "a"}, {Name: "go", Command: "b"}}},
		"empty command":         {Servers: []ServerConfig{{Name: "go"}}},
		"settings array":        {Servers: []ServerConfig{{Name: "go", Command: "gopls", Settings: json.RawMessage(`[1]`)}}},
		"options string":        {Servers: []ServerConfig{{Name: "go", Command: "gopls", InitializationOptions: json.RawMessage(`"x"`)}}},
		"invalid settings JSON": {Servers: []ServerConfig{{Name: "go", Command: "gopls", Settings: json.RawMessage(`{"a":`)}}},
		"settings over 64 KiB":  {Servers: []ServerConfig{{Name: "go", Command: "gopls", Settings: json.RawMessage(`{"a":"` + strings.Repeat("x", 64<<10) + `"}`)}}},
		"env name with =":       {Servers: []ServerConfig{{Name: "go", Command: "gopls", Env: map[string]string{"A=B": secret}}}},
		"env value with NUL":    {Servers: []ServerConfig{{Name: "go", Command: "gopls", Env: map[string]string{"TOKEN": secret + "\x00"}}}},
		"too many servers":      {Servers: []ServerConfig{{Name: "go", Command: "a"}, {Name: "yaml", Command: "a"}, {Name: "helm", Command: "a"}, {Name: "terraform", Command: "a"}, {Name: "go", Command: "a"}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Normalize(cfg)
			wantCode(t, err, CodeConfig)
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("config error leaks env value: %v", err)
			}
		})
	}
}

func TestSettingsSection(t *testing.T) {
	settings := json.RawMessage(`{"gopls":{"ui":{"semanticTokens":true}},"yaml.schemas":{"x":"y"}}`)
	for section, want := range map[string]string{
		"":                     string(settings),
		"gopls":                `{"ui":{"semanticTokens":true}}`,
		"gopls.ui":             `{"semanticTokens":true}`,
		"gopls.ui.missing":     "null",
		"other":                "null",
		"gopls..ui":            "null",
		"gopls.ui.semanticTok": "null",
	} {
		if got := string(settingsSection(settings, section)); got != want {
			t.Errorf("%q = %s, want %s", section, got, want)
		}
	}
	if got := string(settingsSection(nil, "gopls")); got != "null" {
		t.Fatalf("no settings = %s", got)
	}
}

func TestRoutingAndAvailability(t *testing.T) {
	ws := newDirWorkspace(t)
	ws.write(t, "chart/Chart.yaml", "name: c\n")
	ws.write(t, "chart/templates/deploy.yaml", "kind: Deployment\n")
	ws.write(t, "chart/templates/_helpers.tpl", "{{ define \"x\" }}{{ end }}\n")
	ws.write(t, "chart/values.yaml", "a: 1\n")
	ws.write(t, "templates/plain.yaml", "a: 1\n")
	ws.write(t, "stray.tpl", "x\n")
	ws.write(t, "main.tf", "x\n")
	ws.write(t, "vars.tfvars", "x\n")
	ws.write(t, "notes.txt", "x\n")
	m, err := newManager(ws, Config{Enabled: true, Servers: []ServerConfig{{Name: "go", Command: "gopls"}, {Name: "yaml", Command: "y"}, {Name: "helm", Command: "h"}, {Name: "terraform", Command: "t", Disabled: true}}}, nil, testTiming())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	for _, test := range []struct{ path, server, route, language, code string }{
		{"chart/templates/deploy.yaml", "", "helm", "helm", ""},
		{"chart/templates/_helpers.tpl", "", "helm", "helm", ""},
		{"chart/values.yaml", "", "yaml", "yaml", ""},
		{"templates/plain.yaml", "", "yaml", "yaml", ""},
		{"chart/templates/deploy.yaml", "yaml", "yaml", "yaml", ""},
		{"stray.tpl", "", "", "", CodeNoServer},
		{"main.tf", "", "", "", CodeNoServer},
		{"notes.txt", "", "", "", CodeNoServer},
		{"main.tf", "go", "", "", CodeNoServer},
	} {
		sl, language, err := m.routeFor(context.Background(), test.path, test.server)
		if test.code != "" {
			wantCode(t, err, test.code)
			continue
		}
		if err != nil || sl.route.name != test.route || language != test.language {
			t.Fatalf("%s/%s: %v %s %v", test.path, test.server, sl, language, err)
		}
	}
	if r, _ := findRoute("terraform"); r.languageID("vars.tfvars") != "terraform-vars" || r.languageID("main.tf") != "terraform" {
		t.Fatal("terraform language IDs")
	}
	// A missing helm route must not silently treat chart templates as YAML.
	noHelm, err := newManager(ws, Config{Enabled: true, Servers: []ServerConfig{{Name: "yaml", Command: "y"}}}, nil, testTiming())
	if err != nil {
		t.Fatal(err)
	}
	defer noHelm.Close(context.Background())
	if _, _, err = noHelm.routeFor(context.Background(), "chart/templates/deploy.yaml", ""); !strings.Contains(err.Error(), "aren't interpreted as plain YAML") {
		t.Fatalf("helm fallback: %v", err)
	}
	// Routing never reads above the workspace root.
	for _, read := range ws.reads {
		if strings.HasPrefix(read, "..") {
			t.Fatalf("read outside root: %s", read)
		}
	}
}

func TestMissingDisabledAndInvalidQueries(t *testing.T) {
	ws := newDirWorkspace(t)
	ws.write(t, "a.go", "package a\n")
	ws.write(t, "a.yaml", "a: 1\n")
	m, err := newManager(ws, Config{Enabled: true, Servers: []ServerConfig{{Name: "go", Command: "tempest-no-such-gopls", Args: []string{"--token=hidden-arg"}, Env: map[string]string{"SECRET": "hidden-env"}}, {Name: "yaml", Command: "y", Disabled: true}}}, nil, testTiming())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	for range 2 {
		// Missing executables don't trip the cooldown: installing one takes effect at once.
		_, err = m.Query(context.Background(), definitionAt("a.go", 1, 1))
		lspErr := wantCode(t, err, CodeServerMissing)
		if strings.Contains(lspErr.Error(), "hidden") || !strings.Contains(lspErr.Error(), "disable") {
			t.Fatalf("missing-server guidance: %v", lspErr)
		}
	}
	_, err = m.Query(context.Background(), definitionAt("a.yaml", 1, 1))
	wantCode(t, err, CodeNoServer)
	for _, q := range []Query{
		{Method: "hover", Path: "a.go", Position: Position{1, 1}},
		{Method: MethodDefinition, Path: "", Position: Position{1, 1}},
		{Method: MethodDefinition, Path: "a\x00.go", Position: Position{1, 1}},
		{Method: MethodDefinition, Path: "a.go", Server: "python", Position: Position{1, 1}},
		{Method: MethodDefinition, Path: "a.go", Position: Position{1, 1}, MaxResults: 201},
		{Method: MethodRename, Path: "a.go", Position: Position{1, 1}},
		{Method: MethodRename, Path: "a.go", Position: Position{1, 1}, NewName: strings.Repeat("x", 257)},
		{Method: MethodDiagnostics, Path: "a.go", Severity: "fatal"},
		{Method: MethodDiagnostics, Path: "a.go", Wait: 11e9},
	} {
		if _, err = m.Query(context.Background(), q); errorCode(err) != CodeInvalidArgument {
			t.Fatalf("%+v: %v", q, err)
		}
	}
	_, err = m.Query(context.Background(), definitionAt("a.go", 0, 1))
	wantCode(t, err, CodeInvalidPosition)
	_, err = m.Query(context.Background(), definitionAt("a.go", 1, 99))
	wantCode(t, err, CodeInvalidPosition)
	_, err = m.Query(context.Background(), definitionAt("missing.go", 1, 1))
	wantCode(t, err, CodePath)

	disabled, err := newManager(ws, Config{}, nil, testTiming())
	if err != nil {
		t.Fatal(err)
	}
	defer disabled.Close(context.Background())
	_, err = disabled.Query(context.Background(), definitionAt("a.go", 1, 1))
	wantCode(t, err, CodeDisabled)
}
