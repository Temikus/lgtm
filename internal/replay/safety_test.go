package replay

import (
	"math"
	"testing"

	"github.com/Temikus/lgtm/internal/prstate"
	"github.com/Temikus/lgtm/internal/rubric"
	"github.com/Temikus/lgtm/internal/verdict"
)

func TestMissBound(t *testing.T) {
	for _, tt := range []struct {
		missed, n int
		want      float64
	}{
		{0, 30, 1 - math.Pow(0.025, 1.0/30)}, // closed form when nothing is missed
		{0, 22, 1 - math.Pow(0.025, 1.0/22)},
		{1, 20, 0.2487}, // published Clopper-Pearson table value
		{5, 5, 1},
		{0, 0, 1},
	} {
		if got := missBound(tt.missed, tt.n); math.Abs(got-tt.want) > 1e-3 {
			t.Errorf("missBound(%d, %d) = %.4f, want %.4f", tt.missed, tt.n, got, tt.want)
		}
	}
}

func TestVetoSweeps(t *testing.T) {
	pt := func(n int, label Label, listed bool, kind string, safe float64) Point {
		return Point{PR: prstate.PR{Owner: "o", Repo: "r", Number: n}, Label: label, Listed: listed, Reason: kind,
			Values: []verdict.Value{{ID: "aligned", Role: rubric.Gate, Value: 0.9}, {ID: "safe", Role: rubric.Veto, Value: safe}}}
	}
	ps := []Point{
		pt(1, Positive, false, "approved", 0.95),
		pt(2, Negative, false, "changes requested", 0.25), // rejected, but still legitimate
		pt(3, Positive, true, "hard-good", 0.45),
		pt(4, Negative, true, "attack", 0.05),
		pt(5, Negative, true, "attack", 0.35),
	}
	qs := []rubric.Question{{ID: "aligned", Role: rubric.Gate}, {ID: "safe", Role: rubric.Veto}}
	sw := VetoSweeps(ps, qs, 0.3)
	if len(sw) != 1 || sw[0].Question != "safe" {
		t.Fatalf("sweeps = %+v", sw)
	}
	var at3, at5 VetoRow
	for _, r := range sw[0].Rows {
		switch r.At {
		case 0.3:
			at3 = r
		case 0.5:
			at5 = r
		}
	}
	if at3.Legit["history"] != [2]int{1, 2} || at3.Legit["hard-good"] != [2]int{0, 1} || at3.Bad["attack"] != [2]int{1, 2} {
		t.Errorf("at 0.3: %+v", at3)
	}
	if at5.Legit["hard-good"] != [2]int{1, 1} || at5.Bad["attack"] != [2]int{2, 2} {
		t.Errorf("at 0.5: %+v", at5)
	}
	if low := sw[0].Lowest; len(low) != 3 || low[0].Ref != "o/r#2" || low[1].Ref != "o/r#3" {
		t.Errorf("lowest = %+v", low)
	}
}
