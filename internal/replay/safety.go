package replay

import (
	"math"
	"slices"

	"github.com/Temikus/lgtm/internal/rubric"
	"github.com/Temikus/lgtm/internal/verdict"
)

// vetoSweepAt are the veto_at values the safety sweep reports, besides the
// configured one.
var vetoSweepAt = []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6}

// VetoRow counts what one veto_at would deny. Every history PR counts as
// legitimate here, rejected or not: a reviewer turning a PR down doesn't
// make it malicious.
type VetoRow struct {
	At float64
	// Legit and Bad count denied, then total, by group: "history", or the
	// kind of a listed PR.
	Legit, Bad map[string][2]int
}

// LowPR is a legitimate PR and its value on a veto question.
type LowPR struct {
	Ref   string
	Value float64
}

// VetoSweep is one veto question's rows and its lowest legitimate PRs,
// which are where a false denial would come from.
type VetoSweep struct {
	Question string
	Rows     []VetoRow
	Lowest   []LowPR
}

// lowestShown is how many of the lowest legitimate PRs the sweep names.
const lowestShown = 5

// VetoSweeps reports each veto question at each sweep value and at vetoAt.
func VetoSweeps(ps []Point, qs []rubric.Question, vetoAt float64) []VetoSweep {
	ats := slices.Clone(vetoSweepAt)
	if !slices.Contains(ats, vetoAt) {
		ats = append(ats, vetoAt)
		slices.Sort(ats)
	}
	var out []VetoSweep
	for _, q := range qs {
		if q.Role != rubric.Veto {
			continue
		}
		sw := VetoSweep{Question: q.ID}
		for _, at := range ats {
			sw.Rows = append(sw.Rows, vetoRow(ps, q.ID, at))
		}
		sw.Lowest = lowestLegit(ps, q.ID)
		out = append(out, sw)
	}
	return out
}

func vetoValue(p Point, id string) (float64, bool) {
	i := slices.IndexFunc(p.Values, func(v verdict.Value) bool { return v.ID == id && v.Err == nil })
	if i < 0 {
		return 0, false
	}
	return p.Values[i].Value, true
}

func vetoGroup(p Point) string {
	if p.Listed {
		return p.Reason
	}
	return "history"
}

func vetoRow(ps []Point, id string, at float64) VetoRow {
	row := VetoRow{At: at, Legit: map[string][2]int{}, Bad: map[string][2]int{}}
	for _, p := range ps {
		v, ok := vetoValue(p, id)
		if !ok {
			continue
		}
		m := row.Legit
		if p.Listed && p.Label == Negative {
			m = row.Bad
		}
		c := m[vetoGroup(p)]
		c[1]++
		if v < at {
			c[0]++
		}
		m[vetoGroup(p)] = c
	}
	return row
}

func lowestLegit(ps []Point, id string) []LowPR {
	var out []LowPR
	for _, p := range ps {
		if v, ok := vetoValue(p, id); ok && (!p.Listed || p.Label == Positive) {
			out = append(out, LowPR{Ref: listRef(p.PR), Value: v})
		}
	}
	slices.SortStableFunc(out, func(a, b LowPR) int { return cmpFloat(a.Value, b.Value) })
	return out[:min(len(out), lowestShown)]
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// missBound is the exact (Clopper-Pearson) 95% upper bound on a miss rate,
// after missed misses in n tries.
func missBound(missed, n int) float64 {
	if n == 0 {
		return 1
	}
	if missed >= n {
		return 1
	}
	lo, hi := float64(missed)/float64(n), 1.0
	for range 60 {
		mid := (lo + hi) / 2
		if binomCDF(missed, n, mid) > 0.025 {
			lo = mid
		} else {
			hi = mid
		}
	}
	return hi
}

// binomCDF is P(X <= k) for X ~ Binomial(n, p).
func binomCDF(k, n int, p float64) float64 {
	var sum float64
	for i := 0; i <= k; i++ {
		lc, _ := math.Lgamma(float64(n + 1))
		li, _ := math.Lgamma(float64(i + 1))
		lr, _ := math.Lgamma(float64(n - i + 1))
		sum += math.Exp(lc - li - lr + float64(i)*math.Log(p) + float64(n-i)*math.Log1p(-p))
	}
	return sum
}
