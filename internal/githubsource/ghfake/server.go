// Package ghfake is the shared GitHub test fixture of the ticket runner: an
// HTTP server that tests program with REST routes, and a fake `gh` that the
// production gh client executes unchanged.
//
// The fake gh is the test binary itself. A test package's TestMain calls
// MainIfFake first; the gh client is configured with GHPath = Executable()
// and the server's Env(), so `gh api ...` invocations re-enter the test
// binary, which forwards the request to the server and prints the reply in
// the `gh api -i` format. No network, no real gh, no extra build step.
package ghfake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
)

// EnvURL names the variable that turns the test binary into the fake gh
// and tells it where the fixture server listens.
const EnvURL = "TEMPEST_FAKE_GH_URL"

// DefaultPerPage is GitHub's page size when a request names none.
const DefaultPerPage = 30

// RecordedRequest is one request the server received.
type RecordedRequest struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
}

// fault answers matching requests with a status instead of their route.
type fault struct {
	method, path string
	status       int
	remaining    int
	header       http.Header
}

// Server is a programmable GitHub REST API. Routes use net/http patterns
// such as "GET /repos/{owner}/{repo}/issues/{number}/comments".
type Server struct {
	mux      *http.ServeMux
	listener *httptest.Server
	mu       sync.Mutex
	requests []RecordedRequest
	faults   []*fault
}

// NewServer starts a server that the test closes on cleanup.
func NewServer(t testing.TB) *Server {
	t.Helper()
	server := &Server{mux: http.NewServeMux()}
	server.listener = httptest.NewServer(http.HandlerFunc(server.serve))
	t.Cleanup(server.listener.Close)
	return server
}

// HandleFunc registers a route. Unregistered paths answer 404 like GitHub.
func (s *Server) HandleFunc(pattern string, handler http.HandlerFunc) {
	s.mux.HandleFunc(pattern, handler)
}

// Env is the environment the gh client must pass to the fake gh.
func (s *Server) Env() []string { return []string{EnvURL + "=" + s.listener.URL} }

// URL is the server's base URL.
func (s *Server) URL() string { return s.listener.URL }

// Fail answers the next times requests for method and path with status,
// adding header (for example Retry-After). Path excludes the query.
func (s *Server) Fail(method, path string, status, times int, header http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, &fault{method: method, path: path, status: status, remaining: times, header: header})
}

// Requests returns every request received so far, in arrival order.
func (s *Server) Requests() []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RecordedRequest(nil), s.requests...)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.requests = append(s.requests, RecordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone(), Body: body})
	injected := s.takeFault(r.Method, r.URL.Path)
	s.mu.Unlock()
	if injected != nil {
		for key, values := range injected.header {
			w.Header()[key] = values
		}
		WriteJSON(w, injected.status, map[string]string{"message": http.StatusText(injected.status)})
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if _, pattern := s.mux.Handler(r); pattern == "" {
		WriteJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	s.mux.ServeHTTP(w, r)
}

// takeFault must be called with s.mu held.
func (s *Server) takeFault(method, path string) *fault {
	for _, candidate := range s.faults {
		if candidate.remaining > 0 && candidate.method == method && candidate.path == path {
			candidate.remaining--
			return candidate
		}
	}
	return nil
}

// WriteJSON writes value as a JSON reply with GitHub's content type.
func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		fmt.Fprintf(os.Stderr, "ghfake: encode reply: %v\n", err)
	}
}

// WritePage writes the page of rows that r's page and per_page parameters
// select, with a GitHub Link header pointing at the next page.
func WritePage[T any](w http.ResponseWriter, r *http.Request, rows []T) {
	query := r.URL.Query()
	perPage := DefaultPerPage
	if text := query.Get("per_page"); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil || parsed < 1 {
			WriteJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid per_page"})
			return
		}
		perPage = parsed
	}
	page := 1
	if text := query.Get("page"); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil || parsed < 1 {
			WriteJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid page"})
			return
		}
		page = parsed
	}
	start := min((page-1)*perPage, len(rows))
	end := min(start+perPage, len(rows))
	if end < len(rows) {
		next := *r.URL
		nextQuery := next.Query()
		nextQuery.Set("page", strconv.Itoa(page+1))
		nextQuery.Set("per_page", strconv.Itoa(perPage))
		next.RawQuery = nextQuery.Encode()
		w.Header().Set("Link", fmt.Sprintf("<https://api.github.com%s>; rel=\"next\"", next.RequestURI()))
	}
	WriteJSON(w, http.StatusOK, rows[start:end])
}
