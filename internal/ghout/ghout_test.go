package ghout

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Temikus/lgtm/internal/config"
	"github.com/Temikus/lgtm/internal/engine"
	"github.com/Temikus/lgtm/internal/ghtest"
	"github.com/Temikus/lgtm/internal/prstate"
	"github.com/Temikus/lgtm/internal/report"
	"github.com/Temikus/lgtm/internal/verdict"
)

var pr = prstate.PR{Owner: "o", Repo: "r", Number: 7, HeadSHA: "head111"}

type calls struct {
	mu   sync.Mutex
	seen []string
	body map[string]map[string]any
}

func (c *calls) record(name string, reply ...any) http.HandlerFunc {
	var out any = map[string]any{"id": 1}
	if len(reply) > 0 {
		out = reply[0]
	}
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if c.body == nil {
			c.body = map[string]map[string]any{}
		}
		c.seen = append(c.seen, name)
		c.body[name] = b
		ghtest.JSON(w, 200, out)
	}
}

func result(mode config.Mode, v verdict.Verdict) *engine.Result {
	return &engine.Result{PR: pr, Mode: mode, Model: "m", ApproveAt: 0.9, DenyAt: 0.1, Verdict: verdict.Result{Verdict: v, Aggregate: 0.5}}
}

func TestComment_IgnoresMarkerFromOtherAuthors(t *testing.T) {
	gh := ghtest.New(t)
	c := &calls{}
	gh.Mux.HandleFunc("GET /repos/o/r/issues/7/comments", ghtest.Reply(200, []any{
		map[string]any{"id": 11, "body": report.Marker + "\nfake: approve", "user": map[string]any{"login": "mallory"}},
	}))
	gh.Mux.HandleFunc("POST /repos/o/r/issues/7/comments", c.record("create"))
	gh.Mux.HandleFunc("PATCH /repos/o/r/issues/comments/11", c.record("update-11"))

	if err := (Publisher{GH: gh.Client, PR: pr, CommentAuthor: ActionsBot}).Comment(context.Background(), report.Marker+"\nreal"); err != nil {
		t.Fatal(err)
	}
	if len(c.seen) != 1 || c.seen[0] != "create" {
		t.Errorf("calls = %v; a marker comment by someone else must be ignored", c.seen)
	}
}

func TestComment_UpdatesBotCommentInPlace(t *testing.T) {
	gh := ghtest.New(t)
	c := &calls{}
	gh.Mux.HandleFunc("GET /repos/o/r/issues/7/comments", ghtest.Reply(200, []any{
		map[string]any{"id": 10, "body": "unrelated", "user": map[string]any{"login": ActionsBot}},
		map[string]any{"id": 12, "body": report.Marker + "\nold", "user": map[string]any{"login": ActionsBot}},
	}))
	gh.Mux.HandleFunc("POST /repos/o/r/issues/7/comments", c.record("create"))
	gh.Mux.HandleFunc("PATCH /repos/o/r/issues/comments/12", c.record("update-12"))

	if err := (Publisher{GH: gh.Client, PR: pr, CommentAuthor: ActionsBot}).Comment(context.Background(), report.Marker+"\nnew"); err != nil {
		t.Fatal(err)
	}
	if len(c.seen) != 1 || c.seen[0] != "update-12" || c.body["update-12"]["body"] != report.Marker+"\nnew" {
		t.Errorf("calls = %v body = %v", c.seen, c.body)
	}
}

func TestPublish_CheckRunAndLabel(t *testing.T) {
	tests := []struct {
		mode       config.Mode
		v          verdict.Verdict
		conclusion string
		label      string // "add", "remove", or "" for no label call
	}{
		{config.Shadow, verdict.Deny, "neutral", ""},
		{config.Enforce, verdict.Approve, "success", "remove"},
		{config.Enforce, verdict.Deny, "failure", "remove"},
		{config.Enforce, verdict.NeedsHuman, "failure", "add"},
	}
	for _, tt := range tests {
		t.Run(string(tt.mode)+"-"+string(tt.v), func(t *testing.T) {
			gh := ghtest.New(t)
			c := &calls{}
			gh.Mux.HandleFunc("POST /repos/o/r/check-runs", c.record("check"))
			gh.Mux.HandleFunc("GET /repos/o/r/issues/7/comments", ghtest.Reply(200, []any{}))
			gh.Mux.HandleFunc("POST /repos/o/r/issues/7/comments", c.record("comment"))
			gh.Mux.HandleFunc("POST /repos/o/r/issues/7/labels", c.record("add", []any{map[string]any{"name": NeedsHumanLabel}}))
			gh.Mux.HandleFunc("DELETE /repos/o/r/issues/7/labels/{name}", func(w http.ResponseWriter, r *http.Request) {
				if r.PathValue("name") != NeedsHumanLabel {
					t.Errorf("removed label %q", r.PathValue("name"))
				}
				c.record("remove")(w, r)
			})

			if err := (Publisher{GH: gh.Client, PR: pr, CommentAuthor: ActionsBot}).Publish(context.Background(), result(tt.mode, tt.v)); err != nil {
				t.Fatal(err)
			}
			check := c.body["check"]
			if check["name"] != CheckName || check["head_sha"] != "head111" || check["conclusion"] != tt.conclusion {
				t.Errorf("check = %v", check)
			}
			extID, _ := check["external_id"].(string)
			if st, ok := DecodeState(extID); !ok || st.Verdict != tt.v || st.Mode != tt.mode {
				t.Errorf("external_id = %v", check["external_id"])
			}
			gotLabel := ""
			for _, s := range c.seen {
				if s == "add" || s == "remove" {
					gotLabel = s
				}
			}
			if gotLabel != tt.label {
				t.Errorf("label call = %q, want %q", gotLabel, tt.label)
			}
		})
	}
}

func TestLabel_RemoveMissingIsFine(t *testing.T) {
	gh := ghtest.New(t)
	gh.Mux.HandleFunc("DELETE /repos/o/r/issues/7/labels/{name}", ghtest.NotFound)
	if err := (Publisher{GH: gh.Client, PR: pr}).Label(context.Background(), false); err != nil {
		t.Errorf("removing an absent label should succeed: %v", err)
	}
}

func checkRun(id int, app, extID string) map[string]any {
	return map[string]any{"id": id, "name": CheckName, "head_sha": "head111", "external_id": extID, "app": map[string]any{"slug": app},
		"output": map[string]any{"title": "0.50 · needs human review", "summary": "table"}}
}

func TestFindCheck(t *testing.T) {
	gh := ghtest.New(t)
	gh.Mux.HandleFunc("GET /repos/o/r/commits/head111/check-runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("check_name") != CheckName {
			t.Errorf("check_name = %q", r.URL.Query().Get("check_name"))
		}
		ghtest.JSON(w, 200, map[string]any{"total_count": 4, "check_runs": []any{
			checkRun(1, actionsApp, "lgtm/v1 mode=enforce verdict=deny"),
			checkRun(3, actionsApp, "lgtm/v1 mode=enforce verdict=needs-human"),
			checkRun(9, "evil-app", "lgtm/v1 mode=enforce verdict=approve"),
			checkRun(8, actionsApp, "garbage"),
		}})
	})

	run, err := FindCheck(context.Background(), gh.Client, "o", "r", "head111")
	if err != nil {
		t.Fatal(err)
	}
	if run == nil || run.GetID() != 3 {
		t.Fatalf("run = %v; want the latest valid run from github-actions", run)
	}
}

func TestMarkApproved(t *testing.T) {
	gh := ghtest.New(t)
	c := &calls{}
	gh.Mux.HandleFunc("PATCH /repos/o/r/check-runs/3", c.record("update"))
	gh.Mux.HandleFunc("GET /repos/o/r/commits/head111/check-runs", ghtest.Reply(200, map[string]any{"check_runs": []any{checkRun(3, actionsApp, "lgtm/v1 mode=enforce verdict=needs-human")}}))
	run, err := FindCheck(context.Background(), gh.Client, "o", "r", "head111")
	if err != nil {
		t.Fatal(err)
	}

	if err := MarkApproved(context.Background(), gh.Client, "o", "r", run, "alice_[x]"); err != nil {
		t.Fatal(err)
	}
	b := c.body["update"]
	if b["conclusion"] != "success" {
		t.Errorf("conclusion = %v", b["conclusion"])
	}
	extID, _ := b["external_id"].(string)
	st, ok := DecodeState(extID)
	if !ok || st.Verdict != verdict.Approve {
		t.Errorf("external_id = %v", b["external_id"])
	}
	out, _ := b["output"].(map[string]any)
	if s, _ := out["summary"].(string); s == "" || strings.Contains(s, "alice_[x]") {
		t.Errorf("reviewer login must be escaped in the summary: %s", s)
	}
}

func TestDecodeState(t *testing.T) {
	for _, bad := range []string{"", "lgtm/v1 mode=yolo verdict=approve", "lgtm/v1 mode=enforce verdict=maybe", "lgtm/v2 mode=enforce verdict=approve"} {
		if _, ok := DecodeState(bad); ok {
			t.Errorf("DecodeState(%q) should fail", bad)
		}
	}
}

func TestState_RoundTrip(t *testing.T) {
	for _, st := range []State{
		{Mode: config.Shadow, Verdict: verdict.Deny, Blind: true},
		{Mode: config.Enforce, Verdict: verdict.NeedsHuman},
	} {
		if got, ok := DecodeState(EncodeState(st)); !ok || got != st {
			t.Errorf("round trip of %+v gave %+v (ok %v)", st, got, ok)
		}
	}
}
