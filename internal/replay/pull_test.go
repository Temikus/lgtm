package replay

import (
	"context"
	"strings"
	"testing"

	"github.com/Temikus/lgtm/internal/ghtest"
)

func TestParseList(t *testing.T) {
	got, err := ParseList(strings.NewReader("# comment\n\nattack x/y#4\ninjection o.k/r-2#10\n"))
	if err != nil || len(got) != 2 || got[0] != (ListEntry{Kind: "attack", Owner: "x", Repo: "y", Number: 4}) || got[1].Repo != "r-2" {
		t.Errorf("ParseList = %+v, %v", got, err)
	}
	if _, err := ParseList(strings.NewReader("attack x/y 4\n")); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("bad line: err = %v", err)
	}
	sha := strings.Repeat("a1", 20)
	if got, err := ParseList(strings.NewReader("attack x/y#4@" + sha + "\n")); err != nil || got[0].SHA != sha {
		t.Errorf("pinned = %+v, %v", got, err)
	}
	if _, err := ParseList(strings.NewReader("attack x/y#4@abc\n")); err == nil {
		t.Error("a short SHA should be rejected")
	}
	if _, err := ParseList(strings.NewReader("attack x/y#0\n")); err == nil {
		t.Error("PR #0 should be rejected")
	}
	if got, err := ParseList(strings.NewReader("attack x/y@" + sha + "\n")); err != nil || got[0].Number != 0 || got[0].SHA != sha {
		t.Errorf("commit = %+v, %v", got, err)
	}
}

// A listed commit is scored as the diff against its first parent.
func TestPullList_Commit(t *testing.T) {
	sha, parent := strings.Repeat("c3", 20), strings.Repeat("d4", 20)
	gh := ghtest.New(t)
	gh.Mux.HandleFunc("GET /repos/x/y/commits/"+sha, ghtest.Reply(200, map[string]any{
		"sha": sha, "parents": []any{map[string]any{"sha": parent}}, "author": map[string]any{"login": "eve"},
		"commit": map[string]any{"message": "ci: add workflow\n\nSyncs config.", "author": map[string]any{"date": "2026-05-18T12:00:00Z"}},
	}))
	gh.Mux.HandleFunc("GET /repos/x/y/compare/"+parent+"..."+sha, ghtest.Reply(200, map[string]any{"files": []any{
		map[string]any{"filename": ".github/workflows/x.yml", "status": "added", "additions": 1, "changes": 1, "patch": "@@ -0,0 +1 @@\n+exfil\n"},
	}}))
	dir := t.TempDir()
	entries := []ListEntry{{Kind: "workflow-exfil", Owner: "x", Repo: "y", SHA: sha}}
	d := Data{Store: storeAt(t, dir)}
	if st, err := PullList(context.Background(), gh.Client, d, entries, Negative); err != nil || st.Diffs != 1 {
		t.Fatalf("stats %+v, %v", st, err)
	}
	ss, kinds := d.listed()
	if len(ss) != 1 || ss[0].PR.Number >= 0 || ss[0].PR.Title != "ci: add workflow" || ss[0].PR.Body != "Syncs config." || kinds[0] != "workflow-exfil" {
		t.Fatalf("listed = %+v %v", ss, kinds)
	}
	if got := listRef(ss[0].PR); got != "x/y@"+sha[:12] {
		t.Errorf("ref = %q", got)
	}
	gh.Reset()
	if st, _ := PullList(context.Background(), gh.Client, d, entries, Negative); st.Cached != 1 || len(gh.Requests()) != 0 {
		t.Errorf("second pull: %+v, requests %v", st, gh.Requests())
	}
}

// A pinned entry scores the pinned commit, not the PR's current head.
func TestPullList_Pinned(t *testing.T) {
	pinned := strings.Repeat("b2", 20)
	gh := ghtest.New(t)
	gh.Mux.HandleFunc("GET /repos/x/y/pulls/4", ghtest.Reply(200, fakePR{n: 4, title: "ci: fix", author: "eve", closed: day}.json()))
	gh.Mux.HandleFunc("GET /repos/x/y/compare/base..."+pinned, ghtest.Reply(200, map[string]any{"files": []any{
		map[string]any{"filename": "ci.yml", "status": "added", "additions": 1, "changes": 1, "patch": "@@ -0,0 +1 @@\n+exfil\n"},
	}}))
	dir := t.TempDir()
	entries := []ListEntry{{Kind: "attack", Owner: "x", Repo: "y", Number: 4, SHA: pinned}}
	d := Data{Store: storeAt(t, dir)}
	if st, err := PullList(context.Background(), gh.Client, d, entries, Negative); err != nil || st.Diffs != 1 || st.Failed != 0 {
		t.Fatalf("stats %+v, %v", st, err)
	}
	ss, _ := d.listed()
	if len(ss) != 1 || ss[0].PR.HeadSHA != pinned {
		t.Fatalf("listed = %+v", ss)
	}
	raw, ok := d.Store.Raw("x/y", 4, pinned)
	if !ok || len(raw.Files) != 1 || raw.Files[0].Path != "ci.yml" {
		t.Errorf("raw = %+v, %v", raw, ok)
	}
	if snap, _ := raw.Snapshot(nil); len(snap.Partial) != 0 {
		t.Errorf("pinned diff marked partial: %v", snap.Partial)
	}
}

func TestPullList(t *testing.T) {
	gh := ghtest.New(t)
	gh.Mux.HandleFunc("GET /repos/x/y/pulls/4", ghtest.Reply(200, fakePR{n: 4, title: "ci: update build configuration", author: "eve", closed: day}.json()))
	gh.Mux.HandleFunc("GET /repos/x/y/compare/base...head4", ghtest.Reply(200, map[string]any{"files": []any{
		map[string]any{"filename": "Makefile", "status": "modified", "additions": 1, "changes": 1, "patch": "@@ -1 +1 @@\n+exfil\n"},
	}}))
	gh.Mux.HandleFunc("GET /repos/x/gone/pulls/1", ghtest.NotFound)
	dir := t.TempDir()
	entries := []ListEntry{{Kind: "attack", Owner: "x", Repo: "y", Number: 4}, {Kind: "attack", Owner: "x", Repo: "gone", Number: 1}}

	d := Data{Store: storeAt(t, dir)}
	st, err := PullList(context.Background(), gh.Client, d, entries, Negative)
	if err != nil {
		t.Fatal(err)
	}
	if st != (PullStats{PRs: 2, Labelled: 1, Diffs: 1, Failed: 1}) {
		t.Errorf("stats = %+v", st)
	}
	if err := d.Store.Flush(); err != nil {
		t.Fatal(err)
	}
	ss, kinds := Data{Store: storeAt(t, dir)}.listed()
	if len(ss) != 1 || kinds[0] != "attack" || ss[0].Label != Negative || ss[0].PR.HeadSHA != "head4" {
		t.Errorf("listed = %+v %v", ss, kinds)
	}

	// A second pull reads only the PR GitHub didn't serve.
	gh.Reset()
	st, err = PullList(context.Background(), gh.Client, Data{Store: storeAt(t, dir)}, entries, Negative)
	if err != nil {
		t.Fatal(err)
	}
	if reqs := gh.Requests(); len(reqs) != 1 || reqs[0] != "GET /repos/x/gone/pulls/1" || st.Cached != 1 {
		t.Errorf("second pull: stats %+v, requests %v", st, reqs)
	}
}

func TestPull_ThenOfflineHistory(t *testing.T) {
	gh := replayServer(t)
	dir := t.TempDir()
	o := runOpts(t, nil)
	o.History.Data = Data{Store: storeAt(t, dir), Now: o.Now}
	st, err := Pull(context.Background(), gh.Client, "o", "r", o.History)
	if err != nil {
		t.Fatal(err)
	}
	if st.PRs != fixturePRs || st.Labelled != fixturePRs || st.Diffs != fixturePRs || st.Failed != 0 {
		t.Errorf("stats = %+v", st)
	}
	if err := o.History.Data.Store.Flush(); err != nil {
		t.Fatal(err)
	}
	gh.Reset()
	o.History.Data = Data{Store: storeAt(t, dir), Offline: true}
	ss, err := History(context.Background(), nil, "o", "r", o.History)
	if err != nil || len(ss) != fixturePRs {
		t.Fatalf("offline history = %d samples, %v", len(ss), err)
	}
	if _, ok := o.History.Data.Store.Raw("o/r", ss[0].PR.Number, ss[0].PR.HeadSHA); !ok {
		t.Error("pull didn't store the diff")
	}
}

func TestPullList_KnownGood(t *testing.T) {
	gh := ghtest.New(t)
	gh.Mux.HandleFunc("GET /repos/x/y/pulls/5", ghtest.Reply(200, fakePR{n: 5, title: "Bump deps", body: "@coderabbitai review", author: "ann", merged: true, closed: day}.json()))
	gh.Mux.HandleFunc("GET /repos/x/y/compare/base...head5", ghtest.Reply(200, map[string]any{"files": []any{
		map[string]any{"filename": "go.mod", "status": "modified", "additions": 1, "changes": 1, "patch": "@@ -1 +1 @@\n+go 1.27\n"},
	}}))
	d := Data{Store: storeAt(t, t.TempDir())}
	entries := []ListEntry{{Kind: "ai-text", Owner: "x", Repo: "y", Number: 5}}
	if _, err := PullList(context.Background(), gh.Client, d, entries, "maybe"); err == nil {
		t.Error("want an error for a label that isn't positive or negative")
	}
	if _, err := PullList(context.Background(), gh.Client, d, entries, Positive); err != nil {
		t.Fatal(err)
	}
	ss, kinds := d.listed()
	if len(ss) != 1 || ss[0].Label != Positive || kinds[0] != "ai-text" {
		t.Errorf("listed = %+v %v", ss, kinds)
	}
}
