// Package obs provides secret redaction for logs emitted by the Harness binary
// and agent loop. Every logged string must be scrubbed of credentials.
package obs

import (
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"strings"
)

// jsonSecret matches JSON-style credential members ("key":"value"), including
// keys wrapped in quotes with the colon separator, so the quoted value is
// masked while the key stays for context. Runs before the plainer matchers
// because "api_key":"x" has a quote between key and colon that kvSecret cannot
// span.
var jsonSecret = regexp.MustCompile(`(?i)("(?:key|[\w.-]*(?:api[_-]?key|apikey|token|secret|password|authorization|auth[_-]?token)[\w.-]*)"\s*:\s*)"[^"]*"`)

// bareSecret matches standalone credential-looking tokens (bearer values and
// sk-style keys) anywhere in the text. It runs before kvSecret so that
// "Authorization: Bearer <token>" masks the whole "Bearer <token>" span rather
// than only the word "Bearer".
var bareSecret = regexp.MustCompile(`(?i)(bearer\s+[A-Za-z0-9._~+/=-]{8,}|sk-[A-Za-z0-9_-]{8,})`)

// urlSecret masks userinfo credentials embedded in URLs (scheme://user:pass@host).
var urlSecret = regexp.MustCompile(`(?i)(https?://)[^:/?#@\s]+:[^@/?#\s]+@`)

// kvSecret matches "key: value" / "key=value" pairs whose key names a
// credential, so the value can be masked while the key name is kept for context.
var kvSecret = regexp.MustCompile(`(?i)((?:\bkey|[\w.-]*(?:api[_-]?key|apikey|token|secret|password|authorization|auth[_-]?token)[\w.-]*)\s*[:=]\s*)(\S+)`)

// Redact masks common credential patterns so a diagnostic log line cannot
// persist API keys, bearer tokens, or key=value secrets. It is best-effort, not
// a guarantee. Matcher order matters — JSON members, then URL userinfo, then
// bearer/sk spans, then plain key:value.
func Redact(s string) string {
	s = jsonSecret.ReplaceAllString(s, `${1}"[REDACTED]"`)
	s = urlSecret.ReplaceAllString(s, `${1}[REDACTED]@`)
	s = bareSecret.ReplaceAllString(s, "[REDACTED]")
	s = kvSecret.ReplaceAllString(s, "${1}[REDACTED]")
	return s
}

// RedactTruncate redacts secrets, collapses whitespace to a single line, and
// truncates to max runes with a marker. It bounds any string bound for a log
// attribute (error messages, previews) so a huge tool/provider payload cannot
// blow up a log record.
func RedactTruncate(s string, max int) string {
	s = Redact(strings.Join(strings.Fields(s), " "))
	r := []rune(s)
	if max >= 0 && len(r) > max {
		return string(r[:max]) + "…[truncated]"
	}
	return s
}

// RedactValues also masks credentials with arbitrary header or query names.
func RedactValues(s string, values ...string) string {
	for _, value := range values {
		if value == "" {
			continue
		}
		encodedJSON, _ := json.Marshal(value)
		for _, encoded := range []string{value, url.QueryEscape(value), url.PathEscape(value), string(encodedJSON[1 : len(encodedJSON)-1])} {
			s = strings.ReplaceAll(s, encoded, "[REDACTED]")
		}
	}
	return Redact(s)
}

// RedactingWriter scrubs diagnostics before they reach their output destination.
type RedactingWriter struct {
	io.Writer
	Values []string
}

func (w RedactingWriter) Write(p []byte) (int, error) {
	_, err := io.WriteString(w.Writer, RedactValues(string(p), w.Values...))
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
