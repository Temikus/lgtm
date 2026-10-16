package replay

import (
	"cmp"
	"fmt"
	"math"
	"slices"

	"github.com/Temikus/lgtm/internal/prstate"
	"github.com/Temikus/lgtm/internal/rubric"
	"github.com/Temikus/lgtm/internal/verdict"
)

// minSupport is the fewest labelled PRs on each side, and the fewest PRs a
// threshold must cover. Below it a precision figure means little.
const minSupport = 5

// Point is one scored, labelled PR.
type Point struct {
	PR        prstate.PR
	Number    int
	Title     string
	Label     Label
	Low       bool
	Reason    string
	Agg       float64
	Values    []verdict.Value
	Partial   bool
	Untrusted bool
}

func (p Point) fitted() bool { return p.Label != Unlabelled && !p.Low }

// Verdict re-decides p under pol. The borderline re-ask needs the model, so
// replay treats it as passed.
func (p Point) Verdict(pol verdict.Policy) verdict.Verdict {
	return verdict.Decide(pol, verdict.Input{
		Values: p.Values, Partial: p.Partial, Untrusted: p.Untrusted,
		Consistency: func() (bool, error) { return true, nil },
	}).Verdict
}

// Fit is the result of the threshold sweep.
type Fit struct {
	OK               bool
	Why              string // when !OK
	ApproveAt        float64
	DenyAt           float64
	ApprovePrecision float64
	DenyPrecision    float64
	NLabelled        int
}

// FitThresholds picks approve_at as the lowest aggregate whose approvals
// reach target precision against positives, and deny_at as the highest
// aggregate below it whose denials reach target against negatives.
func FitThresholds(ps []Point, target float64) Fit {
	var used []Point
	pos, neg := 0, 0
	for _, p := range ps {
		if !p.fitted() {
			continue
		}
		used = append(used, p)
		if p.Label == Positive {
			pos++
		} else {
			neg++
		}
	}
	f := Fit{NLabelled: len(used)}
	if pos < minSupport || neg < minSupport {
		f.Why = fmt.Sprintf("need at least %d positive and %d negative PRs, have %d and %d", minSupport, minSupport, pos, neg)
		return f
	}
	cands := candidates(used)

	for _, t := range cands {
		if prec, n := precision(used, Positive, func(a float64) bool { return a >= t }); n >= minSupport && prec >= target {
			f.ApproveAt, f.ApprovePrecision = t, prec
			break
		}
	}
	if f.ApproveAt == 0 {
		f.Why = fmt.Sprintf("no approve threshold reaches %.2f precision", target)
		return f
	}
	for _, t := range slices.Backward(cands) {
		if t >= f.ApproveAt {
			continue
		}
		if prec, n := precision(used, Negative, func(a float64) bool { return a <= t }); n >= minSupport && prec >= target {
			f.DenyAt, f.DenyPrecision = t, prec
			break
		}
	}
	if f.DenyAt == 0 {
		f.Why = fmt.Sprintf("no deny threshold below %.2f reaches %.2f precision", f.ApproveAt, target)
		return f
	}
	f.OK = true
	return f
}

// candidates are the distinct aggregates strictly inside (0, 1), which config
// validation requires of thresholds.
func candidates(ps []Point) []float64 {
	var out []float64
	for _, p := range ps {
		if p.Agg > 0 && p.Agg < 1 {
			out = append(out, p.Agg)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func precision(ps []Point, want Label, in func(float64) bool) (float64, int) {
	hit, n := 0, 0
	for _, p := range ps {
		if in(p.Agg) {
			n++
			if p.Label == want {
				hit++
			}
		}
	}
	if n == 0 {
		return 0, 0
	}
	return float64(hit) / float64(n), n
}

// Metrics compare verdicts with labels at one policy.
type Metrics struct {
	N                               int
	Approved, Denied, NeedsHuman    int
	ApprovePrecision, ApproveRecall float64
	DenyPrecision, DenyRecall       float64
	Disagreements                   []Point // approved negatives and denied positives
}

// Measure scores fitted points under pol, caps included.
func Measure(ps []Point, pol verdict.Policy) Metrics {
	var m Metrics
	var pos, neg, approvedPos, deniedNeg int
	for _, p := range ps {
		if !p.fitted() {
			continue
		}
		m.N++
		if p.Label == Positive {
			pos++
		} else {
			neg++
		}
		switch p.Verdict(pol) {
		case verdict.Approve:
			m.Approved++
			if p.Label == Positive {
				approvedPos++
			} else {
				m.Disagreements = append(m.Disagreements, p)
			}
		case verdict.Deny:
			m.Denied++
			if p.Label == Negative {
				deniedNeg++
			} else {
				m.Disagreements = append(m.Disagreements, p)
			}
		default:
			m.NeedsHuman++
		}
	}
	m.ApprovePrecision, m.ApproveRecall = ratio(approvedPos, m.Approved), ratio(approvedPos, pos)
	m.DenyPrecision, m.DenyRecall = ratio(deniedNeg, m.Denied), ratio(deniedNeg, neg)
	return m
}

func ratio(a, b int) float64 {
	if b == 0 {
		return math.NaN()
	}
	return float64(a) / float64(b)
}

// Bin is one row of a reliability table.
type Bin struct {
	Lo, Hi       float64
	N            int
	MeanScore    float64
	PositiveRate float64
}

// QuestionReliability compares one question's scores with PR outcomes.
type QuestionReliability struct {
	ID   string
	ECE  float64 // expected calibration error over 10 equal-width bins
	N    int
	Bins []Bin // non-empty bins only
}

// Reliability covers gate and cap questions. History labels whole PRs, so
// this shows how well each question tracks the PR outcome, not whether its
// own answer was right.
func Reliability(ps []Point, qs []rubric.Question) []QuestionReliability {
	var out []QuestionReliability
	for _, q := range qs {
		if q.Role == rubric.Info {
			continue
		}
		var bins [10]struct {
			n, pos int
			sum    float64
		}
		total := 0
		for _, p := range ps {
			if !p.fitted() {
				continue
			}
			i := slices.IndexFunc(p.Values, func(v verdict.Value) bool { return v.ID == q.ID && v.Err == nil })
			if i < 0 {
				continue
			}
			v := p.Values[i].Value
			b := min(int(v*10), 9)
			bins[b].n++
			bins[b].sum += v
			if p.Label == Positive {
				bins[b].pos++
			}
			total++
		}
		r := QuestionReliability{ID: q.ID, N: total}
		for i, b := range bins {
			if b.n == 0 {
				continue
			}
			bin := Bin{Lo: float64(i) / 10, Hi: float64(i+1) / 10, N: b.n, MeanScore: b.sum / float64(b.n), PositiveRate: float64(b.pos) / float64(b.n)}
			r.Bins = append(r.Bins, bin)
			r.ECE += float64(b.n) / float64(total) * math.Abs(bin.MeanScore-bin.PositiveRate)
		}
		out = append(out, r)
	}
	slices.SortStableFunc(out, func(a, b QuestionReliability) int { return cmp.Compare(b.ECE, a.ECE) })
	return out
}
