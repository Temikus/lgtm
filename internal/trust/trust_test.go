package trust

import (
	"context"
	"net/http"
	"testing"

	"github.com/Temikus/lgtm/internal/ghtest"
	"github.com/Temikus/lgtm/internal/prstate"
)

// perms serves the collaborator permission endpoint from a table.
// role_name is what GitHub returns for custom and maintain/triage roles.
func perms(gh *ghtest.Server, table map[string][2]string) {
	gh.Mux.HandleFunc("GET /repos/o/r/collaborators/{user}/permission", func(w http.ResponseWriter, r *http.Request) {
		p, ok := table[r.PathValue("user")]
		if !ok {
			ghtest.NotFound(w, r)
			return
		}
		ghtest.JSON(w, 200, map[string]any{"permission": p[0], "role_name": p[1]})
	})
}

func TestAuthorTrusted(t *testing.T) {
	gh := ghtest.New(t)
	perms(gh, map[string][2]string{
		"admin":       {"admin", "admin"},
		"maintainer":  {"write", "maintain"},
		"writer":      {"write", "write"},
		"triager":     {"read", "triage"},
		"invitee":     {"read", "read"}, // read-only COLLABORATOR
		"contributor": {"read", "read"}, // CONTRIBUTOR on a public repo
	})
	want := map[string]bool{"admin": true, "maintainer": true, "writer": true, "triager": false, "invitee": false, "contributor": false, "stranger": false}
	for user, w := range want {
		got, err := AuthorTrusted(context.Background(), gh.Client, "o", "r", user)
		if user == "stranger" && err != nil {
			t.Errorf("a 404 should mean untrusted, not an error: %v", err)
		}
		if got != w {
			t.Errorf("%s trusted = %v, want %v", user, got, w)
		}
	}
}

func TestAuthorTrusted_ErrorIsUntrusted(t *testing.T) {
	gh := ghtest.New(t)
	gh.Mux.HandleFunc("GET /repos/o/r/collaborators/{user}/permission", ghtest.Reply(500, map[string]string{"message": "boom"}))

	got, err := AuthorTrusted(context.Background(), gh.Client, "o", "r", "writer")
	if got || err == nil {
		t.Errorf("trusted = %v err = %v; a failed lookup must be untrusted and reported", got, err)
	}
}

func review(user, state, commit string) map[string]any {
	return map[string]any{"user": map[string]any{"login": user}, "state": state, "commit_id": commit}
}

func TestHumanApproval(t *testing.T) {
	pr := prstate.PR{Owner: "o", Repo: "r", Number: 7, HeadSHA: "head111", Author: "author"}
	table := map[string][2]string{"writer": {"write", "write"}, "reader": {"read", "read"}, "author": {"write", "write"}}

	tests := []struct {
		name    string
		reviews []any
		want    bool
	}{
		{"approval on head by writer", []any{review("writer", "APPROVED", "head111")}, true},
		{"approval by read-only user", []any{review("reader", "APPROVED", "head111")}, false},
		{"dismissed approval", []any{review("writer", "DISMISSED", "head111")}, false},
		{"approval on stale commit", []any{review("writer", "APPROVED", "old000")}, false},
		{"later changes requested", []any{review("writer", "APPROVED", "head111"), review("writer", "CHANGES_REQUESTED", "head111")}, false},
		{"comment after approval keeps it", []any{review("writer", "APPROVED", "head111"), review("writer", "COMMENTED", "head111")}, true},
		{"self approval", []any{review("author", "APPROVED", "head111")}, false},
		{"no reviews", []any{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := ghtest.New(t)
			perms(gh, table)
			gh.Mux.HandleFunc("GET /repos/o/r/pulls/7/reviews", ghtest.Reply(200, tt.reviews))

			got, by, err := HumanApproval(context.Background(), gh.Client, pr)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("approved = %v (by %q), want %v", got, by, tt.want)
			}
		})
	}
}
