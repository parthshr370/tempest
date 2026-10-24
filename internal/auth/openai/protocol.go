package openai

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

type discovery struct {
	Issuer        string `json:"issuer"`
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
	JWKS          string `json:"jwks_uri"`
	Revocation    string `json:"revocation_endpoint"`
}

// ValidateIssuer permits production and explicit loopback fixtures, not third-party authorities.
func ValidateIssuer(issuer string) error {
	if issuer == Issuer {
		return nil
	}
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("OpenAI issuer must be %s (or an explicit http://127.0.0.1:<port> protocol fixture)", Issuer)
	}
	return nil
}

func (m *Manager) discover(ctx context.Context, issuer string) (discovery, error) {
	if err := ValidateIssuer(issuer); err != nil {
		return discovery{}, err
	}
	var d discovery
	if err := m.getJSON(ctx, issuer+"/.well-known/openid-configuration", &d); err != nil {
		return d, err
	}
	if d.Issuer != issuer {
		return d, fmt.Errorf("OpenAI discovery issuer mismatch")
	}
	origin, _ := url.Parse(issuer)
	for _, endpoint := range []string{d.Authorization, d.Token, d.JWKS, d.Revocation} {
		if endpoint == "" {
			continue
		}
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != origin.Scheme || u.Host != origin.Host || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return d, fmt.Errorf("OpenAI discovery contains an untrusted endpoint")
		}
	}
	if d.Authorization == "" || d.Token == "" || d.JWKS == "" {
		return d, fmt.Errorf("OpenAI discovery is missing required endpoints")
	}
	if issuer == Issuer && (d.Authorization != Issuer+"/api/accounts/authorize" || d.Token != Issuer+"/api/accounts/oauth/token") {
		return d, fmt.Errorf("OpenAI discovery does not match the documented public-client endpoints")
	}
	return d, nil
}

func (m *Manager) getJSON(ctx context.Context, endpoint string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("cannot create OpenAI identity request")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := m.client().Do(req)
	if err != nil {
		return fmt.Errorf("OpenAI identity service unavailable; try tempest login openai again")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("OpenAI identity service returned HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dst); err != nil {
		return fmt.Errorf("invalid OpenAI identity service response")
	}
	return nil
}

type tokenResponse struct {
	Access          string          `json:"access_token"`
	Refresh         string          `json:"refresh_token"`
	ID              string          `json:"id_token"`
	Type            string          `json:"token_type"`
	Scope           string          `json:"scope"`
	Expires         int64           `json:"expires_in"`
	EarliestRefresh json.RawMessage `json:"earliest_refresh_at,omitempty"`
}

type tokenError struct {
	terminal bool
	status   int
	code     string
}

func (e *tokenError) Error() string {
	if e.terminal {
		return "OpenAI sign-in expired or was revoked; run tempest login openai"
	}
	if e.code == "invalid_client" {
		return "OpenAI client registration is invalid; run tempest login openai for this saved profile, or register a new --profile if the client was removed"
	}
	return fmt.Sprintf("OpenAI token request failed (HTTP %d); retry later or run tempest login openai", e.status)
}

func (m *Manager) exchange(ctx context.Context, endpoint string, values url.Values) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("cannot create OpenAI token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := m.client().Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("OpenAI token service unavailable; retry later or run tempest login openai")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var body struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
			return tokenResponse{}, &tokenError{status: resp.StatusCode}
		}
		terminal := slices.Contains([]string{"invalid_grant", "invalid_token", "invalid_refresh_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused"}, body.Error)
		return tokenResponse{}, &tokenError{terminal: terminal, status: resp.StatusCode, code: body.Error}
	}
	var t tokenResponse
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t) != nil {
		return t, fmt.Errorf("invalid OpenAI token response; run tempest login openai")
	}
	if t.Access == "" || !strings.EqualFold(t.Type, "Bearer") || t.Expires <= 0 || t.Expires > int64((1<<63-1)/time.Second) {
		return t, fmt.Errorf("incomplete OpenAI token response; run tempest login openai")
	}
	return t, nil
}

type identity struct {
	Issuer          string          `json:"iss"`
	Subject         string          `json:"sub"`
	Audience        json.RawMessage `json:"aud"`
	AuthorizedParty string          `json:"azp"`
	Email           string          `json:"email"`
	Nonce           string          `json:"nonce"`
	Expires         int64           `json:"exp"`
	Issued          int64           `json:"iat"`
	NotBefore       int64           `json:"nbf"`
}

func (m *Manager) verify(ctx context.Context, d discovery, raw, client, nonce string) (identity, error) {
	invalid := fmt.Errorf("OpenAI ID token validation failed; run tempest login openai")
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return identity{}, invalid
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	h, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(h, &header) != nil || header.Algorithm != "RS256" || header.KeyID == "" {
		return identity{}, invalid
	}
	var keys struct {
		Keys []struct {
			Type      string `json:"kty"`
			ID        string `json:"kid"`
			Algorithm string `json:"alg"`
			Use       string `json:"use"`
			N         string `json:"n"`
			E         string `json:"e"`
		} `json:"keys"`
	}
	if err := m.getJSON(ctx, d.JWKS, &keys); err != nil {
		return identity{}, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return identity{}, invalid
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	verified := false
	for _, key := range keys.Keys {
		if key.ID != header.KeyID || key.Type != "RSA" || key.Algorithm != "" && key.Algorithm != "RS256" || key.Use != "" && key.Use != "sig" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(key.N)
		if err != nil || len(n) < 256 {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(key.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		exponent := new(big.Int).SetBytes(e).Int64()
		if exponent < 3 || exponent > 2147483647 || exponent%2 == 0 {
			continue
		}
		if rsa.VerifyPKCS1v15(&rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exponent)}, crypto.SHA256, digest[:], signature) == nil {
			verified = true
			break
		}
	}
	if !verified {
		return identity{}, invalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	var id identity
	if err != nil || json.Unmarshal(payload, &id) != nil {
		return identity{}, invalid
	}
	var audiences []string
	var audience string
	if json.Unmarshal(id.Audience, &audience) == nil {
		audiences = []string{audience}
	} else if json.Unmarshal(id.Audience, &audiences) != nil {
		return identity{}, invalid
	}
	now := time.Now().Unix()
	if id.Issuer != d.Issuer || id.Subject == "" || !slices.Contains(audiences, client) || len(audiences) > 1 && id.AuthorizedParty != client || id.AuthorizedParty != "" && id.AuthorizedParty != client || id.Expires <= now || id.Issued <= 0 || id.Issued > now+5 || id.NotBefore > now+5 || nonce != "" && id.Nonce != nonce {
		return identity{}, invalid
	}
	return id, nil
}

func randomValue() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func applyTokens(r *record, t tokenResponse, initial bool) error {
	scopes := r.Scopes
	if t.Scope != "" {
		scopes = strings.Fields(t.Scope)
	}
	if initial && (t.Refresh == "" || t.ID == "") {
		return fmt.Errorf("OpenAI sign-in did not return renewable identity credentials; run tempest login openai")
	}
	if !initial && t.Refresh == "" {
		return fmt.Errorf("OpenAI refresh did not return a replacement refresh token; run tempest login openai")
	}
	r.AccessToken = t.Access
	if t.Refresh != "" {
		r.RefreshToken = t.Refresh
	}
	if t.ID != "" {
		r.IDToken = t.ID
	}
	r.Scopes = scopes
	r.ExpiresAt = time.Now().UTC().Add(time.Duration(t.Expires) * time.Second)
	r.EarliestRefreshAt = time.Time{}
	var earliest time.Time
	if len(t.EarliestRefresh) > 0 && string(t.EarliestRefresh) != "null" {
		var seconds int64
		if json.Unmarshal(t.EarliestRefresh, &seconds) == nil {
			earliest = time.Unix(seconds, 0).UTC()
		} else {
			var timestamp string
			if json.Unmarshal(t.EarliestRefresh, &timestamp) != nil {
				return fmt.Errorf("invalid OpenAI earliest refresh time")
			}
			var err error
			earliest, err = time.Parse(time.RFC3339, timestamp)
			if err != nil {
				return fmt.Errorf("invalid OpenAI earliest refresh time")
			}
		}
	}
	r.EarliestRefreshAt = earliest
	return nil
}

func planEnabled(scopes []string) bool {
	return slices.Contains(scopes, "chatgpt.tokens.use.direct") && slices.Contains(scopes, "resource.invoke")
}
