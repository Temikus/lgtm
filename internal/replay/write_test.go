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
	calls  int
	cost   float64
	tokens int
}

func (c *costDecider) Decide(context.Context, jev.Request) (*jev.Response, error) {
	c.calls++
	return &jev.Response{CostUSD: c.cost, Usage: jev.Usage{InputTokens: c.tokens}}, nil
}

func sized(bytes int) jev.Request {
	return jev.Request{State: strings.Repeat("x", bytes-2)} // JSON adds two quotes
}

func TestBudget_StopsBeforeOverspend(t *testing.T) {
	// Each call is 300k tokens and the provider bills $0.30, $1 per million.
	d := &costDecider{cost: 0.3, tokens: 300_000}
	b := &Budget{Decider: d, LimitUSD: 1}
	var err error
	for range 10 {
		if _, err = b.Decide(context.Background(), sized(900_000)); err != nil {
			break
		}
	}
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("err = %v, want ErrBudget", err)
	}
	// 3 calls spend 0.9; a 4th at the learned price would pass 1.
	if d.calls != 3 || b.Spent() > 1 {
		t.Errorf("calls = %d, spent = %v", d.calls, b.Spent())
	}
}

// The first call has no history, so the fallback price must stop a request
// that is too big for the budget before it is sent.
func TestBudget_FirstCallIsEstimated(t *testing.T) {
	d := &costDecider{cost: 0.001}
	b := &Budget{Decider: d, LimitUSD: 0.01}
	// 3 MB is about 1M tokens, $0.10 at the fallback price.
	if _, err := b.Decide(context.Background(), sized(3_000_000)); !errors.Is(err, ErrBudget) || d.calls != 0 {
		t.Errorf("err = %v after %d calls, want ErrBudget before any call", err, d.calls)
	}
	if _, err := b.Decide(context.Background(), sized(3_000)); err != nil || d.calls != 1 {
		t.Errorf("a small call should go through: %v", err)
	}
}

// Calls in flight hold their reservation, so parallel calls can't all slip
// under the limit.
func TestBudget_ReservesInFlight(t *testing.T) {
	release := make(chan struct{})
	d := &blockingDecider{started: make(chan struct{}), release: release}
	b := &Budget{Decider: d, LimitUSD: 0.15}
	req := sized(3_000_000) // ~$0.10 each at the fallback price
	done := make(chan error)
	go func() { _, err := b.Decide(context.Background(), req); done <- err }()
	<-d.started
	if _, err := b.Decide(context.Background(), req); !errors.Is(err, ErrBudget) {
		t.Errorf("second call while the first is in flight: err = %v, want ErrBudget", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type blockingDecider struct {
	started chan struct{}
	release chan struct{}
}

func (d *blockingDecider) Decide(context.Context, jev.Request) (*jev.Response, error) {
	d.started <- struct{}{}
	<-d.release
	return &jev.Response{CostUSD: 0.1}, nil
}

func TestBudget_PricesTokensWithoutReportedCost(t *testing.T) {
	r := &jev.Response{Usage: jev.Usage{InputTokens: 1_000_000}}
	if got := cost(r); got != fallbackUSDPerMTok {
		t.Errorf("cost = %v", got)
	}
}
