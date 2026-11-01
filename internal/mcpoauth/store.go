package mcpoauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.harness.dev/harness/internal/auth/credstore"
	"go.harness.dev/harness/internal/mcpconfig"
)

// Record is one server's client registration and tokens. Tokens are bound to
// Resource, the canonical URL of the configured server, and are never sent to
// a server whose configured URL no longer matches.
type Record struct {
	Server   string `json:"server"`
	Resource string `json:"resource"`
	// TokenResource is the RFC 8707 resource from the protected resource
	// metadata, sent in authorization and token requests.
	TokenResource         string `json:"token_resource,omitempty"`
	Issuer                string `json:"issuer,omitempty"`
	AuthorizationEndpoint string `json:"authorization_endpoint,omitempty"`
	TokenEndpoint         string `json:"token_endpoint,omitempty"`
	ClientID              string `json:"client_id,omitempty"`
	ClientSecret          string `json:"client_secret,omitempty"`
	TokenAuthMethod       string `json:"token_endpoint_auth_method,omitempty"`
	// Registered marks a dynamically registered client; RedirectURI is the
	// loopback URI it registered, reused when its port is free.
	Registered   bool      `json:"registered,omitempty"`
	RedirectURI  string    `json:"redirect_uri,omitempty"`
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
	Scope        string    `json:"scope,omitempty"`
	// PendingScope is the scope a step-up challenge asked for while no user
	// could authorize it; the next login requests it.
	PendingScope string `json:"pending_scope,omitempty"`
	// ServerNetwork is the network class (public, loopback, private,
	// link-local) the server answered from at login; refresh may reach it.
	ServerNetwork string `json:"server_network,omitempty"`
}

// Store keeps one owner-only JSON file per server name.
type Store struct{ dir credstore.Dir }

// DefaultDir is the store location inside the agent directory.
func DefaultDir(agentDir string) string { return filepath.Join(agentDir, "mcp-oauth") }

// NewStore opens the store rooted at dir; the directory is created 0700 on
// first write.
func NewStore(dir string) *Store {
	return &Store{dir: credstore.Dir{Path: dir, Label: "MCP OAuth"}}
}

// Load returns the server's record, or an error wrapping os.ErrNotExist.
func (s *Store) Load(server string) (Record, error) {
	if err := checkName(server); err != nil {
		return Record{}, err
	}
	data, err := s.dir.Read(server + ".json")
	if err != nil {
		return Record{}, err
	}
	var r Record
	if json.Unmarshal(data, &r) != nil || r.Server != server {
		return Record{}, fmt.Errorf("invalid MCP OAuth record for %s; run `tempest mcp logout %s` and log in again", server, server)
	}
	return r, nil
}

// Save atomically replaces the server's record at mode 0600.
func (s *Store) Save(r Record) error {
	if err := checkName(r.Server); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.dir.Write(r.Server+".json", data)
}

// Delete removes the server's record.
func (s *Store) Delete(server string) error {
	if err := checkName(server); err != nil {
		return err
	}
	return s.dir.Remove(server + ".json")
}

// Lock serializes token changes for one server across processes.
func (s *Store) Lock(ctx context.Context, server string) (func(), error) {
	if err := checkName(server); err != nil {
		return nil, err
	}
	return s.dir.Lock(ctx, server)
}

// checkName keeps record names to configurable server names, which are
// also safe file names.
func checkName(server string) error {
	if !mcpconfig.ValidName(server) {
		return fmt.Errorf("invalid MCP server name %q", server)
	}
	return nil
}

func missing(err error) bool { return errors.Is(err, os.ErrNotExist) }
