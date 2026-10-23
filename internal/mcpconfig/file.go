package mcpconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// fileShape is appended to shape errors so an operator sees both accepted layouts.
const fileShape = `expected {"mcpServers": {"<name>": {"command": "...", "args": [...], "env": {...}}}} or {"mcpServers": [{"name": "<name>", "transport": "stdio", "command": "..."}]}; an entry with "url" (and optional "headers") is an HTTP server`

// fileEntry is one server as written in an operator file. It accepts the
// Claude Desktop/Cursor spelling ("type", transport inferred from
// command/url) beside Tempest's own fields, then becomes a Server.
type fileEntry struct {
	Name           string            `json:"name"`
	Transport      string            `json:"transport"`
	Type           string            `json:"type"`
	Command        string            `json:"command"`
	Args           []string          `json:"args"`
	Env            map[string]string `json:"env"`
	URL            string            `json:"url"`
	Headers        map[string]string `json:"headers"`
	StartupTimeout string            `json:"startupTimeout"`
	CallTimeout    string            `json:"callTimeout"`
	Permission     string            `json:"permission"`
	Disabled       bool              `json:"disabled"`
}

// ParseFile decodes an operator-selected MCP file. mcpServers may be Tempest's
// array of named servers or the Claude Desktop/Cursor object keyed by server
// name. Entries with "disabled": true are skipped. Transport comes from
// "transport" or "type" when present, otherwise command means stdio and url
// means http. The result still needs Normalize.
func ParseFile(data []byte) ([]Server, error) {
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&top); err != nil {
		return nil, fmt.Errorf("invalid JSON (%v); %s", describeDecodeError(err), fileShape)
	}
	var extra json.RawMessage
	if dec.Decode(&extra) != io.EOF {
		return nil, errors.New("trailing JSON after the top-level object")
	}
	if top == nil {
		return nil, fmt.Errorf("top level must be an object; %s", fileShape)
	}
	for key := range top {
		if key != "mcpServers" {
			return nil, fmt.Errorf("unknown top-level field %q; %s", key, fileShape)
		}
	}
	raw, ok := top["mcpServers"]
	if !ok {
		return nil, fmt.Errorf(`missing "mcpServers"; %s`, fileShape)
	}
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) > 0 && raw[0] == '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("mcpServers: %v", describeDecodeError(err))
		}
		servers := make([]Server, 0, len(items))
		for i, item := range items {
			entry, err := decodeEntry(item)
			if err != nil {
				return nil, fmt.Errorf("mcpServers[%d]: %w", i, err)
			}
			if entry.Name == "" {
				return nil, fmt.Errorf(`mcpServers[%d]: missing "name" (or use the object form keyed by server name)`, i)
			}
			if server, keep, err := entry.server(); err != nil {
				return nil, fmt.Errorf("server %q: %w", entry.Name, err)
			} else if keep {
				servers = append(servers, server)
			}
		}
		return servers, nil
	case len(raw) > 0 && raw[0] == '{':
		return parseServerObject(raw)
	default:
		return nil, fmt.Errorf("mcpServers must be an object keyed by server name or an array of servers; %s", fileShape)
	}
}

// parseServerObject keeps the file's key order so startup and errors are deterministic.
func parseServerObject(raw json.RawMessage) ([]Server, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("mcpServers: %v", describeDecodeError(err))
	}
	servers := []Server{}
	seen := map[string]bool{}
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("mcpServers: %v", describeDecodeError(err))
		}
		name, _ := token.(string)
		var item json.RawMessage
		if err := dec.Decode(&item); err != nil {
			return nil, fmt.Errorf("server %q: %v", name, describeDecodeError(err))
		}
		if seen[name] {
			return nil, fmt.Errorf("server %q: duplicate name", name)
		}
		seen[name] = true
		entry, err := decodeEntry(item)
		if err != nil {
			return nil, fmt.Errorf("server %q: %w", name, err)
		}
		if entry.Name != "" && entry.Name != name {
			return nil, fmt.Errorf("server %q: \"name\" %q differs from its key; omit \"name\" in the object form", name, entry.Name)
		}
		entry.Name = name
		server, keep, err := entry.server()
		if err != nil {
			return nil, fmt.Errorf("server %q: %w", name, err)
		}
		if keep {
			servers = append(servers, server)
		}
	}
	return servers, nil
}

func decodeEntry(raw json.RawMessage) (fileEntry, error) {
	var entry fileEntry
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return entry, errors.New("each server must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entry); err != nil {
		return entry, errors.New(describeDecodeError(err))
	}
	return entry, nil
}

// server resolves the transport and drops disabled entries.
func (e fileEntry) server() (Server, bool, error) {
	if e.Disabled {
		return Server{}, false, nil
	}
	transport := e.Transport
	if e.Type != "" {
		if transport != "" && !strings.EqualFold(transport, e.Type) {
			return Server{}, false, fmt.Errorf("\"type\" %q conflicts with \"transport\" %q", e.Type, transport)
		}
		transport = e.Type
	}
	switch strings.ToLower(transport) {
	case "":
		switch {
		case e.Command != "" && e.URL == "":
			transport = "stdio"
		case e.URL != "" && e.Command == "":
			transport = "http"
		default:
			return Server{}, false, errors.New(`set exactly one of "command" (stdio server) or "url" (HTTP server)`)
		}
	case "stdio":
		transport = "stdio"
	case "http", "streamable-http", "streamablehttp", "streamable_http":
		transport = "http"
	case "sse":
		return Server{}, false, errors.New(`legacy HTTP+SSE servers are not supported; point "url" at the server's Streamable HTTP endpoint`)
	default:
		return Server{}, false, fmt.Errorf("unknown transport %q (use stdio or http)", transport)
	}
	return Server{Name: e.Name, Transport: transport, Command: e.Command, Args: e.Args, Env: e.Env, URL: e.URL, Headers: e.Headers,
		StartupTimeout: e.StartupTimeout, CallTimeout: e.CallTimeout, Permission: e.Permission}, true, nil
}

// describeDecodeError turns encoding/json errors into operator wording without Go type names.
func describeDecodeError(err error) string {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		want := typeErr.Type.String()
		switch typeErr.Type.Kind() {
		case reflect.String:
			want = "a string"
		case reflect.Bool:
			want = "true or false"
		case reflect.Slice:
			want = "an array of strings"
			if typeErr.Type.Elem().Kind() != reflect.String {
				want = "an array"
			}
		case reflect.Map:
			want = "an object of string values"
			if typeErr.Type.Elem().Kind() != reflect.String {
				want = "an object"
			}
		case reflect.Struct:
			want = "an object"
		}
		if typeErr.Field != "" {
			return fmt.Sprintf("field %q must be %s, not %s", typeErr.Field, want, typeErr.Value)
		}
		return fmt.Sprintf("must be %s, not %s", want, typeErr.Value)
	}
	if message := err.Error(); strings.HasPrefix(message, "json: unknown field ") {
		return fmt.Sprintf("unknown field %s (supported: name, command, args, env, url, headers, type, transport, permission, startupTimeout, callTimeout, disabled)", strings.TrimPrefix(message, "json: unknown field "))
	}
	return strings.TrimPrefix(err.Error(), "json: ")
}
