package tempest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.harness.dev/harness/internal/config"
	"go.harness.dev/harness/internal/prompt"
	"go.harness.dev/harness/internal/provider/faux"
	"go.harness.dev/harness/internal/telemetry"
)

func scrapeGauge(t *testing.T, tel *telemetry.Telemetry, name string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	tel.MetricsHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if value, ok := strings.CutPrefix(line, name+" "); ok {
			return value
		}
	}
	t.Fatalf("%s missing from scrape:\n%s", name, recorder.Body.String())
	return ""
}

// TestSessionGaugesFollowOpenAndClose: an open session counts in
// tempest.sessions with its unfinished jobs in tempest.jobs, and closing it
// releases both.
func TestSessionGaugesFollowOpenAndClose(t *testing.T) {
	tel := telemetry.New(telemetry.Options{Settings: config.OTelSettings{ServiceName: "tempest"}})
	f := faux.New(faux.Options{})
	s, err := Open(context.Background(), Options{Cwd: t.TempDir(), SessionDir: t.TempDir(), New: &NewSession{PermissionMode: PermissionMode("bypass")}, Stack: &StackConfig{Model: f.Model(), StreamFn: f.StreamSimple, ContextFiles: []prompt.ContextFile{}, Telemetry: tel}})
	if err != nil {
		t.Fatal(err)
	}
	if sessions, jobs := scrapeGauge(t, tel, "tempest_sessions"), scrapeGauge(t, tel, "tempest_jobs"); sessions != "1" || jobs != "0" {
		t.Fatalf("open session: sessions %s, jobs %s", sessions, jobs)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sessions := scrapeGauge(t, tel, "tempest_sessions"); sessions != "0" {
		t.Fatalf("closed session still counted: sessions %s", sessions)
	}
}
