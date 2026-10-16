package config

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/Temikus/lgtm/internal/ghtest"
	"github.com/Temikus/lgtm/internal/rubric"
	"github.com/Temikus/lgtm/internal/verdict"
)

const prdExample = `
provider = "openrouter"
model = "typesafe/jev-1.13"
mode = "shadow"
approve_at = 0.9
deny_at = 0.1
trust_forks = false
exclude = ["**/*.lock", "go.sum", "vendor/**"]
borderline_margin = 0.05

[calibration]
model = "typesafe/jev-1.13"
date = 2026-11-02
n_labelled = 212
approve_at = 0.93
deny_at = 0.08
approve_precision = 0.96

[questions.described]
gate = true

[questions.migration_safe]
type = "noul"
gate = true
instructions = "If the diff adds a database migration, can it run without locking large tables?"
`

func TestLoad_PRDExample(t *testing.T) {
	c, err := Load([]byte(prdExample))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Calibration == nil || c.Calibration.NLabelled != 212 {
		t.Fatalf("calibration = %+v", c.Calibration)
	}
	approve, deny := c.Thresholds()
	if approve != 0.93 || deny != 0.08 {
		t.Errorf("thresholds = %v/%v; a matching calibration should win", approve, deny)
	}
	gates := 0
	for _, q := range c.Rubric {
		if q.Role == rubric.Gate {
			gates++
		}
	}
	if gates != 6 {
		t.Errorf("gates = %d, want 6 (4 default + described + migration_safe)", gates)
	}
}

func TestLoad_EmptyGivesDefaults(t *testing.T) {
	c, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider != "openrouter" || c.Model != DefaultModel || c.Mode != Shadow || c.Aggregate != verdict.Min {
		t.Errorf("defaults = %+v", c)
	}
	if a, d := c.Thresholds(); a != 0.9 || d != 0.1 {
		t.Errorf("thresholds = %v/%v", a, d)
	}
	if len(c.Exclude) == 0 || len(c.Rubric) != len(rubric.Defaults()) {
		t.Errorf("exclude = %v, rubric = %d", c.Exclude, len(c.Rubric))
	}
}

func TestLoad_ChoiceOptionsKeepOrder(t *testing.T) {
	c, err := Load([]byte(`
[questions.lang]
type = "choice"
instructions = "Which language is the diff mostly in?"
options = [{name = "go", definition = "Go"}, {name = "ts", definition = "TypeScript"}, {name = "other", definition = "Anything else"}]
pass = ["go"]
gate = true
`))
	if err != nil {
		t.Fatal(err)
	}
	q := c.Rubric[len(c.Rubric)-1]
	if q.ID != "lang" || len(q.Choices) != 3 || q.Choices[0].Name != "go" || q.Choices[2].Definition != "Anything else" {
		t.Errorf("lang = %+v", q)
	}
}

func TestLoad_Rejects(t *testing.T) {
	tests := map[string]struct{ toml, want string }{
		"base url in toml":   {`base_url = "https://evil.example"`, "LGTM_BASE_URL"},
		"http base url":      {`base_url = "http://evil.example"`, "LGTM_BASE_URL"},
		"unknown key":        {`aprove_at = 0.9`, "aprove_at"},
		"unknown provider":   {`provider = "evil"`, "provider"},
		"bad mode":           {`mode = "yolo"`, "mode"},
		"deny above approve": {"approve_at = 0.5\ndeny_at = 0.6", "deny_at"},
		"approve at one":     {"approve_at = 1.0", "approve_at"},
		"deny at zero":       {"deny_at = 0.0", "deny_at"},
		"bad aggregate":      {`aggregate = "max"`, "aggregate"},
		"no gates": {`
[questions.aligned]
gate = false
[questions.scoped]
gate = false
[questions.safe]
enabled = false
[questions.tested]
enabled = false`, "at least one gating"},
		"long instructions": {"[questions.safe]\ninstructions = \"" + strings.Repeat("x", 2001) + "\"", "2000"},
		"too many options":  {"[questions.big]\ntype = \"choice\"\ninstructions = \"x\"\noptions = [" + manyOptions(33) + "]", "32"},
		"too many questions": {func() string {
			var b strings.Builder
			for i := range 13 {
				b.WriteString("[questions.q" + string(rune('a'+i)) + "]\ntype = \"noul\"\ninstructions = \"x\"\n")
			}
			return b.String()
		}(), "20"},
		"enforce uncalibrated":   {`mode = "enforce"`, "calibration"},
		"enforce wrong model":    {"mode = \"enforce\"\n[calibration]\nmodel = \"typesafe/jev-1.12\"\nn_labelled = 10\napprove_at = 0.9\ndeny_at = 0.1", "calibration"},
		"bad calibration values": {"[calibration]\nmodel = \"typesafe/jev-1.13\"\napprove_at = 0.1\ndeny_at = 0.9", "calibration"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load([]byte(tt.toml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func manyOptions(n int) string {
	parts := make([]string, n)
	for i := range n {
		parts[i] = `{name = "o` + strings.Repeat("x", i) + `", definition = "d"}`
	}
	return strings.Join(parts, ",")
}

func TestLoad_EnforceAllowUncalibrated(t *testing.T) {
	if _, err := Load([]byte("mode = \"enforce\"\nallow_uncalibrated = true")); err != nil {
		t.Fatalf("allow_uncalibrated should permit enforce: %v", err)
	}
}

func TestThresholds_IgnoresStaleCalibrationInShadow(t *testing.T) {
	c, err := Load([]byte("approve_at = 0.95\ndeny_at = 0.05\n[calibration]\nmodel = \"typesafe/jev-1.12\"\nn_labelled = 10\napprove_at = 0.8\ndeny_at = 0.2"))
	if err != nil {
		t.Fatal(err)
	}
	if a, d := c.Thresholds(); a != 0.95 || d != 0.05 {
		t.Errorf("thresholds = %v/%v; a calibration for another model must not apply", a, d)
	}
}

func TestFetchAtSHA(t *testing.T) {
	gh := ghtest.New(t)
	var gotRef string
	gh.Mux.HandleFunc("GET /repos/o/r/contents/.github/lgtm.toml", func(w http.ResponseWriter, r *http.Request) {
		gotRef = r.URL.Query().Get("ref")
		ghtest.JSON(w, 200, map[string]any{
			"type": "file", "encoding": "base64", "path": ".github/lgtm.toml",
			"content": base64.StdEncoding.EncodeToString([]byte("approve_at = 0.95\n")),
		})
	})

	c, err := FetchAtSHA(context.Background(), gh.Client, "o", "r", "basesha")
	if err != nil {
		t.Fatal(err)
	}
	if gotRef != "basesha" {
		t.Errorf("ref = %q, want the base SHA", gotRef)
	}
	if c.ApproveAt != 0.95 {
		t.Errorf("approve_at = %v", c.ApproveAt)
	}
}

func TestFetchAtSHA_MissingFileGivesDefaults(t *testing.T) {
	gh := ghtest.New(t)
	gh.Mux.HandleFunc("GET /repos/o/r/contents/.github/lgtm.toml", ghtest.NotFound)

	c, err := FetchAtSHA(context.Background(), gh.Client, "o", "r", "basesha")
	if err != nil {
		t.Fatal(err)
	}
	if c.ApproveAt != 0.9 {
		t.Errorf("approve_at = %v, want default", c.ApproveAt)
	}
}

func TestFetchAtSHA_ServerErrorFails(t *testing.T) {
	gh := ghtest.New(t)
	gh.Mux.HandleFunc("GET /repos/o/r/contents/.github/lgtm.toml", ghtest.Reply(500, map[string]string{"message": "boom"}))

	if _, err := FetchAtSHA(context.Background(), gh.Client, "o", "r", "basesha"); err == nil {
		t.Fatal("a server error must not fall back to defaults")
	}
}
