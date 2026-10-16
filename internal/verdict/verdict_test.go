package verdict

import (
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/Temikus/lgtm/internal/rubric"
)

func gate(id string, v float64) Value { return Value{ID: id, Role: rubric.Gate, Value: v} }
func capq(id string, v float64) Value { return Value{ID: id, Role: rubric.Cap, Value: v} }
func info(id string, v float64) Value { return Value{ID: id, Role: rubric.Info, Value: v} }

var policy = Policy{ApproveAt: 0.9, DenyAt: 0.1, Margin: 0.05, Aggregate: Min}

func consistent() (bool, error)   { return true, nil }
func inconsistent() (bool, error) { return false, nil }

func TestDecide(t *testing.T) {
	tests := []struct {
		name      string
		in        Input
		want      Verdict
		wantCaps  []CapKind
		wantAgg   float64
		wantCheck bool // consistency check must run
	}{
		{name: "all high approves", in: Input{Values: []Value{gate("a", 0.97), gate("b", 0.99), capq("injection", 0.99)}, Consistency: consistent}, want: Approve, wantAgg: 0.97},
		{name: "min decides", in: Input{Values: []Value{gate("a", 0.99), gate("b", 0.5)}}, want: NeedsHuman, wantAgg: 0.5},
		{name: "low gate denies", in: Input{Values: []Value{gate("a", 0.99), gate("b", 0.05)}}, want: Deny, wantAgg: 0.05},
		{name: "threshold inclusive approve", in: Input{Values: []Value{gate("a", 0.96)}, Consistency: consistent}, want: Approve},
		{name: "threshold inclusive deny", in: Input{Values: []Value{gate("a", 0.1)}}, want: Deny},
		{name: "info ignored", in: Input{Values: []Value{gate("a", 0.97), info("described", 0.0)}, Consistency: consistent}, want: Approve},
		{name: "cap question below approve_at caps approve", in: Input{Values: []Value{gate("a", 0.97), capq("injection", 0.5)}}, want: NeedsHuman, wantCaps: []CapKind{CapQuestion}},
		{name: "cap question leaves deny alone", in: Input{Values: []Value{gate("a", 0.02), capq("injection", 0.01)}}, want: Deny, wantCaps: []CapKind{CapQuestion}},
		{name: "cap question never enters aggregate", in: Input{Values: []Value{gate("a", 0.5), capq("injection", 0.01)}}, want: NeedsHuman, wantAgg: 0.5, wantCaps: []CapKind{CapQuestion}},
		{name: "partial never approves", in: Input{Values: []Value{gate("a", 0.99)}, Partial: true}, want: NeedsHuman, wantCaps: []CapKind{PartialView}},
		{name: "partial can deny", in: Input{Values: []Value{gate("a", 0.01)}, Partial: true}, want: Deny, wantCaps: []CapKind{PartialView}},
		{name: "untrusted never approves", in: Input{Values: []Value{gate("a", 0.99)}, Untrusted: true}, want: NeedsHuman, wantCaps: []CapKind{UntrustedAuthor}},
		{name: "untrusted can deny", in: Input{Values: []Value{gate("a", 0.01)}, Untrusted: true}, want: Deny, wantCaps: []CapKind{UntrustedAuthor}},
		{name: "caps listed in order", in: Input{Values: []Value{gate("a", 0.99), capq("i", 0.2)}, Partial: true, Untrusted: true}, want: NeedsHuman, wantCaps: []CapKind{CapQuestion, PartialView, UntrustedAuthor}},
		{name: "borderline consistent approves", in: Input{Values: []Value{gate("a", 0.92)}, Consistency: consistent}, want: Approve, wantCheck: true},
		{name: "borderline inconsistent caps", in: Input{Values: []Value{gate("a", 0.92)}, Consistency: inconsistent}, want: NeedsHuman, wantCaps: []CapKind{Borderline}, wantCheck: true},
		{name: "borderline check error caps", in: Input{Values: []Value{gate("a", 0.92)}, Consistency: func() (bool, error) { return false, errors.New("boom") }}, want: NeedsHuman, wantCaps: []CapKind{Borderline}, wantCheck: true},
		{name: "borderline without checker caps", in: Input{Values: []Value{gate("a", 0.92)}}, want: NeedsHuman, wantCaps: []CapKind{Borderline}},
		{name: "clear approve skips check", in: Input{Values: []Value{gate("a", 0.99)}, Consistency: inconsistent}, want: Approve},
		{name: "already capped skips check", in: Input{Values: []Value{gate("a", 0.92)}, Untrusted: true, Consistency: inconsistent}, want: NeedsHuman, wantCaps: []CapKind{UntrustedAuthor}},
		{name: "failure is needs-human", in: Input{Values: []Value{gate("a", 0.99)}, Failure: errors.New("provider down")}, want: NeedsHuman},
		{name: "failure beats deny", in: Input{Values: []Value{gate("a", 0.01)}, Failure: errors.New("provider down")}, want: NeedsHuman},
		{name: "missing gate is failure", in: Input{Values: []Value{gate("a", 0.99), {ID: "b", Role: rubric.Gate, Err: errors.New("no answer")}}}, want: NeedsHuman},
		{name: "missing cap is failure", in: Input{Values: []Value{gate("a", 0.99), {ID: "i", Role: rubric.Cap, Err: errors.New("no answer")}}}, want: NeedsHuman},
		{name: "missing info is fine", in: Input{Values: []Value{gate("a", 0.99), {ID: "d", Role: rubric.Info, Err: errors.New("no answer")}}, Consistency: consistent}, want: Approve},
		{name: "no gates is failure", in: Input{Values: []Value{info("d", 0.99)}}, want: NeedsHuman},
		{name: "NaN is failure", in: Input{Values: []Value{gate("a", math.NaN())}}, want: NeedsHuman},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checked := false
			if c := tt.in.Consistency; c != nil {
				tt.in.Consistency = func() (bool, error) { checked = true; return c() }
			}
			r := Decide(policy, tt.in)
			if r.Verdict != tt.want {
				t.Errorf("verdict = %s, want %s (result %+v)", r.Verdict, tt.want, r)
			}
			if !slices.Equal(r.Caps, tt.wantCaps) {
				t.Errorf("caps = %v, want %v", r.Caps, tt.wantCaps)
			}
			if tt.wantAgg != 0 && math.Abs(r.Aggregate-tt.wantAgg) > 1e-9 {
				t.Errorf("aggregate = %v, want %v", r.Aggregate, tt.wantAgg)
			}
			if checked != tt.wantCheck {
				t.Errorf("consistency checked = %v, want %v", checked, tt.wantCheck)
			}
		})
	}
}

func TestDecide_WeightedMean(t *testing.T) {
	p := policy
	p.Aggregate = WeightedMean
	in := Input{Values: []Value{{ID: "a", Role: rubric.Gate, Value: 1.0, Weight: 3}, {ID: "b", Role: rubric.Gate, Value: 0.6}}, Consistency: consistent}
	r := Decide(p, in)
	if math.Abs(r.Aggregate-0.9) > 1e-9 || r.Verdict != Approve {
		t.Errorf("aggregate = %v verdict = %s, want 0.9 approve", r.Aggregate, r.Verdict)
	}
}

func TestDecide_RawVerdictKept(t *testing.T) {
	r := Decide(policy, Input{Values: []Value{gate("a", 0.99)}, Untrusted: true})
	if r.Raw != Approve || r.Verdict != NeedsHuman {
		t.Errorf("raw = %s verdict = %s", r.Raw, r.Verdict)
	}
}
