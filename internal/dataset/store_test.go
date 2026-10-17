package dataset

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Temikus/lgtm/internal/prstate"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func samplePR() prstate.PR {
	return prstate.PR{Owner: "o", Repo: "r", Number: 7, BaseSHA: "b", HeadSHA: "h", Title: "Add retry", Body: "Retries 429s.", Labels: []string{"lgtm"}, Author: "ann", Fork: true, ChangedFiles: 2}
}

func sampleRaw() *prstate.Raw {
	return &prstate.Raw{
		PR: samplePR(),
		Files: []prstate.File{
			{Path: "retry.go", Status: "modified", Additions: 3, Changes: 3, Patch: "@@ -1 +1,3 @@\n+func retry() {}\n"},
			{Path: "logo.png", Status: "added", Changes: 1},
		},
		LinkedIssues: []string{"Retries are missing"},
		Partial:      []string{"compare returned 2 of 3 changed files"},
	}
}

func reopen(t *testing.T, s *Store) *Store {
	t.Helper()
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	got, err := Open(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestOpen_MissingDirIsEmpty(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.PR("o/r", 1); ok {
		t.Error("found a PR in an empty store")
	}
	if err := s.Flush(); err != nil {
		t.Errorf("flushing nothing: %v", err)
	}
}

func TestStore_RoundTrip(t *testing.T) {
	s, _ := Open(t.TempDir())
	pr := FromPR(samplePR())
	pr.Created, pr.Closed, pr.Comments, pr.FetchedAt = Time(t0), Time(t0.Add(time.Hour)), 3, t0
	s.PutPR(pr)
	s.PutRaw(sampleRaw(), t0)
	s.PutTrust("o/r", 7, true, "")
	s.PutLabel(Label{Repo: "o/r", Number: 7, HeadSHA: "h", Mode: "strict", Label: "positive", Reason: "approved by bob", Source: "history", LabelledAt: t0})
	s.PutResponse(Response{Key: "k1", Model: "m", Served: "m-1", Answers: `{"a":{"type":"noul","noul":0.9}}`, CostUSD: 0.001, CreatedAt: t0})
	s.AddScores([]Score{{RunID: "run1", Repo: "o/r", Number: 7, Question: "safe", Value: 0.97, Label: "positive", CreatedAt: t0}})

	s = reopen(t, s)
	got, ok := s.PR("o/r", 7)
	if !ok || got.Title != "Add retry" || got.Closed == nil || !got.Closed.Equal(t0.Add(time.Hour)) || !got.TrustChecked || !got.AuthorTrusted || got.Comments != 3 {
		t.Errorf("PR = %+v, %v", got, ok)
	}
	if got.Merged != nil {
		t.Errorf("unmerged PR came back merged at %v", got.Merged)
	}
	if got.PR().Owner != "o" || got.PR().Repo != "r" || !slices.Equal(got.PR().Labels, []string{"lgtm"}) {
		t.Errorf("PR() = %+v", got.PR())
	}
	raw, ok := s.Raw("o/r", 7, "h")
	if !ok {
		t.Fatal("no raw diff")
	}
	snap, err := raw.Snapshot(nil)
	want, _ := sampleRaw().Snapshot(nil)
	if err != nil || len(snap.Files) != 2 || len(snap.Unseen) != 1 || !slices.Equal(snap.Partial, want.Partial) || !slices.Equal(snap.LinkedIssues, want.LinkedIssues) {
		t.Errorf("snapshot from store = %+v, %v; want %+v", snap, err, want)
	}
	if _, ok := s.Raw("o/r", 7, "other-head"); ok {
		t.Error("raw diff found for a different head")
	}
	if l, ok := s.Label("o/r", 7, "strict"); !ok || l.Reason != "approved by bob" {
		t.Errorf("label = %+v, %v", l, ok)
	}
	if _, ok := s.Label("o/r", 7, "merged"); ok {
		t.Error("label found for another mode")
	}
	if r, ok := s.Response("k1"); !ok || r.Served != "m-1" {
		t.Errorf("response = %+v, %v", r, ok)
	}
	scores, err := ReadScores(s.Dir())
	if err != nil || len(scores) != 1 || scores[0].Value != 0.97 {
		t.Errorf("scores = %+v, %v", scores, err)
	}
}

func TestStore_LaterWriteWins(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.PutLabel(Label{Repo: "o/r", Number: 7, Mode: "strict", Label: "positive"})
	s = reopen(t, s)
	s.PutLabel(Label{Repo: "o/r", Number: 7, Mode: "strict", Label: "negative", Reason: "reverted"})
	s.PutLabel(Label{Repo: "o/r", Number: 8, Mode: "strict", Label: "positive"})
	s = reopen(t, s)

	parts, _ := filepath.Glob(filepath.Join(s.Dir(), "labels", "*.parquet"))
	if len(parts) != 2 {
		t.Errorf("parts = %v, want one per flush", parts)
	}
	if l, _ := s.Label("o/r", 7, "strict"); l.Label != "negative" {
		t.Errorf("label = %+v, want the later write", l)
	}
	if _, ok := s.Label("o/r", 8, "strict"); !ok {
		t.Error("the second part's new row is missing")
	}
}

func TestStore_PRsNewestFirst(t *testing.T) {
	s, _ := Open(t.TempDir())
	for n, closed := range map[int64]time.Duration{1: 3 * time.Hour, 2: time.Hour, 3: 2 * time.Hour} {
		s.PutPR(PR{Repo: "o/r", Number: n, Closed: Time(t0.Add(closed))})
	}
	s.PutPR(PR{Repo: "o/other", Number: 9, Closed: Time(t0)})
	s = reopen(t, s)
	var got []int64
	for _, p := range s.PRs("o/r") {
		got = append(got, p.Number)
	}
	if !slices.Equal(got, []int64{1, 3, 2}) {
		t.Errorf("PRs = %v", got)
	}
}

func TestFlush_LeavesNoTempFiles(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.PutPR(PR{Repo: "o/r", Number: 1})
	s = reopen(t, s)
	err := filepath.Walk(s.Dir(), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Ext(p) != ".parquet" {
			t.Errorf("stray file %s", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
