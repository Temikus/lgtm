package replay

import (
	"math"
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

func TestFitThresholds(t *testing.T) {
	var ps []Point
	ps = append(ps, points(Positive, 0.99, 0.98, 0.97, 0.96, 0.95, 0.94, 0.93, 0.92, 0.91, 0.90, 0.60)...)
	ps = append(ps, points(Negative, 0.02, 0.03, 0.05, 0.08, 0.10, 0.30, 0.85)...)
	// Low-confidence and unlabelled points must not move the fit.
	low := pt(Negative, 0.99)
	low.Low = true
	ps = append(ps, low, pt(Unlabelled, 0.99))

	f := FitThresholds(ps, 0.95)
	if !f.OK {
		t.Fatalf("fit failed: %s", f.Why)
	}
	// 0.90 is the lowest threshold whose approvals are all positive; 0.85
	// would let a negative in (10/11 = 0.91).
	if f.ApproveAt != 0.90 || f.ApprovePrecision != 1 {
		t.Errorf("approve_at = %v (precision %v), want 0.90", f.ApproveAt, f.ApprovePrecision)
	}
	// 0.30 is the highest deny threshold with only negatives at or below
	// it; 0.60 would deny a positive.
	if f.DenyAt != 0.30 {
		t.Errorf("deny_at = %v, want 0.30", f.DenyAt)
	}
	if f.NLabelled != 18 {
		t.Errorf("n_labelled = %d, want 18", f.NLabelled)
	}
}

func TestFitThresholds_Refuses(t *testing.T) {
	tests := []struct {
		name string
		ps   []Point
	}{
		{"no negatives", points(Positive, 0.99, 0.98, 0.97, 0.96, 0.95, 0.5)},
		{"too few negatives", append(points(Positive, 0.99, 0.98, 0.97, 0.96, 0.95), points(Negative, 0.01, 0.02)...)},
		{"scores don't separate", append(points(Positive, 0.5, 0.6, 0.7, 0.8, 0.9), points(Negative, 0.5, 0.6, 0.7, 0.8, 0.9)...)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if f := FitThresholds(tt.ps, 0.95); f.OK {
				t.Errorf("fit = %+v, want refusal", f)
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
