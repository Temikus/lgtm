package verdict

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/Temikus/lgtm/internal/rubric"
)

func gate(id string, v float64) Value { return Value{ID: id, Role: rubric.Gate, Value: v} }
func capq(id string, v float64) Value { return Value{ID: id, Role: rubric.Cap, Value: v} }
func info(id string, v float64) Value { return Value{ID: id, Role: rubric.Info, Value: v} }
func veto(id string, v float64) Value { return Value{ID: id, Role: rubric.Veto, Value: v} }

var policy = Policy{ApproveAt: 0.9, DenyAt: 0.1, CapAt: 0.3, VetoAt: 0.3, Margin: 0.05, Aggregate: Min}

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
		{name: "cap question below cap_at caps approve", in: Input{Values: []Value{gate("a", 0.97), capq("injection", 0.25)}}, want: NeedsHuman, wantCaps: []CapKind{CapQuestion}},
		{name: "cap question above cap_at but below approve_at doesn't cap", in: Input{Values: []Value{gate("a", 0.97), capq("injection", 0.35)}, Consistency: consistent}, want: Approve},
		{name: "cap question leaves deny alone", in: Input{Values: []Value{gate("a", 0.02), capq("injection", 0.01)}}, want: Deny, wantCaps: []CapKind{CapQuestion}},
		{name: "cap question never enters aggregate", in: Input{Values: []Value{gate("a", 0.5), capq("injection", 0.01)}}, want: NeedsHuman, wantAgg: 0.5, wantCaps: []CapKind{CapQuestion}},
		{name: "partial never approves", in: Input{Values: []Value{gate("a", 0.99)}, Partial: true}, want: NeedsHuman, wantCaps: []CapKind{PartialView}},
		{name: "partial can deny", in: Input{Values: []Value{gate("a", 0.01)}, Partial: true}, want: Deny, wantCaps: []CapKind{PartialView}},
		{name: "untrusted never approves", in: Input{Values: []Value{gate("a", 0.99)}, Untrusted: true}, want: NeedsHuman, wantCaps: []CapKind{UntrustedAuthor}},
		{name: "untrusted can deny", in: Input{Values: []Value{gate("a", 0.01)}, Untrusted: true}, want: Deny, wantCaps: []CapKind{UntrustedAuthor}},
		{name: "caps listed in order", in: Input{Values: []Value{gate("a", 0.99), capq("i", 0.2)}, Partial: true, LockSource: true, AutoRun: true, CIFailed: true, Sensitive: true, TooLarge: true, Untrusted: true}, want: NeedsHuman, wantCaps: []CapKind{CapQuestion, PartialView, LockfileSource, AutoRunCommand, CIFailing, SensitivePath, TooLarge, UntrustedAuthor}},
		{name: "sensitive path holds approve", in: Input{Values: []Value{gate("a", 0.99)}, Sensitive: true}, want: NeedsHuman, wantCaps: []CapKind{SensitivePath}},
		{name: "too large holds approve", in: Input{Values: []Value{gate("a", 0.99)}, TooLarge: true}, want: NeedsHuman, wantCaps: []CapKind{TooLarge}},
		{name: "failed CI holds approve", in: Input{Values: []Value{gate("a", 0.99)}, CIFailed: true}, want: NeedsHuman, wantCaps: []CapKind{CIFailing}},
		{name: "auto-run holds approve", in: Input{Values: []Value{gate("a", 0.99)}, AutoRun: true}, want: NeedsHuman, wantCaps: []CapKind{AutoRunCommand}},
		{name: "lockfile source holds approve", in: Input{Values: []Value{gate("a", 0.99)}, LockSource: true}, want: NeedsHuman, wantCaps: []CapKind{LockfileSource}},
		{name: "lockfile source leaves deny alone", in: Input{Values: []Value{gate("a", 0.01)}, LockSource: true}, want: Deny, wantCaps: []CapKind{LockfileSource}},
		{name: "model change caps approve", in: Input{Values: []Value{gate("a", 0.99)}, ModelChanged: true}, want: NeedsHuman, wantCaps: []CapKind{ModelChanged}},
		{name: "model change leaves deny alone", in: Input{Values: []Value{gate("a", 0.01)}, ModelChanged: true}, want: Deny, wantCaps: []CapKind{ModelChanged}},
		{name: "model change skips check", in: Input{Values: []Value{gate("a", 0.92)}, ModelChanged: true, Consistency: inconsistent}, want: NeedsHuman, wantCaps: []CapKind{ModelChanged}},
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
		{name: "veto enters aggregate", in: Input{Values: []Value{gate("a", 0.99), veto("safe", 0.5)}}, want: NeedsHuman, wantAgg: 0.5},
		{name: "veto below veto_at denies above deny_at", in: Input{Values: []Value{gate("a", 0.99), veto("safe", 0.2)}}, want: Deny, wantAgg: 0.2},
		{name: "veto denies a weighted mean above deny_at", in: Input{Values: []Value{gate("a", 0.99), gate("b", 0.99), veto("safe", 0.2)}}, want: Deny},
		{name: "veto at veto_at doesn't deny", in: Input{Values: []Value{gate("a", 0.99), veto("safe", 0.3)}}, want: NeedsHuman},
		{name: "veto beats caps", in: Input{Values: []Value{veto("safe", 0.05)}, Untrusted: true}, want: Deny, wantCaps: []CapKind{UntrustedAuthor}},
		{name: "failure beats veto", in: Input{Values: []Value{veto("safe", 0.01)}, Failure: errors.New("provider down")}, want: NeedsHuman},
		{name: "own cap_at holds above the global one", in: Input{Values: []Value{gate("a", 0.97), {ID: "hygiene", Role: rubric.Cap, Value: 0.7, CapAt: 0.8}}}, want: NeedsHuman, wantCaps: []CapKind{CapQuestion}},
		{name: "own cap_at passes above it", in: Input{Values: []Value{gate("a", 0.97), {ID: "hygiene", Role: rubric.Cap, Value: 0.85, CapAt: 0.8}}, Consistency: consistent}, want: Approve},
		{name: "veto denies when another question failed", in: Input{Values: []Value{veto("safe", 0.05), {ID: "i", Role: rubric.Cap, Err: errors.New("no answer")}}}, want: Deny},
		{name: "a failed veto answer is no veto", in: Input{Values: []Value{gate("a", 0.99), {ID: "safe", Role: rubric.Veto, Err: errors.New("no answer")}, {ID: "i", Role: rubric.Cap, Err: errors.New("no answer")}}}, want: NeedsHuman},
		{name: "missing veto is failure", in: Input{Values: []Value{gate("a", 0.99), {ID: "safe", Role: rubric.Veto, Err: errors.New("no answer")}}}, want: NeedsHuman},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checked := false
			if c := tt.in.Consistency; c != nil {
				tt.in.Consistency = func() (bool, error) { checked = true; return c() }
			}
			p := policy
			if strings.Contains(tt.name, "weighted mean") {
				p.Aggregate = WeightedMean
			}
			r := Decide(p, tt.in)
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

func TestDecide_VetoedNamesTheLowest(t *testing.T) {
	r := Decide(policy, Input{Values: []Value{gate("a", 0.99), veto("safe", 0.2), veto("exfil", 0.1)}})
	if r.Vetoed != "exfil" || r.Verdict != Deny {
		t.Errorf("vetoed = %q, verdict %s", r.Vetoed, r.Verdict)
	}
	if r := Decide(policy, Input{Values: []Value{gate("a", 0.01), veto("safe", 0.9)}}); r.Vetoed != "" {
		t.Errorf("a gate deny was reported as a veto: %q", r.Vetoed)
	}
}

func TestFloor(t *testing.T) {
	const a, n, d = Approve, NeedsHuman, Deny
	// want[rule] lists the published verdict for each (earlier, new) pair.
	pairs := [][2]Verdict{{a, a}, {a, n}, {a, d}, {n, a}, {n, n}, {n, d}, {d, a}, {d, n}, {d, d}}
	want := map[Rescore][]Verdict{
		RescoreLowest:    {a, n, d, n, n, d, d, d, d},
		RescoreNoApprove: {a, n, d, n, n, d, n, n, d},
		RescoreLatest:    {a, n, d, a, n, d, a, n, d},
	}
	for rule, ws := range want {
		for i, p := range pairs {
			earlier, got := p[0], p[1]
			r := Floor(rule, earlier, Result{Verdict: got})
			if r.Verdict != ws[i] {
				t.Errorf("%s: earlier %s, new %s → %s, want %s", rule, earlier, got, r.Verdict, ws[i])
			}
			if lowered := r.Verdict != got; lowered != (r.Earlier != "") {
				t.Errorf("%s: earlier %s, new %s: Earlier = %q, want it set only when lowered", rule, earlier, got, r.Earlier)
			}
		}
	}
}
