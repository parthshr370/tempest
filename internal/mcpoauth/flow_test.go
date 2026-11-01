package mcpoauth

import (
	"encoding/json"
	"testing"
	"time"
)

// A lifetime too long for a time.Duration, or not a number at all, is an
// unknown lifetime: an expiry in the past would refresh before every request.
func TestExpiresInOutOfRangeMeansUnknownLifetime(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for raw, want := range map[string]time.Time{
		`3600`:   now.Add(time.Hour),
		`"90"`:   now.Add(90 * time.Second),
		`0`:      {},
		`-5`:     {},
		`"NaN"`:  {},
		`"inf"`:  {},
		`1e12`:   {},
		`"soon"`: {},
	} {
		got := tokenResponse{ExpiresIn: json.RawMessage(raw)}.expiry(now)
		if !got.Equal(want) {
			t.Errorf("expires_in %s gives expiry %v, want %v", raw, got, want)
		}
	}
}
