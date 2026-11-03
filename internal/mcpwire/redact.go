package mcpwire

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/internal/rpc"
)

// credentialRedactor replaces one connection's credentials with [redacted]
// in everything the server sends back: the configured headers, environment,
// argv and OAuth client secret, and the bearer tokens Auth hands out. A token
// is held while it is the transport's current token and while any exchange
// that sent it is still being answered, so a rotation never unredacts an
// answer in flight and replaced tokens do not accumulate.
type credentialRedactor struct {
	static []string
	// dynamic is set when an Auth supplies tokens; only then do exchanges
	// track what they sent.
	dynamic  bool
	replacer atomic.Pointer[strings.Replacer]
	mu       sync.Mutex
	held     map[string]int
}

// secretLength is the shortest value treated as a credential, so short
// configuration values such as "1" don't mangle every response.
const secretLength = 8

func newCredentialRedactor(cfg mcpconfig.Server) *credentialRedactor {
	values := make(map[string]bool)
	add := func(value string) {
		if len(value) >= secretLength {
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
	if cfg.OAuth != nil {
		add(cfg.OAuth.ClientSecret)
	}
	r := &credentialRedactor{held: make(map[string]int)}
	for value := range values {
		r.static = append(r.static, value)
	}
	r.rebuildLocked()
	return r
}

// rebuildLocked publishes a replacer for the static and held secrets,
// longest first so a secret containing another is replaced whole.
func (r *credentialRedactor) rebuildLocked() {
	secrets := append([]string(nil), r.static...)
	for value := range r.held {
		if !contains(r.static, value) {
			secrets = append(secrets, value)
		}
	}
	if len(secrets) == 0 {
		r.replacer.Store(nil)
		return
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
	r.replacer.Store(strings.NewReplacer(pairs...))
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// hold keeps token redacted until release. Holds of one token nest.
func (r *credentialRedactor) hold(token string) (release func()) {
	if len(token) < secretLength {
		return func() {}
	}
	r.mu.Lock()
	r.held[token]++
	if r.held[token] == 1 {
		r.rebuildLocked()
	}
	r.mu.Unlock()
	return sync.OnceFunc(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.held[token]--; r.held[token] == 0 {
			delete(r.held, token)
			r.rebuildLocked()
		}
	})
}

type exchangeKey struct{}

// exchange is one logical request and everything its answer carries: the
// response, an error, and server messages on its stream. It holds each
// token it sent until the last of those has been scrubbed.
type exchange struct {
	mu       sync.Mutex
	refs     int
	releases []func()
}

// track starts an exchange on ctx. The caller scrubs the outcome, then
// calls end.
func (r *credentialRedactor) track(ctx context.Context) (_ context.Context, end func()) {
	if !r.dynamic {
		return ctx, func() {}
	}
	e := &exchange{refs: 1}
	return context.WithValue(ctx, exchangeKey{}, e), sync.OnceFunc(e.leave)
}

// sent records that a request on ctx carried token.
func (r *credentialRedactor) sent(ctx context.Context, token string) {
	e, _ := ctx.Value(exchangeKey{}).(*exchange)
	if e == nil {
		return
	}
	release := r.hold(token)
	e.mu.Lock()
	if e.refs == 0 {
		e.mu.Unlock()
		release()
		return
	}
	e.releases = append(e.releases, release)
	e.mu.Unlock()
}

// join extends the exchange on ctx, if any, while a server message it
// carried is handled.
func join(ctx context.Context) (leave func()) {
	e, _ := ctx.Value(exchangeKey{}).(*exchange)
	if e == nil {
		return func() {}
	}
	e.mu.Lock()
	e.refs++
	e.mu.Unlock()
	return sync.OnceFunc(e.leave)
}

func (e *exchange) leave() {
	e.mu.Lock()
	e.refs--
	var releases []func()
	if e.refs == 0 {
		releases, e.releases = e.releases, nil
	}
	e.mu.Unlock()
	for _, release := range releases {
		release()
	}
}

func (r *credentialRedactor) text(value string) string {
	replacer := r.replacer.Load()
	if replacer == nil {
		return value
	}
	return replacer.Replace(value)
}

// json scrubs decoded strings, including escaped credentials, without changing
// number precision or copying a response that needs no replacements.
func (r *credentialRedactor) json(raw json.RawMessage) json.RawMessage {
	if r.replacer.Load() == nil {
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

func (r *credentialRedactor) err(err error) error {
	if err == nil {
		return nil
	}
	if remote, ok := err.(*rpc.Error); ok && r.replacer.Load() != nil {
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

func (r *credentialRedactor) value(value any) any {
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
