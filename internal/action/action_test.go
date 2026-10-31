package action

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Temikus/lgtm/internal/config"
	"github.com/Temikus/lgtm/internal/ghout"
	"github.com/Temikus/lgtm/internal/ghtest"
	"github.com/Temikus/lgtm/internal/jev"
)

type fakeDecider struct {
	mu    sync.Mutex
	calls int
	noul  float64
}

func (f *fakeDecider) Decide(_ context.Context, req jev.Request) (*jev.Response, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	out := &jev.Response{Model: req.Model, Answers: map[string]jev.Answer{}}
	for id, q := range req.Questions {
		a := jev.Answer{Type: q.Type, Noul: f.noul}
		switch q.Type {
		case jev.Choice:
			a.Choice = q.Choices[0].Name
		case jev.Score:
			a.Score = float64(len(q.Levels) - 1)
		}
		out.Answers[id] = a
	}
	return out, nil
}

// world is a fake repo o/r with PR 7 (base000...head111).
type world struct {
	*ghtest.Server
	mu     sync.Mutex
	writes []string
	bodies map[string]map[string]any
	cfgRef string
	checks []any
	// checksDown makes check-run reads fail.
	checksDown bool
	reviews    []any
	perms      map[string]string
}

func newWorld(t *testing.T, cfgTOML string) *world {
	t.Helper()
	w := &world{Server: ghtest.New(t), bodies: map[string]map[string]any{}, perms: map[string]string{}}
	m := w.Mux
	m.HandleFunc("GET /repos/o/r/contents/.github/lgtm.toml", func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		w.cfgRef = r.URL.Query().Get("ref")
		w.mu.Unlock()
		if cfgTOML == "" {
			ghtest.NotFound(rw, r)
			return
		}
		ghtest.JSON(rw, 200, map[string]any{"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(cfgTOML))})
	})
	m.HandleFunc("GET /repos/o/r/compare/base000...head111", ghtest.Reply(200, map[string]any{"files": []any{
		map[string]any{"filename": "a.go", "status": "modified", "additions": 1, "changes": 1, "patch": "+x"},
	}}))
	m.HandleFunc("GET /repos/o/r/issues/7/comments", ghtest.Reply(200, []any{}))
	m.HandleFunc("GET /repos/o/r/commits/head111/check-runs", func(rw http.ResponseWriter, _ *http.Request) {
		if w.checksDown {
			ghtest.JSON(rw, 500, map[string]any{"message": "boom"})
			return
		}
		ghtest.JSON(rw, 200, map[string]any{"check_runs": w.checks})
	})
	m.HandleFunc("GET /repos/o/r/pulls/7/reviews", func(rw http.ResponseWriter, _ *http.Request) { ghtest.JSON(rw, 200, w.reviews) })
	m.HandleFunc("GET /repos/o/r/collaborators/{user}/permission", func(rw http.ResponseWriter, r *http.Request) {
		p := w.perms[r.PathValue("user")]
		ghtest.JSON(rw, 200, map[string]any{"permission": p, "role_name": p})
	})
	m.HandleFunc("GET /repos/o/r/pulls", ghtest.Reply(200, []any{prJSON(false)}))
	for _, route := range []string{"POST /repos/o/r/check-runs", "PATCH /repos/o/r/check-runs/{id}", "POST /repos/o/r/issues/7/comments", "POST /repos/o/r/issues/7/labels", "DELETE /repos/o/r/issues/7/labels/{name}"} {
		m.HandleFunc(route, w.record(route))
	}
	return w
}

func (w *world) record(route string) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		w.mu.Lock()
		w.writes = append(w.writes, route)
		w.bodies[route] = b
		w.mu.Unlock()
		if strings.Contains(route, "labels") {
			ghtest.JSON(rw, 200, []any{})
			return
		}
		ghtest.JSON(rw, 200, map[string]any{"id": 1})
	}
}

func (w *world) wrote(route string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range w.writes {
		if r == route {
			return true
		}
	}
	return false
}

func prJSON(fork bool) map[string]any {
	head := "o/r"
	if fork {
		head = "mallory/r"
	}
	return map[string]any{
		"number": 7, "title": "Add x", "body": "Adds x.", "changed_files": 1, "user": map[string]any{"login": "mallory"},
		"base": map[string]any{"sha": "base000", "ref": "main", "repo": map[string]any{"full_name": "o/r"}},
		"head": map[string]any{"sha": "head111", "ref": "x", "repo": map[string]any{"full_name": head}},
	}
}

func event(t *testing.T, v map[string]any) []byte {
	t.Helper()
	v["repository"] = map[string]any{"name": "r", "owner": map[string]any{"login": "o"}}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func env(w *world, name string, ev []byte, d jev.Decider) Env {
	return Env{
		EventName: name, Event: ev, GH: w.Client, CommentAuthor: ghout.ActionsBot, Log: &bytes.Buffer{},
		NewDecider: func(*config.Config) (jev.Decider, error) { return d, nil },
	}
}

const enforce = "mode = \"enforce\"\nallow_uncalibrated = true\n"

func TestRun_ScoresPRTargetInShadow(t *testing.T) {
	w := newWorld(t, "")
	d := &fakeDecider{noul: 0.99}
	outputs := map[string]string{}
	e := env(w, "pull_request_target", event(t, map[string]any{"action": "opened", "pull_request": prJSON(false)}), d)
	e.SetOutput = func(k, v string) { outputs[k] = v }

	code, err := Run(context.Background(), e)
	if err != nil || code != 0 {
		t.Fatalf("code = %d err = %v", code, err)
	}
	if w.cfgRef != "base000" {
		t.Errorf("config read at %q, want the base SHA", w.cfgRef)
	}
	if d.calls == 0 || !w.wrote("POST /repos/o/r/check-runs") || !w.wrote("POST /repos/o/r/issues/7/comments") {
		t.Errorf("calls = %d writes = %v", d.calls, w.writes)
	}
	if w.bodies["POST /repos/o/r/check-runs"]["conclusion"] != "neutral" {
		t.Error("shadow mode check must be neutral")
	}
	if outputs["verdict"] != "approve" {
		t.Errorf("outputs = %v", outputs)
	}
}

func TestRun_EnforceNeedsHumanFailsJob(t *testing.T) {
	w := newWorld(t, enforce)
	code, err := Run(context.Background(), env(w, "pull_request_target", event(t, map[string]any{"action": "synchronize", "pull_request": prJSON(false)}), &fakeDecider{noul: 0.5}))
	if err != nil || code != 1 {
		t.Fatalf("code = %d err = %v", code, err)
	}
	if !w.wrote("POST /repos/o/r/issues/7/labels") {
		t.Error("needs-human in enforce mode should add the label")
	}
}

func TestRun_MissingAPIKeyIsNeedsHuman(t *testing.T) {
	w := newWorld(t, enforce)
	e := env(w, "pull_request_target", event(t, map[string]any{"action": "opened", "pull_request": prJSON(false)}), nil)
	e.NewDecider = func(*config.Config) (jev.Decider, error) { return nil, errors.New("no API key") }

	code, err := Run(context.Background(), e)
	if err != nil || code != 1 {
		t.Fatalf("code = %d err = %v", code, err)
	}
	if w.bodies["POST /repos/o/r/check-runs"]["conclusion"] != "failure" {
		t.Error("a missing key must fail closed")
	}
}

func TestRun_IgnoresOtherActions(t *testing.T) {
	w := newWorld(t, "")
	d := &fakeDecider{}
	code, err := Run(context.Background(), env(w, "pull_request_target", event(t, map[string]any{"action": "labeled", "pull_request": prJSON(false)}), d))
	if err != nil || code != 0 || d.calls != 0 || len(w.writes) != 0 {
		t.Errorf("code = %d err = %v calls = %d writes = %v", code, err, d.calls, w.writes)
	}
}

func needsHumanCheck() map[string]any {
	return map[string]any{"id": 5, "name": ghout.CheckName, "head_sha": "head111", "conclusion": "failure",
		"external_id": "lgtm/v1 mode=enforce verdict=needs-human", "app": map[string]any{"slug": "github-actions"}}
}

func reviewEvent(t *testing.T, fork bool) []byte {
	return event(t, map[string]any{"action": "submitted", "pull_request": prJSON(fork), "review": map[string]any{"state": "approved"}})
}

func TestRun_ReviewFlipsNeedsHuman(t *testing.T) {
	w := newWorld(t, enforce)
	w.checks = []any{needsHumanCheck()}
	w.reviews = []any{map[string]any{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "commit_id": "head111"}}
	w.perms["alice"] = "write"
	d := &fakeDecider{}

	code, err := Run(context.Background(), env(w, "pull_request_review", reviewEvent(t, false), d))
	if err != nil || code != 0 {
		t.Fatalf("code = %d err = %v", code, err)
	}
	if d.calls != 0 {
		t.Error("the approval flip must not call the model")
	}
	if w.bodies["PATCH /repos/o/r/check-runs/{id}"]["conclusion"] != "success" {
		t.Errorf("writes = %v", w.writes)
	}
}

func TestRun_ReviewWithoutQualifyingApproval(t *testing.T) {
	w := newWorld(t, enforce)
	w.checks = []any{needsHumanCheck()}
	w.reviews = []any{map[string]any{"user": map[string]any{"login": "bob"}, "state": "APPROVED", "commit_id": "head111"}}
	w.perms["bob"] = "read"

	code, err := Run(context.Background(), env(w, "pull_request_review", reviewEvent(t, false), &fakeDecider{}))
	if err != nil || code != 1 {
		t.Fatalf("code = %d err = %v", code, err)
	}
	if w.wrote("PATCH /repos/o/r/check-runs/{id}") {
		t.Error("a read-only approval must not flip the check")
	}
}

func TestRun_ReviewWithoutCheckScores(t *testing.T) {
	w := newWorld(t, "")
	d := &fakeDecider{noul: 0.99}
	if _, err := Run(context.Background(), env(w, "pull_request_review", reviewEvent(t, false), d)); err != nil {
		t.Fatal(err)
	}
	if d.calls == 0 {
		t.Error("with no LGTM check on the SHA, a review event scores normally")
	}
}

func TestRun_ForkReviewReadOnlyToken(t *testing.T) {
	w := newWorld(t, enforce)
	w.checks = []any{needsHumanCheck()}
	w.reviews = []any{map[string]any{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "commit_id": "head111"}}
	w.perms["alice"] = "write"
	w.Mux.HandleFunc("PATCH /repos/o/r/check-runs/5", ghtest.Reply(403, map[string]string{"message": "Resource not accessible by integration"}))

	code, err := Run(context.Background(), env(w, "pull_request_review", reviewEvent(t, true), &fakeDecider{}))
	if err != nil || code != 0 {
		t.Errorf("code = %d err = %v; a qualifying approval passes even when the token can't flip the check", code, err)
	}
}

func TestRun_WorkflowRunFlipsForkApproval(t *testing.T) {
	w := newWorld(t, enforce)
	w.checks = []any{needsHumanCheck()}
	w.reviews = []any{map[string]any{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "commit_id": "head111"}}
	w.perms["alice"] = "write"

	ev := event(t, map[string]any{"action": "completed", "workflow_run": map[string]any{"event": "pull_request_review", "head_sha": "head111"}})
	code, err := Run(context.Background(), env(w, "workflow_run", ev, &fakeDecider{}))
	if err != nil || code != 0 {
		t.Fatalf("code = %d err = %v", code, err)
	}
	if !w.wrote("PATCH /repos/o/r/check-runs/{id}") {
		t.Error("workflow_run should flip the check")
	}
}

func TestRun_WorkflowRunWithoutCheckDoesNotScore(t *testing.T) {
	w := newWorld(t, enforce)
	d := &fakeDecider{}
	ev := event(t, map[string]any{"workflow_run": map[string]any{"event": "pull_request_review", "head_sha": "head111"}})
	if code, err := Run(context.Background(), env(w, "workflow_run", ev, d)); err != nil || code != 0 || d.calls != 0 || len(w.writes) != 0 {
		t.Errorf("code = %d err = %v calls = %d writes = %v", code, err, d.calls, w.writes)
	}
}

func TestRun_WorkflowRunFromOtherEventIgnored(t *testing.T) {
	w := newWorld(t, enforce)
	ev := event(t, map[string]any{"workflow_run": map[string]any{"event": "pull_request_target", "head_sha": "head111"}})
	if code, err := Run(context.Background(), env(w, "workflow_run", ev, &fakeDecider{})); err != nil || code != 0 || len(w.writes) != 0 {
		t.Errorf("code = %d err = %v writes = %v", code, err, w.writes)
	}
}

func TestRun_RescoreKeepsApprovalOfSameCommit(t *testing.T) {
	w := newWorld(t, enforce)
	w.reviews = []any{map[string]any{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "commit_id": "head111"}}
	w.perms["alice"] = "write"

	code, err := Run(context.Background(), env(w, "pull_request_target", event(t, map[string]any{"action": "edited", "pull_request": prJSON(false)}), &fakeDecider{noul: 0.5}))
	if err != nil || code != 0 {
		t.Fatalf("code = %d err = %v", code, err)
	}
	check := w.bodies["POST /repos/o/r/check-runs"]
	if check["conclusion"] != "success" || !strings.Contains(fmt.Sprint(check["external_id"]), "verdict=approve") {
		t.Errorf("check = %v", check)
	}
	if w.wrote("POST /repos/o/r/issues/7/labels") || !w.wrote("DELETE /repos/o/r/issues/7/labels/{name}") {
		t.Errorf("writes = %v; an approved PR must not carry the needs-human label", w.writes)
	}
}

func TestRun_RescoreIgnoresApprovalOfOtherCommit(t *testing.T) {
	w := newWorld(t, enforce)
	w.reviews = []any{map[string]any{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "commit_id": "old000"}}
	w.perms["alice"] = "write"

	code, err := Run(context.Background(), env(w, "pull_request_target", event(t, map[string]any{"action": "synchronize", "pull_request": prJSON(false)}), &fakeDecider{noul: 0.5}))
	if err != nil || code != 1 {
		t.Fatalf("code = %d err = %v", code, err)
	}
	if w.bodies["POST /repos/o/r/check-runs"]["conclusion"] != "failure" {
		t.Error("an approval of an earlier commit must not carry over")
	}
}

func TestRun_ApprovalDoesNotOverrideDeny(t *testing.T) {
	w := newWorld(t, enforce)
	w.reviews = []any{map[string]any{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "commit_id": "head111"}}
	w.perms["alice"] = "write"

	code, err := Run(context.Background(), env(w, "pull_request_target", event(t, map[string]any{"action": "edited", "pull_request": prJSON(false)}), &fakeDecider{noul: 0.01}))
	if err != nil || code != 1 {
		t.Fatalf("code = %d err = %v", code, err)
	}
	if w.bodies["POST /repos/o/r/check-runs"]["conclusion"] != "failure" {
		t.Error("a deny stays a deny after an approval")
	}
}

func TestRun_RescoreFloor(t *testing.T) {
	check := func(verdict, extra string) map[string]any {
		c := needsHumanCheck()
		c["external_id"] = "lgtm/v1 mode=enforce verdict=" + verdict + extra
		return c
	}
	tests := []struct {
		name    string
		cfg     string
		earlier map[string]any
		want    string
	}{
		{"first score", "", nil, "approve"},
		{"earlier approve", "", check("approve", ""), "approve"},
		{"earlier needs-human", "", check("needs-human", ""), "needs-human"},
		{"earlier deny", "", check("deny", ""), "deny"},
		{"earlier human approval", "", check("approve", " human=1"), "needs-human"},
		{"no_approve after deny", "rescore = \"no_approve\"\n", check("deny", ""), "needs-human"},
		{"latest after deny", "rescore = \"latest\"\n", check("deny", ""), "approve"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, enforce+tt.cfg)
			if tt.earlier != nil {
				w.checks = []any{tt.earlier}
			}
			ev := event(t, map[string]any{"action": "edited", "pull_request": prJSON(false)})
			if _, err := Run(context.Background(), env(w, "pull_request_target", ev, &fakeDecider{noul: 0.99})); err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(w.bodies["POST /repos/o/r/check-runs"]["external_id"]); !strings.HasSuffix(got, "verdict="+tt.want) {
				t.Errorf("external_id = %q, want verdict=%s", got, tt.want)
			}
		})
	}
}

func TestRun_RescoreLookupFailurePublishesNothing(t *testing.T) {
	w := newWorld(t, enforce)
	w.checksDown = true
	ev := event(t, map[string]any{"action": "edited", "pull_request": prJSON(false)})
	if code, err := Run(context.Background(), env(w, "pull_request_target", ev, &fakeDecider{noul: 0.99})); err == nil || code != 1 || len(w.writes) != 0 {
		t.Errorf("code = %d err = %v writes = %v", code, err, w.writes)
	}
}

// A blind-arm PR records its shadow verdict on the check run and shows it
// nowhere a reviewer looks.
func TestRun_BlindArmHidesVerdict(t *testing.T) {
	w := newWorld(t, "shadow_blind_fraction = 1.0\n")
	var log bytes.Buffer
	outputs := map[string]string{}
	e := env(w, "pull_request_target", event(t, map[string]any{"action": "opened", "pull_request": prJSON(false)}), &fakeDecider{noul: 0.01})
	e.Log = &log
	e.SetOutput = func(k, v string) { outputs[k] = v }
	if code, err := Run(context.Background(), e); err != nil || code != 0 {
		t.Fatalf("code = %d err = %v", code, err)
	}
	check := w.bodies["POST /repos/o/r/check-runs"]
	if ext := fmt.Sprint(check["external_id"]); ext != "lgtm/v1 mode=shadow verdict=deny blind=1" {
		t.Errorf("external_id = %q", ext)
	}
	public := fmt.Sprint(check["output"], w.bodies["POST /repos/o/r/issues/7/comments"]["body"], log.String())
	if strings.Contains(public, "deny") || len(outputs) != 0 {
		t.Errorf("verdict leaked: outputs = %v\n%s", outputs, public)
	}
}
