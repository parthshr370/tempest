package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestVerifyOpenRouter(t *testing.T) {
	if os.Getenv("TEMPEST_LIVE") != "1" {
		t.Skip("set TEMPEST_LIVE=1 to verify exact OpenRouter route IDs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://openrouter.ai/api/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models endpoint status %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]bool, len(body.Data))
	for _, m := range body.Data {
		ids[m.ID] = true
	}
	for _, m := range Models() {
		if m.Provider == "openrouter" && !ids[m.ID] {
			t.Errorf("route ID absent from discovery: %q", m.ID)
		}
	}
}
