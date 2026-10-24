package obs

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestRedactQueryKeysAndConfiguredValues(t *testing.T) {
	for _, input := range []string{`https://example.test?key=private`, `{"key":"private"}`, "key: private"} {
		if strings.Contains(Redact(input), "private") {
			t.Fatalf("key leaked: %s", Redact(input))
		}
	}
	const secret = "opaque /+value"
	var out bytes.Buffer
	writer := RedactingWriter{Writer: &out, Values: []string{secret}}
	input := secret + " " + url.QueryEscape(secret) + " " + url.PathEscape(secret)
	if n, err := writer.Write([]byte(input)); err != nil || n != len(input) {
		t.Fatalf("write n=%d err=%v", n, err)
	}
	if strings.Contains(out.String(), "opaque") || !strings.Contains(out.String(), "[REDACTED]") {
		t.Fatal("configured credential leaked")
	}
	quoted := "opaque \"quoted\" credential"
	encoded, _ := json.Marshal(map[string]string{"message": quoted})
	safe := RedactValues(string(encoded), quoted)
	if strings.Contains(safe, "quoted") {
		t.Fatal("JSON-escaped credential leaked")
	}
}
