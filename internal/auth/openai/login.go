package openai

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"time"
)

// Login opens a loopback listener before visiting the authorization URL. The
// caller opens the system browser or prints the URL for a browser on this host.
func (m *Manager) Login(ctx context.Context, profile string, consent bool, visit func(string) error) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	profile, err := m.profile(profile)
	if err != nil {
		return Status{}, err
	}
	host, err := m.host(ctx)
	if err != nil {
		return Status{}, err
	}
	unlock, err := m.lock(ctx, profile)
	if err != nil {
		return Status{}, err
	}
	previous, err := m.read(profile)
	unlock()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Status{}, err
	}
	issuer := m.issuer()
	if previous.ClientID != "" && previous.Issuer != issuer {
		return Status{}, fmt.Errorf("OpenAI profile belongs to another issuer; choose a different --profile")
	}
	d, err := m.discover(ctx, issuer)
	if err != nil {
		return Status{}, err
	}
	state, err := randomValue()
	if err != nil {
		return Status{}, err
	}
	nonce, err := randomValue()
	if err != nil {
		return Status{}, err
	}
	verifier, err := randomValue()
	if err != nil {
		return Status{}, err
	}
	challenge := sha256.Sum256([]byte(verifier))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return Status{}, fmt.Errorf("cannot start OpenAI loopback callback: %w", err)
	}
	defer listener.Close()
	redirect := "http://" + listener.Addr().String() + "/auth/callback"
	callback := make(chan url.Values, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(values["state"]) != 1 || subtle.ConstantTimeCompare([]byte(values.Get("state")), []byte(state)) != 1 {
			http.Error(w, "Sign-in state did not match", http.StatusBadRequest)
			return
		}
		for _, key := range []string{"code", "error", "client_id"} {
			if len(values[key]) > 1 {
				http.Error(w, "Invalid sign-in callback", http.StatusBadRequest)
				return
			}
		}
		select {
		case callback <- values:
			fmt.Fprintln(w, "Sign-in callback received. Return to Tempest.")
		default:
			http.Error(w, "Sign-in callback already received", http.StatusConflict)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	clientID := previous.ClientID
	if clientID == "" {
		clientID = "dynamic_agent_client"
	}
	values := url.Values{"client_id": {clientID}, "ext_agent_host_id": {host}, "response_type": {"code"}, "redirect_uri": {redirect}, "scope": {Scopes}, "resource": {Resource}, "state": {state}, "nonce": {nonce}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}}
	if clientID == "dynamic_agent_client" {
		values.Set("agent_name_hint", "Tempest")
	} else if previous.Email != "" {
		values.Set("login_hint", previous.Email)
	}
	if consent {
		values.Set("prompt", "consent")
	}
	// The optional ID-token hint is omitted: authorization URLs may be displayed
	// for manual browser opening and must not contain retained credentials.
	if visit == nil {
		return Status{}, fmt.Errorf("OpenAI login requires a browser URL handler")
	}
	if err := visit(d.Authorization + "?" + values.Encode()); err != nil {
		return Status{}, err
	}
	var result url.Values
	select {
	case result = <-callback:
	case <-ctx.Done():
		return Status{}, fmt.Errorf("OpenAI sign-in cancelled or timed out; run tempest login openai")
	case <-serveErr:
		return Status{}, fmt.Errorf("OpenAI callback listener stopped; run tempest login openai")
	}
	if result.Get("error") != "" {
		return Status{}, fmt.Errorf("OpenAI authorization was denied; run tempest login openai --profile %s --consent to approve ChatGPT plan usage", profile)
	}
	issued := result.Get("client_id")
	if clientID == "dynamic_agent_client" {
		if issued == "" || issued == "dynamic_agent_client" {
			return Status{}, fmt.Errorf("OpenAI registration did not issue a client ID; run tempest login openai")
		}
		clientID = issued
	} else if issued != "" && issued != clientID {
		return Status{}, fmt.Errorf("OpenAI callback changed the selected client ID; sign-in rejected")
	}
	code := result.Get("code")
	if code == "" {
		return Status{}, fmt.Errorf("OpenAI callback did not contain a code; run tempest login openai")
	}
	tokens, err := m.exchange(ctx, d.Token, url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {redirect}, "resource": {Resource}})
	if err != nil {
		return Status{}, err
	}
	id, err := m.verify(ctx, d, tokens.ID, clientID, nonce)
	if err != nil {
		return Status{}, err
	}
	if previous.Subject != "" && previous.Subject != id.Subject {
		return Status{}, fmt.Errorf("OpenAI account differs from the selected registration; use tempest login openai --profile <new-label>")
	}
	r := record{Profile: profile, Issuer: issuer, ClientID: clientID, HostID: host, Subject: id.Subject, Email: id.Email}
	if err := applyTokens(&r, tokens, true); err != nil {
		return Status{}, err
	}
	unlock, err = m.lock(ctx, profile)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	current, err := m.read(profile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Status{}, err
	}
	if !reflect.DeepEqual(current, previous) {
		return Status{}, fmt.Errorf("OpenAI profile changed during sign-in; run tempest login openai again")
	}
	if err := m.save(r); err != nil {
		return Status{}, err
	}
	if !planEnabled(r.Scopes) {
		return Status{}, fmt.Errorf("signed in, but ChatGPT plan use is disabled; run tempest login openai --profile %s --consent, or explicitly select an API-key route", profile)
	}
	if err := m.atomic("active", []byte(profile)); err != nil {
		return Status{}, err
	}
	return m.Status(profile)
}

// Credential is a request-local bearer and its bound API origin. It is never persisted in a session.
type Credential struct {
	AccessToken string
	BaseURL     string
}

// Access serializes refresh across processes and rereads after locking so only
// one consumer rotates a renewable session. Login and logout use the same lock.
func (m *Manager) Access(ctx context.Context, profile string) (Credential, error) {
	profile, err := m.profile(profile)
	if err != nil {
		return Credential{}, err
	}
	unlock, err := m.lock(ctx, profile)
	if err != nil {
		return Credential{}, err
	}
	defer unlock()
	r, err := m.read(profile)
	if errors.Is(err, os.ErrNotExist) {
		return Credential{}, fmt.Errorf("not signed in with ChatGPT; run tempest login openai")
	}
	if err != nil {
		return Credential{}, err
	}
	if err := ValidateIssuer(r.Issuer); err != nil {
		return Credential{}, err
	}
	if r.AccessToken == "" || r.RefreshToken == "" {
		return Credential{}, fmt.Errorf("OpenAI sign-in expired or was revoked; run tempest login openai")
	}
	if !planEnabled(r.Scopes) {
		return Credential{}, fmt.Errorf("ChatGPT plan use is disabled; run tempest login openai --profile %s --consent", profile)
	}
	if r.RefreshPending || time.Until(r.ExpiresAt) <= 5*time.Minute && !time.Now().Before(r.EarliestRefreshAt) {
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		d, err := m.discover(refreshCtx, r.Issuer)
		if err != nil {
			if r.RefreshPending || !time.Now().Before(r.ExpiresAt) {
				return Credential{}, err
			}
		} else {
			if !r.RefreshPending {
				tokens, err := m.exchange(refreshCtx, d.Token, url.Values{"grant_type": {"refresh_token"}, "client_id": {r.ClientID}, "refresh_token": {r.RefreshToken}, "resource": {Resource}})
				if err != nil {
					var failure *tokenError
					if errors.As(err, &failure) && failure.terminal {
						r.AccessToken = ""
						r.RefreshToken = ""
						r.IDToken = ""
						if saveErr := m.save(r); saveErr != nil {
							return Credential{}, fmt.Errorf("OpenAI sign-in revoked and local token cleanup failed: %w", saveErr)
						}
						return Credential{}, err
					}
					if !time.Now().Before(r.ExpiresAt) {
						return Credential{}, err
					}
				} else {
					// Rotation consumes the old refresh token, even if identity checks fail.
					applyErr := applyTokens(&r, tokens, false)
					r.RefreshPending = tokens.ID != ""
					if err := m.save(r); err != nil {
						return Credential{}, err
					}
					if applyErr != nil {
						return Credential{}, applyErr
					}
				}
			}
			if r.RefreshPending {
				id, err := m.verify(refreshCtx, d, r.IDToken, r.ClientID, "")
				if err != nil {
					return Credential{}, err
				}
				if id.Subject != r.Subject {
					return Credential{}, fmt.Errorf("OpenAI refreshed identity changed; run tempest login openai")
				}
				r.RefreshPending = false
				if err := m.save(r); err != nil {
					return Credential{}, err
				}
			}
		}
	}
	if !planEnabled(r.Scopes) {
		return Credential{}, fmt.Errorf("ChatGPT plan use is disabled; run tempest login openai --profile %s --consent", profile)
	}
	if !time.Now().Before(r.ExpiresAt) {
		return Credential{}, fmt.Errorf("OpenAI token expired before renewal is permitted; retry later or run tempest login openai")
	}
	base := Resource
	if r.Issuer != Issuer {
		base = r.Issuer + "/v1"
	}
	return Credential{AccessToken: r.AccessToken, BaseURL: base}, nil
}

// Logout attempts session revocation, then clears local secrets even if the
// authority is unavailable. Registration and host identity remain reusable.
func (m *Manager) Logout(ctx context.Context, profile string) error {
	profile, err := m.profile(profile)
	if err != nil {
		return err
	}
	unlock, err := m.lock(ctx, profile)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := m.read(profile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var remoteErr error
	if r.RefreshToken != "" {
		remoteErr = m.revoke(ctx, r)
	}
	r.AccessToken = ""
	r.RefreshToken = ""
	r.IDToken = ""
	r.ExpiresAt = time.Time{}
	r.RefreshPending = false
	if err := m.save(r); err != nil {
		return err
	}
	if remoteErr != nil {
		return fmt.Errorf("signed out locally, but remote revocation was not confirmed; disconnect Tempest in ChatGPT Settings")
	}
	return nil
}

func (m *Manager) revoke(ctx context.Context, r record) error {
	d, err := m.discover(ctx, r.Issuer)
	if err != nil {
		return err
	}
	if d.Revocation == "" {
		return fmt.Errorf("revocation endpoint missing")
	}
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
			}
		}
		values := url.Values{"token": {r.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {r.ClientID}}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Revocation, strings.NewReader(values.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := m.client().Do(req)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		if resp.StatusCode < 500 {
			break
		}
	}
	return fmt.Errorf("revocation unavailable")
}
