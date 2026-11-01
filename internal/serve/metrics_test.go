package serve

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/telemetry"
	"go.harness.dev/harness/tempest"
)

func getMetrics(t *testing.T, url, token string) (*http.Response, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, string(body)
}

// TestMetricsEndpointRequiresToken: /metrics is mounted when the server has
// telemetry and sits behind the same bearer auth as /acp and /a2a.
func TestMetricsEndpointRequiresToken(t *testing.T) {
	tel := telemetry.New(telemetry.Options{Settings: config.OTelSettings{ServiceName: "tempest"}})
	s, err := New(Options{SessionDir: t.TempDir(), Cwd: t.TempDir(), Token: "secret", Stack: &tempest.StackConfig{Telemetry: tel}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	server := httptest.NewServer(s.Handler())
	defer server.Close()

	for _, token := range []string{"", "wrong"} {
		if response, _ := getMetrics(t, server.URL, token); response.StatusCode != http.StatusUnauthorized || response.Header.Get("WWW-Authenticate") == "" {
			t.Fatalf("token %q: status %d, want 401 with a challenge", token, response.StatusCode)
		}
	}
	response, body := getMetrics(t, server.URL, "secret")
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("authorized scrape: %d %q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	if !strings.Contains(body, "# TYPE tempest_sessions gauge\ntempest_sessions 0\n") {
		t.Fatalf("scrape body:\n%s", body)
	}
}

func TestMetricsEndpointAbsentWithoutTelemetry(t *testing.T) {
	s, err := New(Options{SessionDir: t.TempDir(), Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	if response, _ := getMetrics(t, server.URL, ""); response.StatusCode != http.StatusNotFound {
		t.Fatalf("/metrics without telemetry: status %d, want 404", response.StatusCode)
	}
}
