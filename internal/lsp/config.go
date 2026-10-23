package lsp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"unicode/utf8"
)

const (
	maxConfigBytes   = 1 << 20
	maxRawSetting    = 64 << 10
	maxCommandBytes  = 4096
	maxArgs          = 256
	maxEnvEntries    = 256
	maxConfigServers = 4
	// maxEchoedName bounds names and kinds echoed back from config or servers.
	maxEchoedName = 64
)

// route is one of the four built-in language routes.
type route struct {
	name    string
	command string
	args    []string
	exts    []string
}

var routes = []route{
	{name: "go", command: "gopls", exts: []string{".go"}},
	{name: "yaml", command: "yaml-language-server", args: []string{"--stdio"}, exts: []string{".yaml", ".yml"}},
	{name: "terraform", command: "terraform-ls", args: []string{"serve"}, exts: []string{".tf", ".tfvars"}},
	{name: "helm", command: "helm_ls", args: []string{"serve"}, exts: []string{".yaml", ".yml", ".tpl"}},
}

func findRoute(name string) (route, bool) {
	for _, r := range routes {
		if r.name == name {
			return r, true
		}
	}
	return route{}, false
}

// handles reports whether the route accepts files with name's extension.
func (r route) handles(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	for _, e := range r.exts {
		if e == ext {
			return true
		}
	}
	return false
}

// languageID is the LSP languageId this route sends for name.
func (r route) languageID(name string) string {
	if r.name == "terraform" && strings.EqualFold(path.Ext(name), ".tfvars") {
		return "terraform-vars"
	}
	return r.name
}

func defaultServers() []ServerConfig {
	servers := make([]ServerConfig, len(routes))
	for i, r := range routes {
		servers[i] = ServerConfig{Name: r.name, Command: r.command, Args: append([]string(nil), r.args...)}
	}
	return servers
}

// Normalize validates cfg and returns a deep copy. Nil Servers selects the
// built-in definitions; an explicit empty list means no servers. A supplied
// list replaces the defaults entirely.
func Normalize(cfg Config) (Config, error) {
	out := Config{Enabled: cfg.Enabled}
	if cfg.Servers == nil {
		out.Servers = defaultServers()
		return out, nil
	}
	if len(cfg.Servers) > maxConfigServers {
		return Config{}, newError(CodeConfig, fmt.Sprintf("at most %d servers may be configured", maxConfigServers))
	}
	out.Servers = make([]ServerConfig, 0, len(cfg.Servers))
	seen := make(map[string]bool, len(cfg.Servers))
	for i, server := range cfg.Servers {
		where := fmt.Sprintf("servers[%d]", i)
		if _, ok := findRoute(server.Name); !ok {
			return Config{}, newError(CodeConfig, fmt.Sprintf("%s: unknown server name %q; supported: go, yaml, terraform, helm", where, boundText(server.Name, maxEchoedName)))
		}
		where = fmt.Sprintf("server %s", server.Name)
		if seen[server.Name] {
			return Config{}, newError(CodeConfig, where+": duplicate name")
		}
		seen[server.Name] = true
		if server.Command == "" || len(server.Command) > maxCommandBytes || strings.ContainsRune(server.Command, 0) {
			return Config{}, newError(CodeConfig, where+": command must be a nonempty executable name or path")
		}
		if len(server.Args) > maxArgs {
			return Config{}, newError(CodeConfig, fmt.Sprintf("%s: more than %d args", where, maxArgs))
		}
		normalized := ServerConfig{Name: server.Name, Command: server.Command, Disabled: server.Disabled}
		if server.Args != nil {
			normalized.Args = make([]string, len(server.Args))
			for j, arg := range server.Args {
				if strings.ContainsRune(arg, 0) || len(arg) > maxCommandBytes {
					return Config{}, newError(CodeConfig, fmt.Sprintf("%s: args[%d] is invalid", where, j))
				}
				normalized.Args[j] = arg
			}
		}
		if len(server.Env) > maxEnvEntries {
			return Config{}, newError(CodeConfig, fmt.Sprintf("%s: more than %d env entries", where, maxEnvEntries))
		}
		if server.Env != nil {
			normalized.Env = make(map[string]string, len(server.Env))
			for key, value := range server.Env {
				if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) {
					// Never echo env values: they commonly hold credentials.
					return Config{}, newError(CodeConfig, where+": env has an invalid name or value")
				}
				normalized.Env[key] = value
			}
		}
		var err error
		if normalized.InitializationOptions, err = normalizeRawObject(server.InitializationOptions); err != nil {
			return Config{}, newError(CodeConfig, where+": initialization_options "+err.Error())
		}
		if normalized.Settings, err = normalizeRawObject(server.Settings); err != nil {
			return Config{}, newError(CodeConfig, where+": settings "+err.Error())
		}
		out.Servers = append(out.Servers, normalized)
	}
	encoded, err := json.Marshal(out)
	if err != nil || len(encoded) > maxConfigBytes {
		return Config{}, newError(CodeConfig, "configuration exceeds 1 MiB")
	}
	return out, nil
}

// normalizeRawObject returns a compact copy of a JSON object, nil for absent
// or null.
func normalizeRawObject(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if len(trimmed) > maxRawSetting {
		return nil, fmt.Errorf("exceeds 64 KiB")
	}
	if trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, fmt.Errorf("must be a JSON object")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err != nil {
		return nil, fmt.Errorf("must be a JSON object")
	}
	return compact.Bytes(), nil
}

// serverEnv is the MCP stdio allowlist plus explicit overrides. Provider and
// cloud credentials are not inherited unless the operator passes them.
func serverEnv(overrides map[string]string) []string {
	allowed := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "TMP": true, "TEMP": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true, "SYSTEMROOT": true, "WINDIR": true, "USERPROFILE": true}
	environment := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok && (allowed[key] || strings.HasPrefix(key, "LC_")) {
			environment[key] = value
		}
	}
	for key, value := range overrides {
		environment[key] = value
	}
	env := make([]string, 0, len(environment))
	for key, value := range environment {
		env = append(env, key+"="+value)
	}
	return env
}

// settingsSection answers one workspace/configuration item from this
// server's own Settings: the whole object for an empty section, otherwise the
// value at the dotted path, or null.
func settingsSection(settings json.RawMessage, section string) json.RawMessage {
	null := json.RawMessage("null")
	if len(settings) == 0 {
		return null
	}
	if section == "" {
		return settings
	}
	current := settings
	for _, key := range strings.Split(section, ".") {
		var object map[string]json.RawMessage
		if key == "" || json.Unmarshal(current, &object) != nil {
			return null
		}
		next, ok := object[key]
		if !ok {
			return null
		}
		current = next
	}
	return current
}

// boundText returns at most limit bytes of s, cut on a rune boundary.
func boundText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
