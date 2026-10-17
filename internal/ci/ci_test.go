package ci

import (
	"context"
	"slices"
	"testing"

	"github.com/Temikus/lgtm/internal/ghtest"
)

func serve(t *testing.T, rules []any) *ghtest.Server {
	t.Helper()
	gh := ghtest.New(t)
	gh.Mux.HandleFunc("GET /repos/o/r/rules/branches/main", ghtest.Reply(200, rules))
	gh.Mux.HandleFunc("GET /repos/o/r/commits/h/check-runs", ghtest.Reply(200, map[string]any{"total_count": 4, "check_runs": []any{
		map[string]any{"name": "test", "status": "completed", "conclusion": "failure"},
		map[string]any{"name": "lint", "status": "completed", "conclusion": "failure"}, // not required
		map[string]any{"name": "e2e", "status": "in_progress"},                         // still running
		map[string]any{"name": "LGTM", "status": "completed", "conclusion": "failure"}, // LGTM's own
	}}))
	gh.Mux.HandleFunc("GET /repos/o/r/commits/h/status", ghtest.Reply(200, map[string]any{"state": "failure", "statuses": []any{
		map[string]any{"context": "ci/build", "state": "error"},
		map[string]any{"context": "codecov", "state": "failure"}, // not required
	}}))
	return gh
}

func required(contexts ...string) []any {
	var cs []any
	for _, c := range contexts {
		cs = append(cs, map[string]any{"context": c})
	}
	return []any{map[string]any{"type": "required_status_checks", "parameters": map[string]any{"required_status_checks": cs}}}
}

func TestFailed(t *testing.T) {
	gh := serve(t, required("test", "e2e", "LGTM"))
	got, err := Failed(context.Background(), gh.Client, "o", "r", "main", "h", []string{"ci/build"}, "LGTM")
	if err != nil || !slices.Equal(got, []string{"test", "ci/build"}) {
		t.Errorf("Failed = %v, %v", got, err)
	}
}

// With nothing required, LGTM can't tell a flaky optional check from a
// real failure, so nothing counts.
func TestFailed_NothingRequired(t *testing.T) {
	gh := serve(t, []any{})
	if got, err := Failed(context.Background(), gh.Client, "o", "r", "main", "h", nil, "LGTM"); err != nil || len(got) != 0 {
		t.Errorf("Failed = %v, %v", got, err)
	}
}
