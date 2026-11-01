// Package mcpconfig validates runtime-only MCP server settings.
package mcpconfig

import (
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Server is one operator-selected MCP endpoint. Env, Headers and the OAuth
// client secret may contain secrets.
type Server struct {
	Name           string            `json:"name"`
	Transport      string            `json:"transport"`
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	URL            string            `json:"url,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	StartupTimeout string            `json:"startupTimeout,omitempty"`
	CallTimeout    string            `json:"callTimeout,omitempty"`
	Permission     string            `json:"permission,omitempty"`
	// Auth "oauth" lets login discover the authorization server through
	// well-known metadata when the server sends no 401 challenge. HTTP
	// servers without an Authorization header answer a Bearer challenge with
	// OAuth either way.
	Auth  string       `json:"auth,omitempty"`
	OAuth *OAuthClient `json:"oauth,omitempty"`
	// Sampling declares the sampling capability so the server may request
	// model completions; each first use is still approved by the user.
	Sampling bool `json:"sampling,omitempty"`
}

// OAuthClient is a pre-registered OAuth client. Without ClientID the client
// registers itself dynamically (RFC 7591).
type OAuthClient struct {
	ClientID     string   `json:"clientId,omitempty"`
	ClientSecret string   `json:"clientSecret,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
}

// UsesOAuth reports whether an HTTP server's 401 challenge is answered with
// OAuth: a configured Authorization header means static credentials instead.
func (s Server) UsesOAuth() bool {
	if s.Transport != "http" {
		return false
	}
	for name := range s.Headers {
		if strings.EqualFold(name, "Authorization") {
			return false
		}
	}
	return true
}

var serverName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,31}$`)
var headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// ValidName reports whether name is a usable server name; it is also safe
// as a file name.
func ValidName(name string) bool {
	return serverName.MatchString(name) && !strings.Contains(name, "__")
}

// Validate rejects unsafe or ambiguous configurations before starting a server.
func Validate(servers []Server) error { _, err := Normalize(servers); return err }

// Normalize validates and deep-copies settings, making timeouts and policy explicit.
func Normalize(servers []Server) ([]Server, error) {
	if len(servers) > 32 {
		return nil, fmt.Errorf("mcp_config: at most 32 servers")
	}
	if servers == nil {
		return nil, nil
	}
	out := make([]Server, len(servers))
	seen := map[string]bool{}
	for i, s := range servers {
		fail := func(reason string) ([]Server, error) {
			return nil, fmt.Errorf("mcp_config: server %q: %s", s.Name, reason)
		}
		if !serverName.MatchString(s.Name) || strings.Contains(s.Name, "__") {
			return fail(`invalid name: use 1-32 letters, digits, "_" or "-", starting with a letter, without "__"`)
		}
		if seen[s.Name] {
			return fail("duplicate name")
		}
		seen[s.Name] = true
		if s.StartupTimeout == "" {
			s.StartupTimeout = "30s"
		}
		if s.CallTimeout == "" {
			s.CallTimeout = "2m"
		}
		for _, value := range []*string{&s.StartupTimeout, &s.CallTimeout} {
			duration, err := time.ParseDuration(*value)
			if err != nil || duration <= 0 || duration > 24*time.Hour {
				return fail("timeout must be greater than zero and at most 24h")
			}
			*value = duration.String()
		}
		if s.Permission == "" {
			s.Permission = "prompt"
		}
		if s.Permission != "prompt" && s.Permission != "allow" && s.Permission != "deny" {
			return fail(`invalid permission: use "prompt", "allow" or "deny"`)
		}
		switch s.Transport {
		case "stdio":
			if strings.TrimSpace(s.Command) == "" || strings.ContainsRune(s.Command, 0) || s.URL != "" || len(s.Headers) > 0 {
				return fail("stdio requires command and forbids URL/headers")
			}
			for _, a := range s.Args {
				if strings.ContainsRune(a, 0) {
					return fail("invalid argument")
				}
			}
			for k, v := range s.Env {
				if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
					return fail("invalid environment")
				}
			}
		case "http":
			if s.Command != "" || len(s.Args) > 0 || len(s.Env) > 0 {
				return fail("http forbids command, args and env")
			}
			u, e := url.Parse(s.URL)
			if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
				return fail("invalid HTTP URL")
			}
			ip := net.ParseIP(u.Hostname())
			if u.Scheme != "https" && !strings.EqualFold(u.Hostname(), "localhost") && (ip == nil || !ip.IsLoopback()) {
				return fail("HTTPS required outside loopback")
			}
			owned := map[string]bool{"host": true, "content-length": true, "content-type": true, "accept": true, "mcp-protocol-version": true, "mcp-session-id": true, "last-event-id": true}
			keys := map[string]bool{}
			for k, v := range s.Headers {
				lower := strings.ToLower(k)
				if !headerName.MatchString(k) || owned[lower] || keys[lower] {
					return fail("invalid, duplicate or transport-owned header")
				}
				keys[lower] = true
				for _, b := range []byte(v) {
					if (b < 32 && b != '\t') || b == 127 {
						return fail("invalid header value")
					}
				}
				if http.CanonicalHeaderKey(k) == "" {
					return fail("invalid header name")
				}
			}
		default:
			return fail(`unknown transport: use "stdio" (with command) or "http" (with url)`)
		}
		if s.Auth != "" && s.Auth != "oauth" {
			return fail(`invalid auth: use "oauth" or omit it`)
		}
		if (s.Auth != "" || s.OAuth != nil) && !s.UsesOAuth() {
			return fail("auth and oauth need an HTTP server without a configured Authorization header")
		}
		if s.OAuth != nil {
			client := *s.OAuth
			if client.ClientSecret != "" && client.ClientID == "" {
				return fail("oauth.clientSecret needs oauth.clientId")
			}
			for _, value := range []string{client.ClientID, client.ClientSecret} {
				if strings.ContainsAny(value, "\x00\r\n") {
					return fail("invalid oauth client value")
				}
			}
			for _, scope := range client.Scopes {
				if scope == "" || strings.ContainsAny(scope, " \t\r\n\"\\") {
					return fail("oauth.scopes entries must be single scope tokens")
				}
			}
			client.Scopes = append([]string(nil), client.Scopes...)
			s.OAuth = &client
		}
		s.Args = append([]string(nil), s.Args...)
		s.Env = maps.Clone(s.Env)
		if s.Headers != nil {
			headers := make(map[string]string, len(s.Headers))
			for name, value := range s.Headers {
				headers[http.CanonicalHeaderKey(name)] = value
			}
			s.Headers = headers
		}
		out[i] = s
	}
	return out, nil
}
