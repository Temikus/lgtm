package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Temikus/lgtm/internal/config"
	"github.com/Temikus/lgtm/internal/ghtest"
	"github.com/Temikus/lgtm/internal/jev"
)

// judge scores every noul low when the diff says BAD, high otherwise, and
// names a dated snapshot of the requested model.
type judge struct{ snapshots []string }

func (j *judge) Decide(_ context.Context, req jev.Request) (*jev.Response, error) {
	st, _ := json.Marshal(req.State)
	snap := j.snapshots[0]
	if len(j.snapshots) > 1 && strings.Contains(string(st), "BAD") {
		snap = j.snapshots[1]
	}
	out := &jev.Response{Model: req.Model + "-" + snap, Answers: map[string]jev.Answer{}, CostUSD: 0.001}
	for id, q := range req.Questions {
		a := jev.Answer{Type: q.Type, Noul: 0.97}
		if strings.Contains(string(st), "BAD") {
			a.Noul = 0.04
		}
		switch q.Type {
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

func replayServer(t *testing.T) *ghtest.Server {
	t.Helper()
	var prs []fakePR
	for n := 1; n <= 12; n++ {
		p := fakePR{n: n, title: fmt.Sprintf("PR %d", n), author: "ann", closed: time.Duration(n) * time.Hour, lines: 20}
		if n <= 7 {
			p.merged, p.reviews = true, []map[string]any{review("bob", "APPROVED", 0)}
		} else {
			p.reviews = []map[string]any{review("bob", "CHANGES_REQUESTED", 0)}
		}
		prs = append(prs, p)
	}
	gh := serveHistory(t, prs, "ann", "bob")
	for _, p := range prs {
		patch := "@@ -1 +1 @@\n+fine()\n"
		if !p.merged {
			patch = "@@ -1 +1 @@\n+BAD()\n"
		}
		gh.Mux.HandleFunc(fmt.Sprintf("GET /repos/o/r/compare/base...head%d", p.n), ghtest.Reply(200, map[string]any{"files": []any{
			map[string]any{"filename": "x.go", "status": "modified", "additions": 1, "changes": 1, "patch": patch},
		}}))
		var runs []any
		if p.n <= 3 {
			runs = append(runs, map[string]any{"id": p.n, "head_sha": fmt.Sprintf("head%d", p.n), "external_id": "lgtm/v1 mode=shadow verdict=approve", "app": map[string]any{"slug": "github-actions"}})
		}
		gh.Mux.HandleFunc(fmt.Sprintf("GET /repos/o/r/commits/head%d/check-runs", p.n), ghtest.Reply(200, map[string]any{"total_count": len(runs), "check_runs": runs}))
	}
	return gh
}

func runOpts(t *testing.T, d jev.Decider) Options {
	t.Helper()
	cfg, err := config.Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		Owner: "o", Repo: "r", Config: cfg, Decider: d, BudgetUSD: 1, TargetPrecision: 0.95,
		History: HistoryOptions{Since: t0, Mode: Strict}, Perturb: true,
		Now: func() time.Time { return time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC) },
	}
}

func TestRun(t *testing.T) {
	gh := replayServer(t)
	rep, err := Run(context.Background(), gh.Client, runOpts(t, &judge{snapshots: []string{"20260917"}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Points) != 12 || len(rep.Failed) != 0 || rep.BudgetHit {
		t.Fatalf("points %d, failed %v, budget %v", len(rep.Points), rep.Failed, rep.BudgetHit)
	}
	c := rep.Calibration
	if c == nil {
		t.Fatalf("no calibration: %s", rep.WhyNot)
	}
	if c.Snapshot != "typesafe/jev-1.13-20260917" || c.ApproveAt != 0.97 || c.DenyAt != 0.04 || c.NLabelled != 12 || c.Date.String() != "2026-10-16" {
		t.Errorf("calibration = %+v", c)
	}
	if rep.Fitted == nil || rep.Fitted.ApprovePrecision != 1 || rep.Fitted.DenyRecall != 1 {
		t.Errorf("fitted = %+v", rep.Fitted)
	}
	if rep.Shadow == nil || rep.Shadow.N != 3 || rep.Shadow.Agreed != 3 {
		t.Errorf("shadow = %+v", rep.Shadow)
	}
	if rep.Baseline == nil || rep.Baseline.N == 0 || len(rep.Edits) != len(Edits) {
		t.Errorf("perturb baseline = %+v", rep.Baseline)
	}
	if len(rep.Probe) == 0 || rep.CostUSD <= 0 {
		t.Errorf("probe %d, cost %v", len(rep.Probe), rep.CostUSD)
	}
	text := rep.Text(0.95)
	for _, want := range []string{"approve_at 0.97", "Shadow mode: 3 labelled PRs", "same PR scored twice", "Content-free probe"} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
}

func TestRun_TwoSnapshotsWriteNothing(t *testing.T) {
	gh := replayServer(t)
	rep, err := Run(context.Background(), gh.Client, runOpts(t, &judge{snapshots: []string{"20260917", "20261101"}}))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Calibration != nil || !strings.Contains(rep.WhyNot, "2 model versions") {
		t.Errorf("calibration = %+v, why = %q", rep.Calibration, rep.WhyNot)
	}
}

func TestRun_StopsAtBudget(t *testing.T) {
	gh := replayServer(t)
	o := runOpts(t, &judge{snapshots: []string{"20260917"}})
	o.BudgetUSD, o.Concurrency = 0.0045, 1 // two calls per PR at $0.001
	rep, err := Run(context.Background(), gh.Client, o)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.BudgetHit || len(rep.Points) != 2 || rep.CostUSD > o.BudgetUSD {
		t.Errorf("budget hit %v, points %d, cost %v", rep.BudgetHit, len(rep.Points), rep.CostUSD)
	}
	if !strings.Contains(rep.Text(0.95), "Stopped at the budget") {
		t.Error("text should say the budget stopped the run")
	}
}

func TestEstimateCost(t *testing.T) {
	gh := replayServer(t)
	o := runOpts(t, nil)
	ss, err := History(context.Background(), gh.Client, "o", "r", o.History)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := EstimateCost(context.Background(), gh.Client, o.Config, ss, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	// 12 PRs at 2 calls each (full and diff context), plus a 2-PR probe.
	if plain.PRs != 12 || plain.Calls != 28 || plain.Tokens <= 0 || plain.USD <= 0 {
		t.Errorf("estimate = %+v", plain)
	}
	withPerturb, err := EstimateCost(context.Background(), gh.Client, o.Config, ss, true, 10)
	if err != nil {
		t.Fatal(err)
	}
	if withPerturb.Calls != plain.Calls+perturbScorings*10*2 {
		t.Errorf("with perturb = %+v", withPerturb)
	}
	if !strings.Contains(withPerturb.Text(0.000001), "more than --budget-usd") {
		t.Error("text should warn when the estimate passes the budget")
	}
}
