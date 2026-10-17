package replay

import (
	"math"
	"strings"
	"testing"

	"github.com/Temikus/lgtm/internal/rubric"
	"github.com/Temikus/lgtm/internal/verdict"
)

func pt(label Label, agg float64) Point {
	return Point{Label: label, Agg: agg, Values: []verdict.Value{{ID: "g", Role: rubric.Gate, Value: agg}}}
}

func points(label Label, aggs ...float64) []Point {
	var out []Point
	for _, a := range aggs {
		out = append(out, pt(label, a))
	}
	return out
}

func listed(p Point) Point { p.Listed = true; return p }

func TestFitThresholds(t *testing.T) {
	var ps []Point
	// 30 merged PRs from 0.70 to 0.99, and 20 rejected ones, one of them at
	// 0.95 among the merged.
	for i := range 30 {
		ps = append(ps, pt(Positive, 0.70+float64(i)*0.01))
	}
	ps = append(ps, points(Negative, 0.02, 0.03, 0.04, 0.05, 0.06, 0.07, 0.08, 0.09, 0.10, 0.11,
		0.20, 0.30, 0.40, 0.50, 0.55, 0.60, 0.62, 0.64, 0.66, 0.95)...)
	// Low-confidence and unlabelled points must not move the fit.
	low := pt(Negative, 0.99)
	low.Low = true
	ps = append(ps, low, pt(Unlabelled, 0.99))

	f := FitThresholds(ps, DefaultRates)
	if !f.OK {
		t.Fatalf("fit failed: %s", f.Why)
	}
	// The lowest threshold that approves at most 5% of 20 negatives (one) is
	// 0.70: it lets in only the 0.95. 0.66 would let in two.
	if !near(f.ApproveAt, 0.70) || f.FalseApproved != 1 || !near(f.FalseApprove, 0.05) {
		t.Errorf("approve_at = %v, false approvals %d (%v), want 0.70 and 1", f.ApproveAt, f.FalseApproved, f.FalseApprove)
	}
	if !near(f.ApproveRecall, 1) {
		t.Errorf("recall = %v, want every merged PR approved", f.ApproveRecall)
	}
	// No merged PR is below 0.70, so deny_at is the highest candidate under
	// it: 0.66.
	if !near(f.DenyAt, 0.66) || f.FalseDenied != 0 {
		t.Errorf("deny_at = %v, false denials %d, want 0.66 and 0", f.DenyAt, f.FalseDenied)
	}
	if f.NLabelled != 50 || f.NNegative != 20 || f.NPositive != 30 {
		t.Errorf("counts = %d/%d/%d", f.NLabelled, f.NPositive, f.NNegative)
	}
}

// Precision chases the base rate: with 96% merged, approving almost
// everything reaches 95% precision. Rates per class don't.
func TestFitThresholds_IgnoresBaseRate(t *testing.T) {
	var ps []Point
	for i := range 480 {
		ps = append(ps, pt(Positive, 0.05+0.9*float64(i)/480))
	}
	for i := range 20 {
		ps = append(ps, pt(Negative, 0.05+0.9*float64(i)/20))
	}
	f := FitThresholds(ps, DefaultRates)
	if !f.OK {
		t.Fatalf("fit failed: %s", f.Why)
	}
	if f.ApproveAt < 0.85 || f.FalseApproved > 1 {
		t.Errorf("approve_at = %v with %d false approvals; scores that don't separate must not approve low", f.ApproveAt, f.FalseApproved)
	}
}

func TestFitThresholds_Lists(t *testing.T) {
	var ps []Point
	for i := range 20 {
		ps = append(ps, pt(Positive, 0.90+float64(i)*0.004), pt(Negative, 0.10+float64(i)*0.01))
	}
	base := FitThresholds(ps, DefaultRates)
	if !base.OK {
		t.Fatal(base.Why)
	}
	// A known-bad listed PR at 0.99 would be a false approval in history,
	// but listed negatives count towards neither rate.
	withBad := FitThresholds(append(ps, listed(pt(Negative, 0.99))), DefaultRates)
	if withBad.ApproveAt != base.ApproveAt || withBad.NNegative != 20 {
		t.Errorf("a listed negative moved the fit: %+v", withBad)
	}
	// Known-good listed PRs count towards false denials: one at 0.05 among
	// 21 positives is 4.8%, so deny_at must drop below it.
	withGood := FitThresholds(append(ps, listed(pt(Positive, 0.05))), DefaultRates)
	if withGood.DenyAt >= 0.05 || withGood.FalseDenied != 0 {
		t.Errorf("deny_at = %v, false denials %d; want below the known-good PR", withGood.DenyAt, withGood.FalseDenied)
	}
	if withGood.ApproveAt != base.ApproveAt {
		t.Errorf("a listed positive moved approve_at: %v vs %v", withGood.ApproveAt, base.ApproveAt)
	}
}

func TestFitThresholds_Refuses(t *testing.T) {
	var many []Point
	for range 25 {
		many = append(many, pt(Positive, 0.5))
	}
	tests := []struct {
		name, why string
		ps        []Point
	}{
		{"no negatives", "20 rejected", points(Positive, 0.99, 0.98, 0.97, 0.96, 0.95, 0.5)},
		{"too few negatives", "20 rejected", append(points(Positive, 0.99, 0.98, 0.97, 0.96, 0.95), points(Negative, 0.01, 0.02)...)},
		{"scores don't separate", "false approvals", append(many, points(Negative, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5)...)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := FitThresholds(tt.ps, DefaultRates)
			if f.OK || !strings.Contains(f.Why, tt.why) {
				t.Errorf("fit = %+v, want refusal mentioning %q", f, tt.why)
			}
		})
	}
}

func TestMetrics(t *testing.T) {
	ps := append(points(Positive, 0.95, 0.95, 0.5, 0.05), points(Negative, 0.95, 0.05, 0.05)...)
	m := Measure(ps, verdict.Policy{ApproveAt: 0.9, DenyAt: 0.1})
	if m.Approved != 3 || m.Denied != 3 || m.NeedsHuman != 1 {
		t.Errorf("counts = %d/%d/%d", m.Approved, m.Denied, m.NeedsHuman)
	}
	if !near(m.ApprovePrecision, 2.0/3) || !near(m.ApproveRecall, 2.0/4) {
		t.Errorf("approve precision/recall = %v/%v", m.ApprovePrecision, m.ApproveRecall)
	}
	if !near(m.DenyPrecision, 2.0/3) || !near(m.DenyRecall, 2.0/3) {
		t.Errorf("deny precision/recall = %v/%v", m.DenyPrecision, m.DenyRecall)
	}
	if len(m.Disagreements) != 2 {
		t.Errorf("disagreements = %d, want approve-negative and deny-positive", len(m.Disagreements))
	}
}

func TestMetrics_CapsApply(t *testing.T) {
	p := pt(Positive, 0.99)
	p.Untrusted = true
	m := Measure([]Point{p}, verdict.Policy{ApproveAt: 0.9, DenyAt: 0.1})
	if m.Approved != 0 || m.NeedsHuman != 1 {
		t.Errorf("an untrusted PR must not count as approved: %+v", m)
	}
}

func TestReliability(t *testing.T) {
	// Perfectly calibrated: score 0.95 → 95% positive would need 20 points;
	// here 0.0 and 1.0 scores match their labels exactly, so ECE is 0.
	ps := append(points(Positive, 1, 1, 1), points(Negative, 0, 0)...)
	r := Reliability(ps, []rubric.Question{{ID: "g", Role: rubric.Gate}})
	if len(r) != 1 || r[0].ECE != 0 {
		t.Fatalf("reliability = %+v", r)
	}
	// Overconfident: every PR scores 0.9 but half are negative.
	ps = append(points(Positive, 0.9, 0.9), points(Negative, 0.9, 0.9)...)
	r = Reliability(ps, []rubric.Question{{ID: "g", Role: rubric.Gate}})
	if !near(r[0].ECE, 0.4) {
		t.Errorf("ECE = %v, want 0.4", r[0].ECE)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCapSweeps(t *testing.T) {
	capPt := func(label Label, v float64, kind string, isListed bool) Point {
		return Point{Label: label, Reason: kind, Listed: isListed, Agg: 0.9, Values: []verdict.Value{
			{ID: "g", Role: rubric.Gate, Value: 0.9}, {ID: "injection", Role: rubric.Cap, Value: v},
		}}
	}
	ps := []Point{
		capPt(Positive, 0.45, "", false), capPt(Positive, 0.8, "", false),
		capPt(Positive, 0.25, "ai-text", true),
		capPt(Negative, 0.01, "injection", true), capPt(Negative, 0.15, "injection", true), capPt(Negative, 0.9, "attack", true),
	}
	qs := []rubric.Question{{ID: "g", Role: rubric.Gate}, {ID: "injection", Role: rubric.Cap}}
	sw := CapSweeps(ps, qs, 0.35)
	if len(sw) != 1 || sw[0].Question != "injection" || len(sw[0].Rows) != len(capSweepAt)+1 {
		t.Fatalf("sweeps = %+v", sw)
	}
	var at035, at05 CapRow
	for _, r := range sw[0].Rows {
		switch r.At {
		case 0.35:
			at035 = r
		case 0.5:
			at05 = r
		}
	}
	if at035.MergedHeld != 0 || at035.GoodHeld != 1 || at035.Caught["injection"] != [2]int{2, 2} || at035.Caught["attack"] != [2]int{0, 1} {
		t.Errorf("at 0.35: %+v", at035)
	}
	if at05.MergedHeld != 1 || at05.Merged != 2 {
		t.Errorf("at 0.5: %+v", at05)
	}
}
