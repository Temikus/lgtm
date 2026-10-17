// Package ghtest serves a fake GitHub REST API over httptest for tests.
// Handlers return recorded response shapes, so go-github's real request and
// decoding paths run.
package ghtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/google/go-github/v92/github"
)

// Server is a fake GitHub API. Register handlers on Mux with paths like
// "GET /repos/o/r/pulls/1".
type Server struct {
	Mux    *http.ServeMux
	Client *github.Client

	mu       sync.Mutex
	requests []string
}

// Requests returns "METHOD /path" for each request served so far.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// Reset forgets the requests served so far.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

// New starts a server and returns a client pointed at it.
func New(t *testing.T) *Server {
	t.Helper()
	mux := http.NewServeMux()
	s := &Server{Mux: mux}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	base := srv.URL + "/"
	c, err := github.NewClient(github.WithURLs(&base, &base), github.WithDisableRateLimitCheck())
	if err != nil {
		t.Fatal(err)
	}
	s.Client = c
	return s
}

// JSON writes v as a JSON response.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Reply returns a handler that always writes v.
func Reply(status int, v any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { JSON(w, status, v) }
}

// NotFound is GitHub's 404 body.
var NotFound = Reply(http.StatusNotFound, map[string]string{"message": "Not Found"})
