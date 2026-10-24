package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type refreshContextTransport struct{ t *testing.T }

func (transport refreshContextTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	deadline, ok := request.Context().Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 30*time.Second {
		transport.t.Error("refresh did not use its own bounded timeout")
	}
	return http.DefaultTransport.RoundTrip(request)
}

func visitFixture(authorizationURL string) error {
	response, err := http.Get(authorizationURL)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("callback HTTP %d", response.StatusCode)
	}
	return nil
}

func TestRefreshPersistsRotationBeforeIdentityValidation(t *testing.T) {
	for _, scenario := range []string{"valid", "jwks-outage", "signature", "subject", "audience", "expiry", "earliest-refresh"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newAuthorizationFixture(t)
			manager := fixtureManager(t, fixture)
			fixtureLogin(t, manager)
			expireFixture(t, manager)
			fixture.mu.Lock()
			fixture.checkRefreshJWKS = func() {
				saved, err := manager.read("default")
				if err != nil || saved.AccessToken != "fixture-access-rotated" || saved.RefreshToken != "fixture-refresh-rotated" || !saved.RefreshPending {
					t.Errorf("rotation was not saved before JWKS: %v", err)
				}
			}
			switch scenario {
			case "jwks-outage":
				fixture.jwksStatus = http.StatusServiceUnavailable
			case "signature":
				fixture.tamperSignature = true
			case "subject":
				fixture.identitySubject = "another-account"
			case "audience":
				fixture.modifyIdentity = func(id *identity) { id.AuthorizedParty = "another-client" }
			case "expiry":
				fixture.modifyIdentity = func(id *identity) { id.Expires = time.Now().Add(-time.Hour).Unix() }
			case "earliest-refresh":
				fixture.refreshEarliest = "invalid-timestamp"
			}
			fixture.mu.Unlock()
			credential, err := manager.Access(t.Context(), "default")
			if scenario == "valid" {
				if err != nil || credential.AccessToken != "fixture-access-rotated" {
					t.Fatalf("valid refreshed identity: %v", err)
				}
			} else if err == nil || credential.AccessToken != "" {
				t.Fatal("invalid refresh metadata authorized inference")
			}
			saved, err := manager.read("default")
			if err != nil || saved.AccessToken != "fixture-access-rotated" || saved.RefreshToken != "fixture-refresh-rotated" {
				t.Fatalf("replacement token pair lost: %v", err)
			}
			if saved.RefreshPending != (scenario != "valid") {
				t.Fatal("unvalidated refreshed identity was not fenced")
			}
			if scenario == "jwks-outage" {
				fixture.mu.Lock()
				fixture.jwksStatus = 0
				fixture.mu.Unlock()
				restarted := &Manager{Dir: manager.Dir}
				if _, err := restarted.Access(t.Context(), "default"); err != nil {
					t.Fatalf("recovering JWKS fetch lost rotated credentials: %v", err)
				}
			} else if scenario != "valid" && scenario != "earliest-refresh" {
				if _, err := (&Manager{Dir: manager.Dir}).Access(t.Context(), "default"); err == nil {
					t.Fatal("restart bypassed refreshed identity validation")
				}
			}
			refreshes, _, _ := fixture.counts()
			if refreshes != 1 {
				t.Fatalf("rotated again before identity validation: %d", refreshes)
			}
		})
	}
}

func TestRefreshSurvivesRequestCancellation(t *testing.T) {
	fixture := newAuthorizationFixture(t)
	manager := fixtureManager(t, fixture)
	fixtureLogin(t, manager)
	expireFixture(t, manager)
	manager.Client = &http.Client{Transport: refreshContextTransport{t}}
	started, proceed := make(chan struct{}), make(chan struct{})
	fixture.mu.Lock()
	fixture.refreshStarted, fixture.continueRefresh = started, proceed
	fixture.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := manager.Access(ctx, "default")
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(proceed)
		t.Fatal("refresh did not start")
	}
	cancel()
	close(proceed)
	if err := <-result; err != nil {
		t.Fatalf("turn cancellation interrupted rotating refresh: %v", err)
	}
	saved, err := manager.read("default")
	if err != nil || saved.RefreshToken != "fixture-refresh-rotated" || saved.RefreshPending {
		t.Fatalf("cancelled request lost the validated rotation: %v", err)
	}
}

func TestRefreshOutageUsesOnlyUnexpiredAccessToken(t *testing.T) {
	fixture := newAuthorizationFixture(t)
	manager := fixtureManager(t, fixture)
	fixtureLogin(t, manager)
	saved, err := manager.read("default")
	if err != nil {
		t.Fatal(err)
	}
	saved.ExpiresAt = time.Now().Add(4 * time.Minute)
	if err := manager.save(saved); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.refreshStatus = http.StatusServiceUnavailable
	fixture.mu.Unlock()
	credential, err := manager.Access(t.Context(), "default")
	if err != nil || credential.AccessToken != "fixture-access" {
		t.Fatalf("refresh outage rejected still-valid access token: %v", err)
	}
	expireFixture(t, manager)
	if _, err := manager.Access(t.Context(), "default"); err == nil {
		t.Fatal("refresh outage authorized expired access token")
	}
}

func TestLoginDoesNotLockBrowserWaitOrOverwriteConcurrentLogout(t *testing.T) {
	fixture := newAuthorizationFixture(t)
	manager := fixtureManager(t, fixture)
	fixtureLogin(t, manager)
	_, err := manager.Login(t.Context(), "default", false, func(authorizationURL string) error {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if _, err := manager.Access(ctx, "default"); err != nil {
			return fmt.Errorf("model request blocked by browser wait: %w", err)
		}
		if err := manager.Logout(ctx, "default"); err != nil {
			return err
		}
		return visitFixture(authorizationURL)
	})
	if err == nil || !strings.Contains(err.Error(), "profile changed") {
		t.Fatalf("login did not compare saved profile after browser wait: %v", err)
	}
	saved, err := manager.read("default")
	if err != nil || saved.AccessToken != "" || saved.RefreshToken != "" {
		t.Fatalf("login resurrected concurrent logout: %v", err)
	}
}

func TestPlanConsentIsExplicitAndDeclineKeepsActiveProfile(t *testing.T) {
	fixture := newAuthorizationFixture(t)
	manager := fixtureManager(t, fixture)
	fixtureLogin(t, manager)
	fixture.mu.Lock()
	fixture.missingScope = true
	fixture.mu.Unlock()
	_, err := manager.Login(t.Context(), "work", false, visitFixture)
	if err == nil || !strings.Contains(err.Error(), "--profile work --consent") {
		t.Fatalf("declined plan did not explain re-consent: %v", err)
	}
	active, err := os.ReadFile(filepath.Join(manager.Dir, "active"))
	if err != nil || string(active) != "default" {
		t.Fatalf("declined plan replaced working active profile: %v", err)
	}
	fixture.mu.Lock()
	fixture.missingScope = false
	fixture.mu.Unlock()
	for _, consent := range []bool{true, false} {
		_, err := manager.Login(t.Context(), "work", consent, func(raw string) error {
			u, err := url.Parse(raw)
			if err != nil {
				return err
			}
			q := u.Query()
			if (q.Get("prompt") == "consent") != consent || q.Get("scope") != Scopes || q.Get("client_id") != "oaiapp_fixture" {
				t.Error("re-consent did not preserve registration and complete scope set")
			}
			return visitFixture(raw)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	active, err = os.ReadFile(filepath.Join(manager.Dir, "active"))
	if err != nil || string(active) != "work" {
		t.Fatalf("enabled plan did not select profile: %v", err)
	}
}
