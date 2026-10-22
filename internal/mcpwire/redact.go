package mcpwire

import (
	"encoding/json"
	"sort"
	"strings"

	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/internal/rpc"
)

type credentialRedactor struct{ replacer *strings.Replacer }

func newCredentialRedactor(cfg mcpconfig.Server) credentialRedactor {
	values := make(map[string]bool)
	add := func(value string) {
		if len(value) >= 8 {
			values[value] = true
		}
	}
	for _, value := range cfg.Headers {
		add(value)
	}
	for _, value := range cfg.Env {
		add(value)
	}
	for _, argument := range cfg.Args {
		add(argument)
		if _, value, ok := strings.Cut(argument, "="); ok {
			add(value)
		}
	}
	secrets := make([]string, 0, len(values))
	for value := range values {
		secrets = append(secrets, value)
	}
	sort.Slice(secrets, func(i, j int) bool {
		if len(secrets[i]) != len(secrets[j]) {
			return len(secrets[i]) > len(secrets[j])
		}
		return secrets[i] < secrets[j]
	})
	pairs := make([]string, 0, 2*len(secrets))
	for _, secret := range secrets {
		pairs = append(pairs, secret, "[redacted]")
	}
	if len(pairs) == 0 {
		return credentialRedactor{}
	}
	return credentialRedactor{strings.NewReplacer(pairs...)}
}

func (r credentialRedactor) text(value string) string {
	if r.replacer == nil {
		return value
	}
	return r.replacer.Replace(value)
}

// json scrubs decoded strings, including escaped credentials, without changing
// number precision or copying a response that needs no replacements.
func (r credentialRedactor) json(raw json.RawMessage) json.RawMessage {
	if r.replacer == nil {
		return raw
	}
	var out []byte
	copied := 0
	for i := 0; i < len(raw); i++ {
		if raw[i] != '"' {
			continue
		}
		start := i
		for i++; i < len(raw); i++ {
			if raw[i] == '\\' {
				i++
			} else if raw[i] == '"' {
				break
			}
		}
		if i >= len(raw) {
			return raw
		}
		var value string
		if json.Unmarshal(raw[start:i+1], &value) != nil {
			return raw
		}
		if safe := r.text(value); safe != value {
			encoded, _ := json.Marshal(safe)
			out = append(out, raw[copied:start]...)
			out = append(out, encoded...)
			copied = i + 1
		}
	}
	if out == nil {
		return raw
	}
	return append(out, raw[copied:]...)
}

type redactedError struct {
	error
	message string
}

func (e redactedError) Error() string { return e.message }
func (e redactedError) Unwrap() error { return e.error }

func (r credentialRedactor) err(err error) error {
	if err == nil {
		return nil
	}
	if remote, ok := err.(*rpc.Error); ok && r.replacer != nil {
		safe := *remote
		safe.Message = r.text(safe.Message)
		safe.Data = r.value(safe.Data)
		return &safe
	}
	if safe := r.text(err.Error()); safe != err.Error() {
		return redactedError{error: err, message: safe}
	}
	return err
}

func (r credentialRedactor) value(value any) any {
	switch value := value.(type) {
	case string:
		return r.text(value)
	case map[string]any:
		safe := make(map[string]any, len(value))
		for key, member := range value {
			safe[r.text(key)] = r.value(member)
		}
		return safe
	case []any:
		safe := make([]any, len(value))
		for i, member := range value {
			safe[i] = r.value(member)
		}
		return safe
	default:
		return value
	}
}
