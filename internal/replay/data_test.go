package replay

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Temikus/lgtm/internal/dataset"
	"github.com/Temikus/lgtm/internal/ghtest"
	"github.com/Temikus/lgtm/internal/jev"
	"github.com/Temikus/lgtm/internal/prstate"
)

type countCalls struct {
	jev.Decider
	n atomic.Int64
}

func (c *countCalls) Decide(ctx context.Context, req jev.Request) (*jev.Response, error) {
	c.n.Add(1)
	return c.Decider.Decide(ctx, req)
}

func storeAt(t *testing.T, dir string) *dataset.Store {
	t.Helper()
	s, err := dataset.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// perPR are the requests replay makes for each PR it labels and scores.
func perPR(reqs []string) []string {
	var out []string
	for _, r := range reqs {
		if strings.Contains(r, "/pulls/") || strings.Contains(r, "/compare/") || strings.Contains(r, "/issues/") || strings.Contains(r, "/permission") {
			out = append(out, r)
		}
	}
	return out
}

func TestRun_SecondRunReadsTheDataset(t *testing.T) {
	gh := replayServer(t)
	dir := t.TempDir()
	model := &countCalls{Decider: &judge{snapshots: []string{"20260917"}}}
	o := runOpts(t, model)
	o.Perturb = false
	o.Data = Data{Store: storeAt(t, dir)}
	first, err := Run(context.Background(), gh.Client, o)
	if err != nil {
		t.Fatal(err)
	}
	calls := model.n.Load()
	if len(perPR(gh.Requests())) == 0 {
		t.Fatal("first run made no per-PR requests, so the check below proves nothing")
	}

	gh.Reset()
	o.Data = Data{Store: storeAt(t, dir)}
	second, err := Run(context.Background(), gh.Client, o)
	if err != nil {
		t.Fatal(err)
	}
	if got := perPR(gh.Requests()); len(got) != 0 {
		t.Errorf("second run re-read PRs: %v", got)
	}
	if model.n.Load() != calls || second.CostUSD != 0 {
		t.Errorf("second run asked the model: %d calls after %d, cost %v", model.n.Load(), calls, second.CostUSD)
	}
	if len(second.Points) != len(first.Points) || second.Calibration == nil || *second.Calibration != *first.Calibration {
		t.Errorf("second run differs: %d points, calibration %+v vs %+v", len(second.Points), second.Calibration, first.Calibration)
	}

	// Offline makes no GitHub requests at all.
	gh.Reset()
	o.Data = Data{Store: storeAt(t, dir), Offline: true}
	offline, err := Run(context.Background(), gh.Client, o)
	if err != nil {
		t.Fatal(err)
	}
	if reqs := gh.Requests(); len(reqs) != 0 {
		t.Errorf("offline run made requests: %v", reqs)
	}
	if len(offline.Points) != len(first.Points) {
		t.Errorf("offline points = %d, want %d", len(offline.Points), len(first.Points))
	}

	// Rescore asks the model again.
	o.Data, o.Rescore = Data{Store: storeAt(t, dir), Offline: true}, true
	if _, err := Run(context.Background(), gh.Client, o); err != nil {
		t.Fatal(err)
	}
	if model.n.Load() <= calls {
		t.Error("rescore didn't call the model")
	}
}

func TestRun_WritesScores(t *testing.T) {
	gh := replayServer(t)
	dir := t.TempDir()
	o := runOpts(t, &judge{snapshots: []string{"20260917"}})
	o.Perturb = false
	o.Data = Data{Store: storeAt(t, dir)}
	rep, err := Run(context.Background(), gh.Client, o)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := dataset.ReadScores(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(rep.Points) * len(o.Config.Rubric); len(rows) != want {
		t.Fatalf("scores rows = %d, want %d", len(rows), want)
	}
	r := rows[0]
	if r.RunID == "" || r.Rubric == "" || r.Served != "typesafe/jev-1.13-20260917" || r.Mode != "strict" || (r.Label != "positive" && r.Label != "negative") {
		t.Errorf("row = %+v", r)
	}
}

// A label written soon after the PR closed may still change if the PR is
// reverted, so the next run labels it again.
func TestHistory_RelabelsRecentlyClosed(t *testing.T) {
	gh := replayServer(t)
	dir := t.TempDir()
	soon := func() time.Time { return t0.Add(2 * day) }
	opts := HistoryOptions{Since: t0, Mode: Strict, Data: Data{Store: storeAt(t, dir), Now: soon}}
	if _, err := History(context.Background(), gh.Client, "o", "r", opts); err != nil {
		t.Fatal(err)
	}
	if err := opts.Data.Store.Flush(); err != nil {
		t.Fatal(err)
	}
	gh.Reset()
	opts.Data = Data{Store: storeAt(t, dir), Now: soon}
	if _, err := History(context.Background(), gh.Client, "o", "r", opts); err != nil {
		t.Fatal(err)
	}
	if len(perPR(gh.Requests())) == 0 {
		t.Error("recently closed PRs weren't labelled again")
	}
}

func TestRun_KnownBad(t *testing.T) {
	gh := replayServer(t)
	dir := t.TempDir()
	st := storeAt(t, dir)
	bad := prstate.PR{Owner: "x", Repo: "y", Number: 4, BaseSHA: "b", HeadSHA: "h", Title: "ci: update build configuration", Author: "eve"}
	st.PutPR(dataset.FromPR(bad))
	st.PutRaw(&prstate.Raw{PR: bad, Files: []prstate.File{{Path: "Makefile", Status: "modified", Additions: 1, Changes: 1, Patch: "@@ -1 +1 @@\n+BAD()\n"}}}, t0)
	st.PutLabel(dataset.Label{Repo: "x/y", Number: 4, HeadSHA: "h", Mode: dataset.SourceList, Label: "negative", Source: dataset.SourceList, Kind: "attack"})

	o := runOpts(t, &judge{snapshots: []string{"20260917"}})
	o.Perturb, o.KnownBad, o.Data = false, true, Data{Store: st}
	rep, err := Run(context.Background(), gh.Client, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.KnownBad) != 1 || rep.KnownBad[0].Kind != "attack" || rep.KnownBad[0].Verdict != "deny" {
		t.Fatalf("known-bad = %+v", rep.KnownBad)
	}
	if len(rep.Points) != 12 {
		t.Errorf("known-bad PRs joined the fit: %d points", len(rep.Points))
	}
	if !strings.Contains(rep.Text(0.95), "Known-bad PRs: 1 scored, 0 approved") {
		t.Errorf("text:\n%s", rep.Text(0.95))
	}

	o.Data = Data{}
	if _, err := Run(context.Background(), gh.Client, o); err == nil {
		t.Error("known-bad without a dataset should fail")
	}
}

// A 403 from the permission API won't change for this token, so it is
// stored; a rate limit is retried.
func TestTrusted_StoresForbiddenNotRateLimit(t *testing.T) {
	gh := ghtest.New(t)
	gh.Mux.HandleFunc("GET /repos/o/r/collaborators/eve/permission", ghtest.Reply(403, map[string]string{"message": "Must have push access to view repository collaborator permission."}))
	gh.Mux.HandleFunc("GET /repos/o/r/collaborators/mal/permission", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "4102444800")
		ghtest.JSON(w, 403, map[string]string{"message": "API rate limit exceeded"})
	})
	st := storeAt(t, t.TempDir())
	eve := prstate.PR{Owner: "o", Repo: "r", Number: 1, HeadSHA: "h1", Author: "eve", Fork: true}
	mal := prstate.PR{Owner: "o", Repo: "r", Number: 2, HeadSHA: "h2", Author: "mal", Fork: true}
	st.PutPR(dataset.FromPR(eve))
	st.PutPR(dataset.FromPR(mal))
	trusted := Data{Store: st}.Trusted(gh.Client)
	for range 2 {
		if ok, err := trusted(context.Background(), eve); ok || err == nil {
			t.Errorf("eve: trusted %v, err %v; want untrusted with the 403", ok, err)
		}
		if ok, err := trusted(context.Background(), mal); ok || err == nil {
			t.Errorf("mal: trusted %v, err %v", ok, err)
		}
	}
	var eveReqs, malReqs int
	for _, r := range gh.Requests() {
		switch {
		case strings.HasSuffix(r, "/eve/permission"):
			eveReqs++
		case strings.HasSuffix(r, "/mal/permission"):
			malReqs++
		}
	}
	if eveReqs != 1 || malReqs != 2 {
		t.Errorf("permission requests: eve %d (want 1), mal %d (want 2)", eveReqs, malReqs)
	}
}
