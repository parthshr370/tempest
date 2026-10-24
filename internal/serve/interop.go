package serve

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	model "github.com/a2aproject/a2a-go/v2/a2a"
	"go.harness.dev/harness/internal/a2a"
	"go.harness.dev/harness/internal/mcpconfig"
	"go.harness.dev/harness/tempest"
)

type opening struct {
	done chan struct{}
	err  error
}

func (s *Server) initInterop() error {
	servers, err := mcpconfig.Normalize(s.opts.DefaultNew.MCPServers)
	if err != nil {
		return err
	}
	s.opts.DefaultNew.MCPServers = servers
	if s.opts.Cwd == "" {
		s.opts.Cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	if !filepath.IsAbs(s.opts.Cwd) {
		return errors.New("server workspace must be absolute")
	}
	info, err := os.Stat(s.opts.Cwd)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("server workspace must be a directory")
	}
	if s.opts.A2ABaseURL != "" {
		return s.configureA2A(s.opts.A2ABaseURL)
	}
	return nil
}
func (s *Server) configureA2A(base string) error {
	server, err := a2a.New(s, a2a.Options{BaseURL: base, Cwd: s.opts.Cwd, Defaults: s.Defaults()})
	if err != nil {
		return err
	}
	u, _ := url.Parse(base)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return tempest.ErrClosed
	}
	s.a2a = server
	s.origin = u.Scheme + "://" + u.Host
	return nil
}
func (s *Server) openContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(s.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}
func (s *Server) resumeOpen(ctx context.Context, cwd, id string, servers []tempest.MCPServerConfig, pending *opening) (session *tempest.Session, err error) {
	defer s.openers.Done()
	defer func() { s.mu.Lock(); pending.err = err; delete(s.opening, id); close(pending.done); s.mu.Unlock() }()
	ctx, cancel := s.openContext(ctx)
	defer cancel()
	settings := tempest.ResumeSession{SessionID: id, Budget: s.opts.DefaultNew.Budget, PermissionMode: s.opts.DefaultNew.PermissionMode, MCPServers: servers}
	session, err = s.host.OpenWithOptions(ctx, id, settings)
	if err != nil {
		return nil, err
	}
	if cwd != "" && session.Snapshot().Cwd != cwd {
		_ = s.host.CloseSession(context.Background(), session.ID())
		return nil, errors.New("workspace differs from resumed session")
	}
	s.mu.Lock()
	if !s.closed {
		s.sessions[session.ID()] = &entry{session: session, lastActive: time.Now()}
		s.mu.Unlock()
		return session, nil
	}
	s.mu.Unlock()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), s.opts.ShutdownTimeout)
	defer closeCancel()
	_ = s.host.CloseSession(closeCtx, session.ID())
	return nil, tempest.ErrClosed
}
func (s *Server) authorize(queryToken, checkOrigin bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Token != "" {
			header := r.Header.Get("Authorization")
			token := ""
			if strings.HasPrefix(header, "Bearer ") {
				token = strings.TrimPrefix(header, "Bearer ")
			} else if queryToken {
				token = r.URL.Query().Get("token")
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(s.opts.Token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="Tempest"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		s.mu.Lock()
		closed, origin := s.closed, s.origin
		s.mu.Unlock()
		if closed {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		if checkOrigin && r.Header.Get("Origin") != "" && r.Header.Get("Origin") != origin {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) handleA2A(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	server := s.a2a
	s.mu.Unlock()
	if server == nil {
		http.Error(w, "A2A advertised address is not configured", 503)
		return
	}
	server.Handler().ServeHTTP(w, r)
}
func (s *Server) publicCard() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", 405)
			return
		}
		s.mu.Lock()
		server, origin := s.a2a, s.origin
		s.mu.Unlock()
		if r.Header.Get("Origin") != "" && r.Header.Get("Origin") != origin {
			http.Error(w, "origin not allowed", 403)
			return
		}
		if server == nil {
			http.Error(w, "A2A advertised address is not configured", 503)
			return
		}
		card := server.Card()
		if s.opts.Token != "" {
			card.SecuritySchemes = model.NamedSecuritySchemes{"bearer": model.HTTPAuthSecurityScheme{Scheme: "Bearer"}}
			card.SecurityRequirements = model.SecurityRequirementsOptions{{"bearer": {}}}
		}
		raw, err := json.Marshal(card)
		if err != nil {
			http.Error(w, "cannot encode agent card", 500)
			return
		}
		digest := sha256.Sum256(raw)
		etag := `"` + hex.EncodeToString(digest[:]) + `"`
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(raw)
	})
}
