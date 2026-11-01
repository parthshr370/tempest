// Package openai manages the documented public-client Sign in with ChatGPT flow.
package openai

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.harness.dev/harness/internal/auth/credstore"
)

// Issuer is the production OpenAI OIDC authority.
const Issuer = "https://auth.openai.com"

// Resource is the audience for direct ChatGPT plan usage.
const Resource = "https://api.openai.com/v1"

// Scopes combines identity, renewable access and direct plan permission.
const Scopes = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"

// Manager keeps token storage and refresh ownership outside provider configuration.
type Manager struct {
	Dir    string
	Issuer string
	Client *http.Client
}

type record struct {
	Profile           string    `json:"profile"`
	Issuer            string    `json:"issuer"`
	Subject           string    `json:"subject"`
	Email             string    `json:"email,omitempty"`
	ClientID          string    `json:"client_id"`
	HostID            string    `json:"ext_agent_host_id"`
	IDToken           string    `json:"id_token,omitempty"`
	AccessToken       string    `json:"access_token,omitempty"`
	RefreshToken      string    `json:"refresh_token,omitempty"`
	Scopes            []string  `json:"scopes,omitempty"`
	ExpiresAt         time.Time `json:"expires_at"`
	EarliestRefreshAt time.Time `json:"earliest_refresh_at,omitempty"`
	RefreshPending    bool      `json:"refresh_pending,omitempty"`
}

// Status contains display metadata only, never tokens.
type Status struct {
	Profile     string
	Account     string
	ExpiresAt   time.Time
	SignedIn    bool
	PlanEnabled bool
}

// Default uses an app-owned user configuration directory, never another tool's store.
func Default() (*Manager, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		var err error
		dir, err = os.UserConfigDir()
		if err != nil {
			return nil, err
		}
	}
	return &Manager{Dir: filepath.Join(dir, "tempest", "openai")}, nil
}

func (m *Manager) issuer() string {
	if m.Issuer != "" {
		return strings.TrimRight(m.Issuer, "/")
	}
	return Issuer
}

func (m *Manager) client() *http.Client {
	var c http.Client
	if m.Client != nil {
		c = *m.Client
	}
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	// Credentials must never follow a redirect to another endpoint.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

var profileName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func (m *Manager) profile(profile string) (string, error) {
	if profile == "" {
		data, err := os.ReadFile(filepath.Join(m.Dir, "active"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		profile = strings.TrimSpace(string(data))
		if profile == "" {
			profile = "default"
		}
	}
	if !profileName.MatchString(profile) {
		return "", fmt.Errorf("invalid OpenAI profile: use letters, digits, hyphens or underscores")
	}
	return profile, nil
}

// Profile resolves the active label once so a running session cannot switch accounts.
func (m *Manager) Profile(profile string) (string, error) { return m.profile(profile) }

func (m *Manager) dir() credstore.Dir { return credstore.Dir{Path: m.Dir, Label: "OpenAI"} }

func (m *Manager) read(profile string) (record, error) {
	data, err := m.dir().Read(profile + ".json")
	if err != nil {
		return record{}, err
	}
	var r record
	if json.Unmarshal(data, &r) != nil || r.Profile != profile || r.ClientID == "" || r.ClientID == "dynamic_agent_client" || r.Subject == "" {
		return record{}, fmt.Errorf("invalid OpenAI credential record; run tempest login openai")
	}
	return r, nil
}

func (m *Manager) save(r record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return m.atomic(r.Profile+".json", data)
}

func (m *Manager) atomic(name string, data []byte) error { return m.dir().Write(name, data) }

func (m *Manager) lock(ctx context.Context, name string) (func(), error) {
	return m.dir().Lock(ctx, name)
}

func (m *Manager) host(ctx context.Context) (string, error) {
	unlock, err := m.lock(ctx, "host")
	if err != nil {
		return "", err
	}
	defer unlock()
	data, err := os.ReadFile(filepath.Join(m.Dir, "host"))
	if err == nil {
		id := strings.TrimSpace(string(data))
		if !strings.HasPrefix(id, "urn:uuid:") {
			return "", fmt.Errorf("invalid OpenAI host identity file")
		}
		return id, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	id := fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	return id, m.atomic("host", []byte(id))
}

// Status reads local metadata without refreshing or contacting the provider.
func (m *Manager) Status(profile string) (Status, error) {
	profile, err := m.profile(profile)
	if err != nil {
		return Status{}, err
	}
	r, err := m.read(profile)
	if errors.Is(err, os.ErrNotExist) {
		return Status{Profile: profile}, nil
	}
	if err != nil {
		return Status{}, err
	}
	account := r.Email
	if account == "" {
		account = r.Subject
	}
	return Status{Profile: profile, Account: account, ExpiresAt: r.ExpiresAt, SignedIn: r.AccessToken != "" && r.RefreshToken != "", PlanEnabled: planEnabled(r.Scopes)}, nil
}

// List returns safe metadata for the separate account registrations.
func (m *Manager) List() ([]Status, error) {
	entries, err := os.ReadDir(m.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []Status
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		status, err := m.Status(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		result = append(result, status)
	}
	return result, nil
}
