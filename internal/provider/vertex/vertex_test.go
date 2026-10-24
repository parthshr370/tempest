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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
	"go.harness.dev/harness/internal/provider/conformance"
)

func TestGeminiConformance(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_API_KEY", "")
	conformance.Run(t, conformance.Factory{Dialect: "google", Stream: StreamSimple})
}
func TestAnthropicConformance(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "fixture-project")
	t.Setenv("GOOGLE_CLOUD_API_KEY", "")
	conformance.Run(t, conformance.Factory{Dialect: "anthropic", Stream: func(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream {
		model.ID = "claude-fixture"
		copy := *opts
		copy.APIKey = ""
		copy.Headers = map[string]string{"Authorization": "Bearer fixture-token"}
		return StreamSimple(ctx, model, c, &copy)
	}})
}

func TestServiceAccountJWTAndCache(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var exchanges atomic.Int32
	var endpoint *httptest.Server
	endpoint = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			exchanges.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
				t.Errorf("grant: %v", r.Form)
			}
			parts := strings.Split(r.Form.Get("assertion"), ".")
			if len(parts) != 3 {
				t.Error("invalid JWT")
				http.Error(w, "JWT", 400)
				return
			}
			signature, err := base64.RawURLEncoding.DecodeString(parts[2])
			if err != nil {
				t.Error(err)
			}
			sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], signature); err != nil {
				t.Errorf("RS256: %v", err)
			}
			headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
			if err != nil {
				t.Error(err)
			}
			var header struct {
				Algorithm string `json:"alg"`
				KeyID     string `json:"kid"`
			}
			if err := json.Unmarshal(headerRaw, &header); err != nil {
				t.Error(err)
			}
			if header.Algorithm != "RS256" || header.KeyID != "key-id" {
				t.Errorf("header: %+v", header)
			}
			claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
			if err != nil {
				t.Error(err)
			}
			var claims struct {
				Issuer   string `json:"iss"`
				Scope    string `json:"scope"`
				Audience string `json:"aud"`
				Issued   int64  `json:"iat"`
				Expires  int64  `json:"exp"`
			}
			if err := json.Unmarshal(claimsRaw, &claims); err != nil {
				t.Error(err)
			}
			if claims.Issuer != "agent@example.iam.gserviceaccount.com" || claims.Audience != endpoint.URL+"/token" || claims.Scope != "https://www.googleapis.com/auth/cloud-platform" || claims.Expires-claims.Issued != 3600 || claims.Issued > time.Now().Unix() {
				t.Errorf("claims: %+v", claims)
			}
			io.WriteString(w, `{"access_token":"oauth-token","token_type":"Bearer","expires_in":3600}`)
			return
		}
		if strings.Contains(r.URL.Path, "/publishers/anthropic/") {
			if r.Header.Get("Authorization") != "Bearer oauth-token" || r.Header.Get("X-Goog-Api-Key") != "" || r.Header.Get("X-Api-Key") != "" {
				t.Errorf("Claude credentials: %v", r.Header)
			}
			io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		if r.URL.Path != "/v1/projects/account-project/locations/us-central1/publishers/google/models/gemini-test:streamGenerateContent" || r.URL.Query().Get("alt") != "sse" || r.Header.Get("Authorization") != "Bearer oauth-token" || r.Header.Get("X-Goog-Api-Key") != "" {
			t.Errorf("Vertex request: %s %v", r.URL, r.Header)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if _, ok := body["model"]; ok {
			t.Error("model must be omitted from Vertex body")
		}
		if !strings.Contains(string(body["contents"]), "inlineData") || !strings.Contains(string(body["tools"]), "functionDeclarations") {
			t.Errorf("wire: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":2,\"thoughtsTokenCount\":3,\"cachedContentTokenCount\":2,\"totalTokenCount\":13}}\n\n")
	}))
	defer endpoint.Close()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	credentials := account{Type: "service_account", Project: "account-project", Email: "agent@example.iam.gserviceaccount.com", KeyID: "key-id", Key: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), TokenURI: endpoint.URL + "/token"}
	raw, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "account.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GCLOUD_PROJECT", "")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "us-central1")
	t.Setenv("GOOGLE_CLOUD_API_KEY", "")
	model := types.Model{ID: "gemini-test", API: "google-vertex", Provider: "vertex", BaseURL: endpoint.URL, MaxTokens: 100, Input: []string{"text", "image"}}
	history := types.Context{Messages: []types.Message{types.UserMessage{Content: types.BlockContent(types.NewText("hi"), types.NewImage("aW1hZ2U=", "image/png"))}}, Tools: []types.Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	for range 2 {
		result, err := StreamSimple(context.Background(), model, history, nil).Result(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if result.StopReason != types.StopStop || result.Usage.Reasoning == nil || *result.Usage.Reasoning != 3 {
			t.Fatalf("result: %+v", result)
		}
	}
	if exchanges.Load() != 1 {
		t.Fatalf("token wasn't cached: %d exchanges", exchanges.Load())
	}
	hash := sha256.Sum256(raw)
	tokens.Lock()
	cached := tokens.entries[hash]
	cached.Expires = time.Now().Add(-time.Second)
	tokens.entries[hash] = cached
	tokens.Unlock()
	if _, _, err := accessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exchanges.Load() != 2 {
		t.Fatal("expired token wasn't refreshed")
	}
	t.Setenv("GOOGLE_CLOUD_API_KEY", "express-key")
	model.ID = "claude-test"
	for _, opts := range []*types.SimpleStreamOptions{nil, {StreamOptions: types.StreamOptions{APIKey: "explicit-express-key"}}} {
		reply, err := StreamSimple(context.Background(), model, history, opts).Result(context.Background())
		if err != nil || reply.StopReason != types.StopStop {
			t.Fatalf("Express key blocked Claude service-account auth: %+v %v", reply, err)
		}
	}
}
func TestAnthropicVertexWire(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "project")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "global")
	t.Setenv("GOOGLE_CLOUD_API_KEY", "")
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/project/locations/global/publishers/anthropic/models/claude-test:streamRawPredict" || r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("Anthropic-Version") != "" {
			t.Errorf("request: %s %v", r.URL, r.Header)
		}
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if string(payload["anthropic_version"]) != `"vertex-2023-10-16"` || payload["model"] != nil || !strings.Contains(r.Header.Get("Anthropic-Beta"), "context-1m-2025-08-07") {
			t.Errorf("wire: %s", payload)
		}
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer endpoint.Close()
	result, err := StreamSimple(context.Background(), types.Model{ID: "claude-test", BaseURL: endpoint.URL, MaxTokens: 100}, types.Context{Messages: []types.Message{types.UserMessage{Content: types.StringContent("hi")}}}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{Headers: map[string]string{"Authorization": "Bearer token", "anthropic-beta": "context-1m-2025-08-07"}}}).Result(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != types.StopStop || result.Content[0].Text != "hello" {
		t.Fatalf("result: %+v", result)
	}
}

func TestGeminiStreamStatusCodes(t *testing.T) {
	for _, test := range []struct {
		Status int
		Want   string
	}{{429, "rate_limited"}, {503, "server_failure"}, {401, "authentication"}} {
		t.Run(fmt.Sprint(test.Status), func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				fmt.Fprintf(writer, "data: {\"error\":{\"code\":%d,\"message\":\"request failed\"}}\n\n", test.Status)
			}))
			defer endpoint.Close()
			result, err := StreamSimple(context.Background(), types.Model{ID: "gemini-test", BaseURL: endpoint.URL + "/v1/publishers/google/models/gemini-test:streamGenerateContent?alt=sse"}, types.Context{}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{NoAuth: true}}).Result(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result.ErrorCode != test.Want {
				t.Fatalf("error code: %s, want %s", result.ErrorCode, test.Want)
			}
		})
	}
}

func TestGeminiTruncatedStream(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}\n\n")
	}))
	defer endpoint.Close()
	result, err := StreamSimple(context.Background(), types.Model{ID: "gemini-test", BaseURL: endpoint.URL + "/v1/publishers/google/models/gemini-test:streamGenerateContent?alt=sse"}, types.Context{}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{NoAuth: true}}).Result(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.ErrorCode != "stream_interrupted" || len(result.Content) != 1 || result.Content[0].Text != "partial" {
		t.Fatalf("truncated response: %+v", result)
	}
}

func TestGcloudFallbackDoesNotReplaceConfiguredAccount(t *testing.T) {
	directory := t.TempDir()
	command := filepath.Join(directory, "gcloud")
	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	calls := filepath.Join(directory, "calls")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1 $2 $3 $4\" != 'config config-helper --format=json --min-expiry=60s' ]; then exit 2; fi\nprintf 'call\\n' >> %q\nprintf '%%s' '{\"credential\":{\"access_token\":\"cli-token\",\"token_expiry\":%q},\"configuration\":{\"properties\":{\"core\":{\"project\":\"cli-project\"}}}}'\n", calls, expiry)
	if err := os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	for range 2 {
		token, project, err := accessToken(context.Background())
		if err != nil || token != "cli-token" || project != "cli-project" {
			t.Fatalf("gcloud token: %q %q %v", token, project, err)
		}
	}
	if raw, err := os.ReadFile(calls); err != nil || string(raw) != "call\n" {
		t.Fatalf("gcloud token wasn't cached: %q %v", raw, err)
	}
	tokens.Lock()
	for hash, cached := range tokens.entries {
		if cached.Token == "cli-token" {
			cached.Expires = time.Now().Add(30 * time.Second)
			tokens.entries[hash] = cached
		}
	}
	tokens.Unlock()
	if _, _, err := accessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(calls); err != nil || string(raw) != "call\ncall\n" {
		t.Fatalf("expiring gcloud token wasn't refreshed: %q %v", raw, err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(directory, "missing-account.json"))
	if token, _, err := accessToken(context.Background()); err == nil || token != "" {
		t.Fatal("configured service account fell back to another identity")
	}
}

func TestAnthropicEndpointAndResourceRouting(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "project")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "global")
	t.Setenv("GOOGLE_CLOUD_API_KEY", "ignored-express-key")
	for _, test := range []struct{ Model, Base, Path string }{
		{"publishers/anthropic/models/claude-test@20250929", "", "/v1/projects/project/locations/global/publishers/anthropic/models/claude-test@20250929:streamRawPredict"},
		{"projects/resource-project/locations/us-east5/publishers/anthropic/models/claude-test", "", "/v1/projects/resource-project/locations/us-east5/publishers/anthropic/models/claude-test:streamRawPredict"},
		{"deployment-alias", "/v1/custom:streamRawPredict", "/v1/custom:streamRawPredict"},
	} {
		t.Run(test.Model, func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != test.Path || strings.Contains(r.URL.EscapedPath(), "%2F") {
					t.Errorf("resource path: %s", r.URL)
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["messages"] == nil || string(body["anthropic_version"]) != `"vertex-2023-10-16"` || body["contents"] != nil {
					t.Errorf("wrong Vertex publisher: %s", body)
				}
				io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer endpoint.Close()
			reply, err := StreamSimple(context.Background(), types.Model{ID: test.Model, BaseURL: endpoint.URL + test.Base, MaxTokens: 100}, types.Context{}, &types.SimpleStreamOptions{StreamOptions: types.StreamOptions{Headers: map[string]string{"Authorization": "Bearer token"}}}).Result(context.Background())
			if err != nil || reply.StopReason != types.StopStop {
				t.Fatalf("Claude route: %+v %v", reply, err)
			}
		})
	}
}
