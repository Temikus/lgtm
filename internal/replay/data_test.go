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
	o.Perturb, o.Concurrency = false, 1
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
	// Only the first call asks, to learn which version is being served.
	if model.n.Load() != calls+1 {
		t.Errorf("second run asked the model %d times, want 1", model.n.Load()-calls)
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

	// When the short name moves to a new version, stored replies from the
	// old one are asked again, and the fit is for the new version only.
	moved := &countCalls{Decider: &judge{snapshots: []string{"20261101"}}}
	o.Decider, o.Data = moved, Data{Store: storeAt(t, dir), Offline: true}
	swapped, err := Run(context.Background(), gh.Client, o)
	if err != nil {
		t.Fatal(err)
	}
	if swapped.Calibration == nil || swapped.Calibration.Snapshot != "typesafe/jev-1.13-20261101" || moved.n.Load() < int64(len(first.Points)) {
		t.Errorf("after a version change: calibration %+v, %d calls for %d PRs", swapped.Calibration, moved.n.Load(), len(first.Points))
	}
	o.Decider = model

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

func TestRun_Lists(t *testing.T) {
	gh := replayServer(t)
	dir := t.TempDir()
	st := storeAt(t, dir)
	bad := prstate.PR{Owner: "x", Repo: "y", Number: 4, BaseSHA: "b", HeadSHA: "h", Title: "ci: update build configuration", Author: "eve"}
	st.PutPR(dataset.FromPR(bad))
	st.PutRaw(&prstate.Raw{PR: bad, Files: []prstate.File{{Path: "Makefile", Status: "modified", Additions: 1, Changes: 1, Patch: "@@ -1 +1 @@\n+BAD()\n"}}}, t0)
	st.PutLabel(dataset.Label{Repo: "x/y", Number: 4, HeadSHA: "h", Mode: dataset.SourceList, Label: "negative", Source: dataset.SourceList, Kind: "attack"})

	o := runOpts(t, &judge{snapshots: []string{"20260917"}})
	o.Perturb, o.Lists, o.Data = false, true, Data{Store: st}
	rep, err := Run(context.Background(), gh.Client, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Listed) != 1 || rep.Listed[0].Kind != "attack" || rep.Listed[0].Label != Negative || rep.Listed[0].Verdict != "deny" {
		t.Fatalf("listed = %+v", rep.Listed)
	}
	if len(rep.Points) != fixturePRs {
		t.Errorf("known-bad PRs joined the fit: %d points", len(rep.Points))
	}
	if text := rep.Text(); !strings.Contains(text, "Cap sweep") || !strings.Contains(text, "known-bad caught: attack 1 of 1") {
		t.Errorf("cap sweep missing:\n%s", text)
	}
	if text := rep.Text(); !strings.Contains(text, "Safety:") || !strings.Contains(text, "known-bad caught: attack 1 of 1 (miss ≤ 98%)") {
		t.Errorf("safety section missing:\n%s", text)
	}
	if !strings.Contains(rep.Text(), "Listed PRs: 1 scored, 0 known-bad approved") {
		t.Errorf("text:\n%s", rep.Text())
	}

	o.Data = Data{}
	if _, err := Run(context.Background(), gh.Client, o); err == nil {
		t.Error("lists without a dataset should fail")
	}
}

// A 403 from the permission API is stored for the login that got it; a
// rate limit is retried.
func TestTrusted_StoresForbiddenNotRateLimit(t *testing.T) {
	login := "me"
	gh := ghtest.New(t)
	gh.Mux.HandleFunc("GET /user", func(w http.ResponseWriter, _ *http.Request) { ghtest.JSON(w, 200, map[string]any{"login": login}) })
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
	now := t0
	data := Data{Store: st, Now: func() time.Time { return now }}
	count := func(user string) int {
		n := 0
		for _, r := range gh.Requests() {
			if strings.HasSuffix(r, "/"+user+"/permission") {
				n++
			}
		}
		return n
	}
	check := func(pr prstate.PR) {
		t.Helper()
		if ok, err := data.Trusted(gh.Client)(context.Background(), pr); ok || err == nil {
			t.Errorf("%s: trusted %v, err %v; want untrusted with an error", pr.Author, ok, err)
		}
	}

	for range 2 {
		check(eve)
		check(mal)
	}
	if count("eve") != 1 || count("mal") != 2 {
		t.Errorf("permission requests: eve %d (want 1), mal %d (want 2)", count("eve"), count("mal"))
	}

	// Another login may have push access, so it checks again.
	login = "maintainer"
	check(eve)
	if count("eve") != 2 {
		t.Errorf("a different login reused the stored 403: %d requests", count("eve"))
	}
	// So does the same login once the stored answer is old.
	now = now.Add(forbiddenTTL + time.Hour)
	check(eve)
	if count("eve") != 3 {
		t.Errorf("an expired 403 was reused: %d requests", count("eve"))
	}
	// Offline uses whatever is stored.
	gh.Reset()
	data.Offline = true
	if _, err := data.Trusted(nil)(context.Background(), eve); err == nil || len(gh.Requests()) != 0 {
		t.Errorf("offline: err %v, requests %v", err, gh.Requests())
	}
}

// A history pull of the same PR moves the row's head; the listed sample
// keeps the commit its label names.
func TestListed_KeepsPinnedHead(t *testing.T) {
	st := storeAt(t, t.TempDir())
	st.PutPR(dataset.FromPR(prstate.PR{Owner: "x", Repo: "y", Number: 4, HeadSHA: "cleaned"}))
	st.PutLabel(dataset.Label{Repo: "x/y", Number: 4, HeadSHA: "attack", Mode: dataset.SourceList, Label: "negative", Source: dataset.SourceList, Kind: "workflow-exfil"})
	ss, _ := Data{Store: st}.listed()
	if len(ss) != 1 || ss[0].PR.HeadSHA != "attack" {
		t.Errorf("listed = %+v", ss)
	}
}
