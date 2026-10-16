// Package ghtest serves a fake GitHub REST API over httptest for tests.
// Handlers return recorded response shapes, so go-github's real request and
// decoding paths run.
package ghtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v92/github"
)

// Server is a fake GitHub API. Register handlers on Mux with paths like
// "GET /repos/o/r/pulls/1".
type Server struct {
	Mux    *http.ServeMux
	Client *github.Client
}

// New starts a server and returns a client pointed at it.
func New(t *testing.T) *Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base := srv.URL + "/"
	c, err := github.NewClient(github.WithURLs(&base, &base), github.WithDisableRateLimitCheck())
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Mux: mux, Client: c}
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
