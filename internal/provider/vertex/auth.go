// Package vertex routes Gemini and Claude to Google Vertex AI.
package vertex

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"go.harness.dev/harness/internal/provider/base"
)

type account struct {
	Type     string `json:"type"`
	Project  string `json:"project_id"`
	Email    string `json:"client_email"`
	KeyID    string `json:"private_key_id"`
	Key      string `json:"private_key"`
	TokenURI string `json:"token_uri"`
}
type cachedToken struct {
	Token, Project string
	Expires        time.Time
}

var tokens = struct {
	sync.Mutex
	entries map[[32]byte]cachedToken
}{entries: map[[32]byte]cachedToken{}}

func accessToken(ctx context.Context) (string, string, error) {
	path := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if path == "" {
		command, err := exec.LookPath("gcloud")
		if err != nil {
			return "", "", fmt.Errorf("Vertex requires GOOGLE_APPLICATION_CREDENTIALS, an Authorization header, or gcloud auth login: %w", err)
		}
		identity := strings.Join([]string{"gcloud", command, os.Getenv("HOME"), os.Getenv("CLOUDSDK_CONFIG"), os.Getenv("CLOUDSDK_ACTIVE_CONFIG_NAME"), os.Getenv("CLOUDSDK_CORE_ACCOUNT"), os.Getenv("CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT")}, "\x00")
		hash := sha256.Sum256([]byte(identity))
		tokens.Lock()
		defer tokens.Unlock()
		if cached, ok := tokens.entries[hash]; ok && time.Now().Add(time.Minute).Before(cached.Expires) {
			return cached.Token, cached.Project, nil
		}
		// The config helper reports expiry; print-access-token doesn't.
		commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(commandCtx, command, "config", "config-helper", "--format=json", "--min-expiry=60s").Output()
		if err != nil {
			return "", "", fmt.Errorf("Vertex gcloud credentials: %w", err)
		}
		var result struct {
			Credential struct {
				Token  string    `json:"access_token"`
				Expiry time.Time `json:"token_expiry"`
			} `json:"credential"`
			Configuration struct {
				Properties struct {
					Core struct {
						Project string `json:"project"`
					} `json:"core"`
				} `json:"properties"`
			} `json:"configuration"`
		}
		if err := json.Unmarshal(out, &result); err != nil {
			return "", "", fmt.Errorf("Vertex gcloud credentials: %w", err)
		}
		if result.Credential.Token == "" {
			return "", "", fmt.Errorf("gcloud returned an empty access token")
		}
		cached := cachedToken{result.Credential.Token, result.Configuration.Properties.Core.Project, result.Credential.Expiry}
		if cached.Expires.After(time.Now()) {
			tokens.entries[hash] = cached
		}
		return cached.Token, cached.Project, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("Vertex service account: %w", err)
	}
	hash := sha256.Sum256(raw)
	tokens.Lock()
	defer tokens.Unlock()
	if cached, ok := tokens.entries[hash]; ok && time.Now().Add(time.Minute).Before(cached.Expires) {
		return cached.Token, cached.Project, nil
	}
	var a account
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", "", fmt.Errorf("Vertex service account: %w", err)
	}
	if a.Type != "service_account" || a.Email == "" || a.Key == "" {
		return "", "", fmt.Errorf("GOOGLE_APPLICATION_CREDENTIALS must contain a service_account JSON key")
	}
	if a.TokenURI == "" {
		a.TokenURI = "https://oauth2.googleapis.com/token"
	}
	uri, err := url.Parse(a.TokenURI)
	if err != nil || uri.Host == "" || uri.User != nil || (uri.Scheme != "https" && !(uri.Scheme == "http" && (uri.Hostname() == "localhost" || uri.Hostname() == "127.0.0.1" || uri.Hostname() == "::1"))) {
		return "", "", fmt.Errorf("invalid service-account token_uri: HTTPS is required")
	}
	block, _ := pem.Decode([]byte(a.Key))
	if block == nil {
		return "", "", fmt.Errorf("invalid service-account private key PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", "", fmt.Errorf("service-account PKCS8 private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return "", "", fmt.Errorf("service-account private key must be RSA")
	}
	now := time.Now()
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": a.KeyID})
	claims, _ := json.Marshal(map[string]any{"iss": a.Email, "scope": "https://www.googleapis.com/auth/cloud-platform", "aud": a.TokenURI, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", "", err
	}
	assertion := unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return "", "", base.ResponseError(response, body)
	}
	var result struct {
		Token   string `json:"access_token"`
		Expires int    `json:"expires_in"`
		Type    string `json:"token_type"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return "", "", err
	}
	if result.Token == "" || result.Expires <= 0 || !strings.EqualFold(result.Type, "Bearer") {
		return "", "", fmt.Errorf("invalid OAuth access token response")
	}
	tokens.entries[hash] = cachedToken{result.Token, a.Project, now.Add(time.Duration(result.Expires) * time.Second)}
	return result.Token, a.Project, nil
}
