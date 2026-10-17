package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/Temikus/lgtm/internal/config"
	"github.com/Temikus/lgtm/internal/ghtest"
	"github.com/Temikus/lgtm/internal/jev"
	"github.com/Temikus/lgtm/internal/prstate"
	"github.com/Temikus/lgtm/internal/verdict"
)

// fakeDecider answers like a provider: every noul gets p[id] (default 0.99),
// every choice its first option sent (a maximally position-biased model),
// every score its top level.
type fakeDecider struct {
	mu   sync.Mutex
	p    map[string]float64
	err  error
	reqs []jev.Request
	// served is the model the reply names; empty echoes the request.
	served string
}

func (f *fakeDecider) Decide(_ context.Context, req jev.Request) (*jev.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	model := req.Model
	if f.served != "" {
		model = f.served
	}
	out := &jev.Response{Model: model, Answers: map[string]jev.Answer{}, CostUSD: 0.0001, Usage: jev.Usage{InputTokens: 100}}
	for id, q := range req.Questions {
		a := jev.Answer{Type: q.Type}
		switch q.Type {
		case jev.Noul:
			a.Noul = 0.99
			if v, ok := f.p[id]; ok {
				a.Noul = v
			}
		case jev.Choice:
			a.Choice = q.Choices[0].Name
			a.Probabilities = map[string]float64{a.Choice: 1}
		case jev.Score:
			a.Score = float64(len(q.Levels) - 1)
		}
		out.Answers[id] = a
	}
	return out, nil
}

func fakeGitHub(t *testing.T, fork bool, authorPerm string) (*ghtest.Server, prstate.PR) {
	t.Helper()
	gh := ghtest.New(t)
	gh.Mux.HandleFunc("GET /repos/o/r/compare/b...h", ghtest.Reply(200, map[string]any{"files": []any{
		map[string]any{"filename": "retry.go", "status": "modified", "additions": 5, "deletions": 1, "changes": 6, "patch": "+retry()"},
		map[string]any{"filename": "retry_test.go", "status": "added", "additions": 9, "changes": 9, "patch": "+func TestRetry"},
	}}))
	gh.Mux.HandleFunc("GET /repos/o/r/collaborators/mallory/permission", ghtest.Reply(200, map[string]any{"permission": authorPerm, "role_name": authorPerm}))
	return gh, prstate.PR{Owner: "o", Repo: "r", Number: 1, BaseSHA: "b", HeadSHA: "h", Title: "Add retry", Body: "Retries 429s.", Author: "mallory", Fork: fork, ChangedFiles: 2}
}

func load(t *testing.T, toml string) *config.Config {
	t.Helper()
	c, err := config.Load([]byte(toml))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestScore_CleanPRApproves(t *testing.T) {
	gh, pr := fakeGitHub(t, false, "write")
	d := &fakeDecider{}

	r := Score(context.Background(), gh.Client, pr, Options{Config: load(t, ""), Decider: d})
	if r.Verdict.Verdict != verdict.Approve {
		t.Fatalf("verdict = %+v", r.Verdict)
	}
	if len(d.reqs) != 2 || r.Calls != 2 {
		t.Errorf("calls = %d/%d, want one per context", len(d.reqs), r.Calls)
	}
	for _, req := range d.reqs {
		st, ok := req.State.(prstate.State)
		if !ok {
			t.Fatalf("state type %T", req.State)
		}
		_, hasSafe := req.Questions["malicious"]
		if hasSafe && st.PR != nil {
			t.Error("the diff-context call must not carry PR prose")
		}
	}
	if r.CostUSD == 0 || r.Usage.InputTokens != 200 {
		t.Errorf("usage = %+v cost = %v", r.Usage, r.CostUSD)
	}
}

func TestScore_LowGateDenies(t *testing.T) {
	gh, pr := fakeGitHub(t, false, "write")
	r := Score(context.Background(), gh.Client, pr, Options{Config: load(t, ""), Decider: &fakeDecider{p: map[string]float64{"malicious": 0.02}}})
	if r.Verdict.Verdict != verdict.Deny {
		t.Errorf("verdict = %+v", r.Verdict)
	}
}

func TestScore_UntrustedForkCapped(t *testing.T) {
	gh, pr := fakeGitHub(t, true, "read")
	r := Score(context.Background(), gh.Client, pr, Options{Config: load(t, ""), Decider: &fakeDecider{}})
	if !r.Untrusted || r.Verdict.Verdict != verdict.NeedsHuman {
		t.Errorf("untrusted = %v verdict = %+v", r.Untrusted, r.Verdict)
	}
}

func TestScore_TrustedForkOrTrustForks(t *testing.T) {
	gh, pr := fakeGitHub(t, true, "write")
	if r := Score(context.Background(), gh.Client, pr, Options{Config: load(t, ""), Decider: &fakeDecider{}}); r.Untrusted {
		t.Error("a fork from a writer is trusted")
	}
	gh, pr = fakeGitHub(t, true, "read")
	if r := Score(context.Background(), gh.Client, pr, Options{Config: load(t, "trust_forks = true"), Decider: &fakeDecider{}}); r.Untrusted {
		t.Error("trust_forks lifts the cap")
	}
}

func TestScore_ProviderErrorIsNeedsHuman(t *testing.T) {
	gh, pr := fakeGitHub(t, false, "write")
	r := Score(context.Background(), gh.Client, pr, Options{Config: load(t, ""), Decider: &fakeDecider{err: &jev.APIError{Status: 503, Message: "down"}}})
	if r.Verdict.Verdict != verdict.NeedsHuman || r.Verdict.Failure == nil {
		t.Errorf("verdict = %+v", r.Verdict)
	}
}

func TestScore_FetchErrorIsNeedsHuman(t *testing.T) {
	gh := ghtest.New(t)
	gh.Mux.HandleFunc("GET /repos/o/r/compare/b...h", ghtest.Reply(200, map[string]any{"files": []any{
		map[string]any{"filename": "go.sum", "status": "modified", "additions": 1, "changes": 1, "patch": "+x"},
	}}))
	pr := prstate.PR{Owner: "o", Repo: "r", Number: 1, BaseSHA: "b", HeadSHA: "h", ChangedFiles: 1}

	r := Score(context.Background(), gh.Client, pr, Options{Config: load(t, ""), Decider: &fakeDecider{}})
	if r.Verdict.Verdict != verdict.NeedsHuman || !errors.Is(r.Verdict.Failure, prstate.ErrNoFiles) {
		t.Errorf("verdict = %+v", r.Verdict)
	}
	if len(r.Excluded) != 1 {
		t.Error("the report should still list what was excluded")
	}
}

// A position-biased model flips its choice when the options are reversed,
// so a borderline approve must be capped.
func TestScore_BorderlineConsistencyCap(t *testing.T) {
	cfg := load(t, `
[questions.lang]
type = "choice"
gate = true
instructions = "Is the diff in one language?"
options = [{name = "single", definition = "One language"}, {name = "mixed", definition = "Several languages"}]
pass = ["single"]
`)
	gh, pr := fakeGitHub(t, false, "write")
	d := &fakeDecider{p: map[string]float64{"aligned": 0.92}}

	r := Score(context.Background(), gh.Client, pr, Options{Config: cfg, Decider: d})
	if r.Verdict.Verdict != verdict.NeedsHuman || len(r.Verdict.Caps) != 1 || r.Verdict.Caps[0] != verdict.Borderline {
		t.Fatalf("verdict = %+v", r.Verdict)
	}
	if r.Calls != 3 {
		t.Errorf("calls = %d, want 2 + 1 reversal", r.Calls)
	}

	// Well above the margin, no re-ask happens.
	d = &fakeDecider{}
	r = Score(context.Background(), gh.Client, pr, Options{Config: cfg, Decider: d})
	if r.Verdict.Verdict != verdict.Approve || r.Calls != 2 {
		t.Errorf("verdict = %+v calls = %d", r.Verdict, r.Calls)
	}
}

// A calibration only holds for the dated version it was fitted on.
func TestScore_ModelChangedSinceCalibration(t *testing.T) {
	cfg := load(t, `
mode = "enforce"
[calibration]
model = "typesafe/jev-1.13"
snapshot = "typesafe/jev-1.13-20260917"
n_labelled = 50
approve_at = 0.9
deny_at = 0.1
`)
	gh, pr := fakeGitHub(t, false, "write")

	r := Score(context.Background(), gh.Client, pr, Options{Config: cfg, Decider: &fakeDecider{served: "typesafe/jev-1.13-20260917"}})
	if r.Verdict.Verdict != verdict.Approve {
		t.Fatalf("same snapshot: verdict = %+v", r.Verdict)
	}
	if len(r.Served) != 1 || r.Served[0] != "typesafe/jev-1.13-20260917" {
		t.Errorf("served = %v", r.Served)
	}

	r = Score(context.Background(), gh.Client, pr, Options{Config: cfg, Decider: &fakeDecider{served: "typesafe/jev-1.13-20261101"}})
	if r.Verdict.Verdict != verdict.NeedsHuman || len(r.Verdict.Caps) != 1 || r.Verdict.Caps[0] != verdict.ModelChanged {
		t.Fatalf("new snapshot: verdict = %+v", r.Verdict)
	}

	// Without a recorded snapshot there is nothing to compare.
	cfg.Calibration.Snapshot = ""
	r = Score(context.Background(), gh.Client, pr, Options{Config: cfg, Decider: &fakeDecider{served: "typesafe/jev-1.13-20261101"}})
	if r.Verdict.Verdict != verdict.Approve {
		t.Fatalf("no snapshot: verdict = %+v", r.Verdict)
	}
}

func TestScore_LockfileSourceHolds(t *testing.T) {
	raw := &prstate.Raw{
		PR: prstate.PR{Owner: "o", Repo: "r", Number: 1, BaseSHA: "b", HeadSHA: "h", Title: "Bump deps", Body: "Routine bump.", Author: "ann"},
		Files: []prstate.File{
			{Path: "main.go", Status: "modified", Additions: 1, Changes: 1, Patch: "+package main"},
			{Path: "package-lock.json", Status: "modified", Additions: 1, Changes: 1, Patch: `+      "resolved": "https://npm.corp.example/a-1.0.0.tgz",`},
		},
	}
	snap, err := raw.Snapshot(config.DefaultExclude)
	if err != nil {
		t.Fatal(err)
	}
	r := ScoreSnapshot(context.Background(), nil, snap, Options{Config: load(t, ""), Decider: &fakeDecider{}})
	if r.Verdict.Verdict != verdict.NeedsHuman || !slices.Contains(r.Verdict.Caps, verdict.LockfileSource) || len(r.LockSources) != 1 {
		t.Errorf("verdict %+v, lock sources %+v", r.Verdict, r.LockSources)
	}
	cfg := load(t, `lockfile_registries = ["npm.corp.example"]`)
	if r := ScoreSnapshot(context.Background(), nil, snap, Options{Config: cfg, Decider: &fakeDecider{}}); r.Verdict.Verdict != verdict.Approve {
		t.Errorf("an allowed registry still held: %+v", r.Verdict)
	}
}
