package replay

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/Temikus/lgtm/internal/config"
	"github.com/Temikus/lgtm/internal/jev"
)

var newCal = config.Calibration{
	Model: "typesafe/jev-1.13", Snapshot: "typesafe/jev-1.13-20260917",
	Date: toml.LocalDate{Year: 2026, Month: 10, Day: 16}, NLabelled: 212,
	ApproveAt: 0.9312, DenyAt: 0.08, ApprovePrecision: 0.96,
}

func TestSetCalibration_ReplacesTableKeepsComments(t *testing.T) {
	src := `# LGTM config.
mode = "shadow" # still shadow

# Written by ` + "`lgtm replay --write`" + `.
[calibration]
model = "typesafe/jev-1.12"
n_labelled = 10
approve_at = 0.8
deny_at = 0.2

# Make described a gate.
[questions.described]
gate = true
`
	got, err := SetCalibration([]byte(src), newCal)
	if err != nil {
		t.Fatal(err)
	}
	want := `# LGTM config.
mode = "shadow" # still shadow

# Written by ` + "`lgtm replay --write`" + `.
[calibration]
model = "typesafe/jev-1.13"
snapshot = "typesafe/jev-1.13-20260917"
date = 2026-10-16
n_labelled = 212
approve_at = 0.9312
deny_at = 0.08
approve_precision = 0.96

# Make described a gate.
[questions.described]
gate = true
`
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestSetCalibration_AppendsWhenMissing(t *testing.T) {
	for _, src := range []string{"", "mode = \"shadow\"\n", "[questions.described]\ngate = true\n\n\n"} {
		got, err := SetCalibration([]byte(src), newCal)
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		if !strings.HasPrefix(string(got), strings.TrimRight(src, "\n")) || !strings.Contains(string(got), "[calibration]\nmodel = ") {
			t.Errorf("%q gave:\n%s", src, got)
		}
	}
}

func TestSetCalibration_RefusesWhatItCannotEdit(t *testing.T) {
	// An inline table isn't a [calibration] header, so appending one would
	// define the key twice.
	if _, err := SetCalibration([]byte("calibration = { model = \"x\" }\n"), newCal); err == nil {
		t.Error("want an error for an inline calibration table")
	}
}

type costDecider struct {
	calls int
	cost  float64
}

func (c *costDecider) Decide(context.Context, jev.Request) (*jev.Response, error) {
	c.calls++
	return &jev.Response{CostUSD: c.cost}, nil
}

func TestBudget_StopsBeforeOverspend(t *testing.T) {
	d := &costDecider{cost: 0.3}
	b := &Budget{Decider: d, LimitUSD: 1}
	var err error
	for range 10 {
		if _, err = b.Decide(context.Background(), jev.Request{}); err != nil {
			break
		}
	}
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("err = %v, want ErrBudget", err)
	}
	// 3 calls spend 0.9; a 4th at the 0.3 average would pass 1.
	if d.calls != 3 || b.Spent() > 1 {
		t.Errorf("calls = %d, spent = %v", d.calls, b.Spent())
	}
}

func TestBudget_PricesTokensWithoutReportedCost(t *testing.T) {
	r := &jev.Response{Usage: jev.Usage{InputTokens: 1_000_000}}
	if got := cost(r); got != fallbackUSDPerMTok {
		t.Errorf("cost = %v", got)
	}
}
