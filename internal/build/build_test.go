package build

import "testing"

func TestMetadataFromInjectedLinkerValues(t *testing.T) {
	got := metadataFrom("v1.2.3", "abc123def", "2026-07-17T12:34:56Z", nil)
	want := Metadata{Version: "v1.2.3", Commit: "abc123def", Date: "2026-07-17T12:34:56Z"}
	if got != want {
		t.Fatalf("metadata = %#v, want %#v", got, want)
	}
}

func TestCurrentMetadataIsComplete(t *testing.T) {
	if Name != "tempest" {
		t.Fatalf("Name = %q, want tempest", Name)
	}
	got := Current()
	if got.Version == "" || got.Commit == "" || got.Date == "" {
		t.Fatalf("Current() = %#v", got)
	}
}
