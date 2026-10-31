package report

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Temikus/lgtm/internal/config"
	"github.com/Temikus/lgtm/internal/engine"
	"github.com/Temikus/lgtm/internal/jev"
	"github.com/Temikus/lgtm/internal/prstate"
	"github.com/Temikus/lgtm/internal/rubric"
	"github.com/Temikus/lgtm/internal/verdict"
)

var update = flag.Bool("update", false, "rewrite golden files")

func result(mode config.Mode, v verdict.Result) *engine.Result {
	r := &engine.Result{
		PR:        prstate.PR{Owner: "o", Repo: "r", Number: 7, HeadSHA: "0123456789abcdef"},
		Mode:      mode,
		Model:     "typesafe/jev-1.13",
		ApproveAt: 0.9, DenyAt: 0.1, VetoAt: 0.3, CapAt: 0.3,
		Verdict: v,
		Usage:   jev.Usage{InputTokens: 4210}, CostUSD: 0.00018, Calls: 2,
		Excluded: []prstate.File{{Path: "go.sum", Additions: 12, Deletions: 3}},
	}
	for _, q := range rubric.Defaults() {
		qr := engine.QuestionResult{Question: q, Value: 0.97, Calls: 1}
		a := jev.Answer{Type: q.Type, Noul: 0.97}
		switch q.ID {
		case "tested":
			qr.Value, a.Noul = 0.42, 0.42
		case "risk":
			a = jev.Answer{Type: jev.Choice, Choice: "moderate", Probabilities: map[string]float64{"moderate": 0.7}}
			qr.Value = 0.2
		case "reviewability":
			a = jev.Answer{Type: jev.Score, Score: 1}
			qr.Value = 0.75
		}
		qr.Answer = &a
		r.Questions = append(r.Questions, qr)
	}
	return r
}

// vetoed is a PR whose malicious answer is below veto_at but above deny_at.
func vetoed() *engine.Result {
	r := result(config.Enforce, verdict.Result{Verdict: verdict.Deny, Raw: verdict.NeedsHuman, Aggregate: 0.12, Vetoed: "malicious"})
	for i := range r.Questions {
		if r.Questions[i].ID == "malicious" {
			r.Questions[i].Value = 0.12
		}
	}
	return r
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) // #nosec G304 -- test fixture
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch (run with -update to accept)\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestComment_Golden(t *testing.T) {
	tests := map[string]*engine.Result{
		"shadow_needs_human": result(config.Shadow, verdict.Result{Verdict: verdict.NeedsHuman, Raw: verdict.NeedsHuman, Aggregate: 0.42}),
		"enforce_approve":    result(config.Enforce, verdict.Result{Verdict: verdict.Approve, Raw: verdict.Approve, Aggregate: 0.97}),
		"enforce_capped":     result(config.Enforce, verdict.Result{Verdict: verdict.NeedsHuman, Raw: verdict.Approve, Aggregate: 0.97, Caps: []verdict.CapKind{verdict.PartialView, verdict.UntrustedAuthor}}),
		"enforce_failure":    result(config.Enforce, verdict.Result{Verdict: verdict.NeedsHuman, Raw: verdict.NeedsHuman, Failure: errors.New("decision API: HTTP 529: overloaded")}),
		"enforce_vetoed":     vetoed(),
		"enforce_vetoed_shown": func() *engine.Result {
			r := vetoed()
			r.ShowScores = true
			return r
		}(),
		"shadow_nothing_to_score": func() *engine.Result {
			r := result(config.Shadow, verdict.Result{Verdict: verdict.NeedsHuman, Raw: verdict.NeedsHuman, Failure: prstate.ErrNoFiles, Caps: []verdict.CapKind{verdict.LockfileSource}})
			r.LockSources = []prstate.LockSource{{Path: "package-lock.json", Host: "evil.example"}}
			return r
		}(),
		"shadow_blind": func() *engine.Result {
			r := result(config.Shadow, verdict.Result{Verdict: verdict.Deny, Raw: verdict.Deny, Aggregate: 0.05})
			r.Blind = true
			return r
		}(),
		"shadow_rescored": result(config.Shadow, verdict.Result{Verdict: verdict.NeedsHuman, Raw: verdict.Approve, Aggregate: 0.97, Earlier: verdict.NeedsHuman}),
	}
	for name, r := range tests {
		t.Run(name, func(t *testing.T) { golden(t, name, Comment(r)) })
	}
}

func TestComment_PassWording(t *testing.T) {
	approve := Comment(result(config.Enforce, verdict.Result{Verdict: verdict.Approve, Aggregate: 0.97}))
	if !strings.Contains(approve, PassText) || strings.Contains(strings.ToLower(approve), "looks good") {
		t.Error("an approve must use the pass wording and never say it looks good")
	}
	deny := Comment(result(config.Enforce, verdict.Result{Verdict: verdict.Deny, Aggregate: 0.05}))
	if strings.Contains(deny, PassText) {
		t.Error("pass wording only on approve")
	}
}

func TestComment_InformationalCollapsed(t *testing.T) {
	c := Comment(result(config.Enforce, verdict.Result{Verdict: verdict.NeedsHuman, Aggregate: 0.42}))
	visible, _, ok := strings.Cut(c, "<details>")
	if !ok {
		t.Fatal("no details section")
	}
	for _, info := range []string{"described", "risk", "reviewability", "go.sum", "jev-1.13"} {
		if strings.Contains(visible, info) {
			t.Errorf("%q should be collapsed", info)
		}
	}
	if strings.Index(visible, "tested") > strings.Index(visible, "aligned") {
		t.Error("lowest value should come first")
	}
}

func TestEscape_UntrustedStrings(t *testing.T) {
	r := result(config.Enforce, verdict.Result{Verdict: verdict.NeedsHuman, Aggregate: 0.5})
	r.Excluded = []prstate.File{
		{Path: "<img src=x onerror=alert(1)>.go"},
		{Path: "[click](https://evil.example)|x|.md"},
		{Path: "@octocat `code` **bold**.txt"},
	}
	r.Partial = []string{"<script>alert(1)</script>"}
	c := Comment(r)
	for _, bad := range []string{"<img", "<script", "](https://", "@octocat", "|x|", "**bold**"} {
		if strings.Contains(c, bad) {
			t.Errorf("comment contains unescaped %q", bad)
		}
	}
}

func TestTitleAndConclusion(t *testing.T) {
	tests := []struct {
		mode       config.Mode
		v          verdict.Result
		title      string
		conclusion string
	}{
		{config.Shadow, verdict.Result{Verdict: verdict.Deny, Aggregate: 0.05}, "0.05 · would have: deny", "neutral"},
		{config.Enforce, verdict.Result{Verdict: verdict.Approve, Aggregate: 0.94}, "0.94 · approve", "success"},
		{config.Enforce, verdict.Result{Verdict: verdict.Deny, Aggregate: 0.05}, "0.05 · deny", "failure"},
		{config.Enforce, verdict.Result{Verdict: verdict.NeedsHuman, Aggregate: 0.5}, "0.50 · needs human review", "failure"},
		{config.Enforce, verdict.Result{Verdict: verdict.NeedsHuman, Failure: errors.New("x")}, "needs human review (scoring failed)", "failure"},
		{config.Enforce, verdict.Result{Verdict: verdict.NeedsHuman, Failure: fmt.Errorf("snapshot: %w", prstate.ErrNoFiles)}, "needs human review (nothing to score)", "failure"},
	}
	for _, tt := range tests {
		r := result(tt.mode, tt.v)
		r.ShowScores = true
		if got := Title(r); got != tt.title {
			t.Errorf("title = %q, want %q", got, tt.title)
		}
		if got := Conclusion(r); got != tt.conclusion {
			t.Errorf("%s %s conclusion = %q, want %q", tt.mode, tt.v.Verdict, got, tt.conclusion)
		}
	}
}

// With hidden scores, no veto or cap value, and no aggregate, reaches public
// output: they'd let an author tune a PR until it clears the bar.
func TestHiddenScores(t *testing.T) {
	r := result(config.Enforce, verdict.Result{Verdict: verdict.Deny, Raw: verdict.NeedsHuman, Aggregate: 0.27, Vetoed: "malicious", Caps: []verdict.CapKind{verdict.CapQuestion}})
	secret := map[string]float64{"malicious": 0.27, "injection": 0.21, "hygiene": 0.83, "breaks": 0.66}
	for i, q := range r.Questions {
		if v, ok := secret[q.ID]; ok {
			r.Questions[i].Value = v
		}
	}
	r.Questions[0].Value = 0.58 // aligned stays visible
	for name, out := range map[string]string{"comment": Comment(r), "summary": Summary(r), "title": Title(r), "text": Text(r)} {
		for _, leak := range []string{"0.27", "0.21", "0.83", "0.66"} {
			if strings.Contains(out, leak) {
				t.Errorf("%s leaks %s:\n%s", name, leak, out)
			}
		}
	}
	c := Comment(r)
	for _, want := range []string{"| malicious (veto) | | deny |", "| injection (cap) | | hold |", "| hygiene (cap) | | pass |", "| breaks (cap) | | hold |", "0.58"} {
		if !strings.Contains(c, want) {
			t.Errorf("comment missing %q:\n%s", want, c)
		}
	}
	// Hidden rows come after the gates, holds first, whatever their values.
	if i, j, k := strings.Index(c, "scoped"), strings.Index(c, "malicious"), strings.Index(c, "hygiene"); i > j || j > k {
		t.Errorf("row order leaks values:\n%s", c)
	}
}

func TestText_StripsControlCharacters(t *testing.T) {
	r := result(config.Shadow, verdict.Result{Verdict: verdict.NeedsHuman, Aggregate: 0.5})
	r.Excluded = []prstate.File{{Path: "evil\x1b]0;pwned\x07.go"}}
	if strings.ContainsAny(Text(r), "\x1b\x07") {
		t.Error("terminal output must not carry control characters")
	}
}

func TestComment_ModelChanged(t *testing.T) {
	r := result(config.Enforce, verdict.Result{Verdict: verdict.NeedsHuman, Raw: verdict.Approve, Aggregate: 0.97, Caps: []verdict.CapKind{verdict.ModelChanged}})
	r.Served = []string{"typesafe/jev-1.13-20261101"}
	got := Comment(r)
	for _, want := range []string{"the model version changed since calibration", "typesafe/jev-1.13-20261101"} {
		if !strings.Contains(got, want) {
			t.Errorf("comment missing %q:\n%s", want, got)
		}
	}
	b, err := JSON(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"served": [`) {
		t.Errorf("JSON missing served:\n%s", b)
	}
}

func TestJSON(t *testing.T) {
	b, err := JSON(result(config.Shadow, verdict.Result{Verdict: verdict.NeedsHuman, Aggregate: 0.42}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"verdict": "needs-human"`, `"id": "tested"`, `"head_sha": "0123456789abcdef"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON missing %s", want)
		}
	}
}

// Caps only hold approve, so on a deny they read as extra context.
func TestComment_CapsOnDeny(t *testing.T) {
	r := vetoed()
	r.Verdict.Caps = []verdict.CapKind{verdict.UntrustedAuthor}
	c := Comment(r)
	if strings.Contains(c, "Held at needs-human") || !strings.Contains(c, "Would also hold approve because:") {
		t.Errorf("comment:\n%s", c)
	}
}
