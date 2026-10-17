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
	st, err := PullList(context.Background(), gh.Client, d, entries)
	if err != nil {
		t.Fatal(err)
	}
	if st != (PullStats{PRs: 2, Labelled: 1, Diffs: 1, Failed: 1}) {
		t.Errorf("stats = %+v", st)
	}
	if err := d.Store.Flush(); err != nil {
		t.Fatal(err)
	}
	ss, kinds := Data{Store: storeAt(t, dir)}.knownBad()
	if len(ss) != 1 || kinds[0] != "attack" || ss[0].Label != Negative || ss[0].PR.HeadSHA != "head4" {
		t.Errorf("known-bad = %+v %v", ss, kinds)
	}

	// A second pull reads only the PR GitHub didn't serve.
	gh.Reset()
	st, err = PullList(context.Background(), gh.Client, Data{Store: storeAt(t, dir)}, entries)
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
	if st.PRs != 12 || st.Labelled != 12 || st.Diffs != 12 || st.Failed != 0 {
		t.Errorf("stats = %+v", st)
	}
	if err := o.History.Data.Store.Flush(); err != nil {
		t.Fatal(err)
	}
	gh.Reset()
	o.History.Data = Data{Store: storeAt(t, dir), Offline: true}
	ss, err := History(context.Background(), nil, "o", "r", o.History)
	if err != nil || len(ss) != 12 {
		t.Fatalf("offline history = %d samples, %v", len(ss), err)
	}
	if _, ok := o.History.Data.Store.Raw("o/r", ss[0].PR.Number, ss[0].PR.HeadSHA); !ok {
		t.Error("pull didn't store the diff")
	}
}
