package replay

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Temikus/lgtm/internal/ghtest"
)

var t0 = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func ts(d time.Duration) string { return t0.Add(d).Format(time.RFC3339) }

const day = 24 * time.Hour

type fakePR struct {
	n             int
	title, body   string
	author        string
	bot           bool
	merged        bool
	mergedBy      string
	created       time.Duration
	closed        time.Duration
	lines         int
	reviewComment int
	mergeSHA      string
	reviews       []map[string]any
}

func (p fakePR) json() map[string]any {
	userType := "User"
	if p.bot {
		userType = "Bot"
	}
	m := map[string]any{
		"number": p.n, "title": p.title, "body": p.body, "state": "closed",
		"user":       map[string]any{"login": p.author, "type": userType},
		"created_at": ts(p.created), "updated_at": ts(p.closed + time.Hour), "closed_at": ts(p.closed),
		"merged": p.merged, "additions": p.lines, "deletions": 0, "changed_files": 1,
		"review_comments": p.reviewComment, "merge_commit_sha": p.mergeSHA,
		"base": map[string]any{"sha": "base", "ref": "main", "repo": map[string]any{"full_name": "o/r"}},
		"head": map[string]any{"sha": fmt.Sprintf("head%d", p.n), "ref": "b", "repo": map[string]any{"full_name": "o/r"}},
	}
	if p.merged {
		m["merged_at"] = ts(p.closed)
		m["merged_by"] = map[string]any{"login": p.mergedBy}
	}
	return m
}

func review(login, state string, at time.Duration) map[string]any {
	return map[string]any{"user": map[string]any{"login": login}, "state": state, "submitted_at": ts(at), "commit_id": "c1"}
}

func serveHistory(t *testing.T, prs []fakePR, writers ...string) *ghtest.Server {
	t.Helper()
	gh := ghtest.New(t)
	var list []any
	for _, p := range prs {
		list = append(list, p.json())
		gh.Mux.HandleFunc(fmt.Sprintf("GET /repos/o/r/pulls/%d", p.n), ghtest.Reply(200, p.json()))
		reviews := p.reviews
		if reviews == nil {
			reviews = []map[string]any{}
		}
		gh.Mux.HandleFunc(fmt.Sprintf("GET /repos/o/r/pulls/%d/reviews", p.n), ghtest.Reply(200, reviews))
	}
	gh.Mux.HandleFunc("GET /repos/o/r/pulls", ghtest.Reply(200, list))
	gh.Mux.HandleFunc("GET /repos/o/r/collaborators/{user}/permission", func(w http.ResponseWriter, r *http.Request) {
		perm := "read"
		for _, u := range writers {
			if r.PathValue("user") == u {
				perm = "write"
			}
		}
		ghtest.JSON(w, 200, map[string]any{"permission": perm, "role_name": perm})
	})
	gh.Mux.HandleFunc("GET /repos/o/r/git/commits/c1", ghtest.Reply(200, map[string]any{"sha": "c1", "committer": map[string]any{"date": ts(2 * day)}}))
	return gh
}

func byNumber(t *testing.T, ss []Sample) map[int]Sample {
	t.Helper()
	out := map[int]Sample{}
	for _, s := range ss {
		out[s.PR.Number] = s
	}
	return out
}

func TestHistory_Strict(t *testing.T) {
	prs := []fakePR{
		{n: 1, title: "Add retry", author: "ann", merged: true, closed: 3 * day, lines: 40, reviews: []map[string]any{review("bob", "APPROVED", 3*day)}},
		{n: 2, title: "Big refactor", author: "ann", merged: true, closed: 3 * day, lines: 900, reviews: []map[string]any{review("bob", "APPROVED", 2*day+time.Minute)}},
		{n: 3, title: "Bad idea", author: "ann", closed: 4 * day, reviews: []map[string]any{review("bob", "CHANGES_REQUESTED", 3*day)}},
		{n: 4, title: "Self approved", author: "ann", merged: true, closed: 4 * day, reviews: []map[string]any{review("ann", "APPROVED", 3*day)}},
		{n: 5, title: "Outsider approved", author: "ann", merged: true, closed: 4 * day, reviews: []map[string]any{review("eve", "APPROVED", 3*day)}},
		{n: 6, title: "Approval dismissed", author: "ann", merged: true, closed: 4 * day, reviews: []map[string]any{review("bob", "APPROVED", 3*day), review("bob", "DISMISSED", 3*day)}},
		{n: 7, title: "Abandoned", author: "ann", closed: 4 * day},
	}
	gh := serveHistory(t, prs, "ann", "bob")

	got, err := History(context.Background(), gh.Client, "o", "r", HistoryOptions{Since: t0, Mode: Strict})
	if err != nil {
		t.Fatal(err)
	}
	s := byNumber(t, got)
	want := map[int]struct {
		label Label
		low   bool
	}{
		1: {Positive, false},
		2: {Positive, true}, // 900 lines approved a minute after the commit
		3: {Negative, false},
		4: {Unlabelled, false},
		5: {Unlabelled, false},
		6: {Unlabelled, false},
		7: {Unlabelled, false},
	}
	for n, w := range want {
		if s[n].Label != w.label || s[n].Low != w.low {
			t.Errorf("#%d = %q low=%v (%s), want %q low=%v", n, s[n].Label, s[n].Low, s[n].Reason, w.label, w.low)
		}
	}
}

func TestHistory_Reverts(t *testing.T) {
	approved := []map[string]any{review("bob", "APPROVED", time.Hour)}
	prs := []fakePR{
		{n: 1, title: "Add cache", author: "ann", merged: true, closed: day, reviews: approved},
		{n: 2, title: `Revert "Add cache"`, author: "ann", merged: true, closed: 3 * day},
		{n: 3, title: "Add queue", author: "ann", merged: true, closed: day, reviews: approved},
		{n: 4, title: "Undo queue", body: "Reverts o/r#3", author: "ann", merged: true, closed: 2 * day},
		{n: 5, title: "Add flag", author: "ann", merged: true, closed: day, mergeSHA: "abcdef1234567890", reviews: approved},
		{n: 6, title: "Back out flag", body: "This reverts commit abcdef12.", author: "ann", merged: true, closed: 2 * day},
		{n: 7, title: "Add knob", author: "ann", merged: true, closed: day, reviews: approved},
		{n: 8, title: `Revert "Add knob"`, author: "ann", merged: true, closed: 20 * day},
	}
	gh := serveHistory(t, prs, "ann", "bob")

	got, err := History(context.Background(), gh.Client, "o", "r", HistoryOptions{Since: t0, Mode: Strict})
	if err != nil {
		t.Fatal(err)
	}
	s := byNumber(t, got)
	for _, n := range []int{1, 3, 5} {
		if s[n].Label != Negative {
			t.Errorf("#%d = %q (%s), want negative", n, s[n].Label, s[n].Reason)
		}
	}
	if s[7].Label != Positive {
		t.Errorf("#7 reverted after 19 days = %q, want positive", s[7].Label)
	}
}

func TestHistory_MergedMode(t *testing.T) {
	prs := []fakePR{
		{n: 1, title: "Solo change", author: "ann", merged: true, mergedBy: "ann", closed: day},
		{n: 2, title: "Abandoned", author: "ann", closed: day},
		{n: 3, title: "Bump x", author: "renovate[bot]", bot: true, closed: day},
	}
	gh := serveHistory(t, prs, "ann")

	got, err := History(context.Background(), gh.Client, "o", "r", HistoryOptions{Since: t0, Mode: Merged})
	if err != nil {
		t.Fatal(err)
	}
	s := byNumber(t, got)
	if _, ok := s[3]; ok {
		t.Error("bot PR should be skipped in merged mode")
	}
	if s[1].Label != Positive || s[2].Label != Negative {
		t.Errorf("labels = %q %q", s[1].Label, s[2].Label)
	}
}

func TestHistory_SinceAndMax(t *testing.T) {
	prs := []fakePR{
		{n: 3, title: "new", author: "ann", closed: 5 * day},
		{n: 2, title: "mid", author: "ann", closed: 3 * day},
		{n: 1, title: "old", author: "ann", closed: -3 * day},
	}
	gh := serveHistory(t, prs, "ann")

	got, err := History(context.Background(), gh.Client, "o", "r", HistoryOptions{Since: t0, Mode: Strict})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %d PRs, want 2 closed after since", len(got))
	}
	got, err = History(context.Background(), gh.Client, "o", "r", HistoryOptions{Since: t0, Mode: Strict, MaxPRs: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PR.Number != 3 {
		t.Errorf("max 1: got %+v", got)
	}
}

// Without push access the permission API answers 403, so strict labels fall
// back to each review's author association.
func TestHistory_StrictWithoutPushAccess(t *testing.T) {
	approved := func(assoc string) []map[string]any {
		r := review("bob", "APPROVED", time.Hour)
		r["author_association"] = assoc
		return []map[string]any{r}
	}
	prs := []fakePR{
		{n: 1, title: "member approved", author: "ann", merged: true, closed: day, reviews: approved("MEMBER")},
		{n: 2, title: "contributor approved", author: "ann", merged: true, closed: day, reviews: approved("CONTRIBUTOR")},
	}
	gh := ghtest.New(t)
	var list []any
	for _, p := range prs {
		list = append(list, p.json())
		gh.Mux.HandleFunc(fmt.Sprintf("GET /repos/o/r/pulls/%d", p.n), ghtest.Reply(200, p.json()))
		gh.Mux.HandleFunc(fmt.Sprintf("GET /repos/o/r/pulls/%d/reviews", p.n), ghtest.Reply(200, p.reviews))
	}
	gh.Mux.HandleFunc("GET /repos/o/r/pulls", ghtest.Reply(200, list))
	gh.Mux.HandleFunc("GET /repos/o/r/collaborators/{user}/permission", ghtest.Reply(403, map[string]string{"message": "Must have push access to view collaborator permission."}))

	got, err := History(context.Background(), gh.Client, "o", "r", HistoryOptions{Since: t0, Mode: Strict})
	if err != nil {
		t.Fatal(err)
	}
	s := byNumber(t, got)
	if s[1].Label != Positive || s[2].Label != Unlabelled {
		t.Errorf("labels = %q, %q", s[1].Label, s[2].Label)
	}
}
