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

// minSupport is the fewest merged PRs a fit needs, and the fewest an
// approve threshold must approve.
const minSupport = 5

// minNegatives is the fewest rejected history PRs a fit needs. Below it one
// false approval is already more than 5%, so the rate can't be estimated.
const minNegatives = 20

// Rates bound the fit, as shares of each class rather than precision, which
// depends on how many PRs a repo rejects.
type Rates struct {
	MaxFalseApprove float64 // share of rejected history PRs approved
	MaxFalseDeny    float64 // share of merged and known-good PRs denied
}

// DefaultRates are the --max-false-approve and --max-false-deny defaults.
var DefaultRates = Rates{MaxFalseApprove: 0.05, MaxFalseDeny: 0.01}

// Point is one scored, labelled PR.
type Point struct {
	PR         prstate.PR
	Number     int
	Title      string
	Label      Label
	Low        bool
	Reason     string
	Agg        float64
	Values     []verdict.Value
	Partial    bool
	LockSource bool
	AutoRun    bool
	Sensitive  bool
	TooLarge   bool
	Untrusted  bool
	// Listed marks a PR from a known-bad or known-good list. Known-good ones
	// count towards false denials only; known-bad ones towards neither rate.
	Listed bool
}

func (p Point) fitted() bool { return p.Label != Unlabelled && !p.Low }

// Verdict re-decides p under pol. The borderline re-ask needs the model, so
// replay treats it as passed.
func (p Point) Verdict(pol verdict.Policy) verdict.Verdict {
	return verdict.Decide(pol, verdict.Input{
		Values: p.Values, Partial: p.Partial, LockSource: p.LockSource, AutoRun: p.AutoRun, Sensitive: p.Sensitive, TooLarge: p.TooLarge, Untrusted: p.Untrusted,
		Consistency: func() (bool, error) { return true, nil },
	}).Verdict
}

// Fit is the result of the threshold sweep.
type Fit struct {
	OK        bool
	Why       string // when !OK
	ApproveAt float64
	DenyAt    float64
	// FalseApprove is the share of rejected history PRs approve_at lets in;
	// FalseDeny the share of merged and known-good PRs deny_at denies.
	FalseApprove, FalseDeny    float64
	FalseApproved, FalseDenied int
	ApproveRecall              float64 // share of merged history PRs approved
	// Precision at the fitted thresholds, on history. Reported only: it
	// depends on the base rate.
	ApprovePrecision, DenyPrecision float64
	NLabelled, NPositive, NNegative int // history PRs used
	NListedPositive                 int // known-good PRs in the false-deny rate
}

// fitSets splits points the way the fit uses them. Listed known-bad PRs
// and unfitted ones are dropped.
type fitSets struct{ hist, pos, neg, good []Point }

func splitFit(ps []Point) fitSets {
	var s fitSets
	for _, p := range ps {
		switch {
		case !p.fitted():
		case p.Listed && p.Label == Positive:
			s.good = append(s.good, p)
		case p.Listed:
		case p.Label == Positive:
			s.hist, s.pos = append(s.hist, p), append(s.pos, p)
		default:
			s.hist, s.neg = append(s.hist, p), append(s.neg, p)
		}
	}
	return s
}

// FitThresholds picks approve_at as the lowest aggregate that approves at
// most r.MaxFalseApprove of rejected history PRs, and deny_at as the highest
// aggregate below it that denies at most r.MaxFalseDeny of positives.
func FitThresholds(ps []Point, r Rates) Fit {
	s := splitFit(ps)
	f := Fit{NLabelled: len(s.hist), NPositive: len(s.pos), NNegative: len(s.neg), NListedPositive: len(s.good)}
	switch {
	case len(s.neg) < minNegatives:
		f.Why = fmt.Sprintf("need at least %d rejected PRs from history to estimate false approvals, have %d", minNegatives, len(s.neg))
		return f
	case len(s.pos) < minSupport:
		f.Why = fmt.Sprintf("need at least %d merged PRs, have %d", minSupport, len(s.pos))
		return f
	}
	cands := candidates(s.hist)
	if !f.fitApprove(s, cands, r.MaxFalseApprove) {
		f.Why = fmt.Sprintf("no approve threshold keeps false approvals at or below %s of %d rejected PRs while approving %d merged ones", pct(r.MaxFalseApprove), len(s.neg), minSupport)
		return f
	}
	f.fitDeny(s, cands, r.MaxFalseDeny)
	f.OK = true
	return f
}

func (f *Fit) fitApprove(s fitSets, cands []float64, maxRate float64) bool {
	for _, t := range cands {
		approved := countAt(s.pos, func(a float64) bool { return a >= t })
		falseApp := countAt(s.neg, func(a float64) bool { return a >= t })
		if approved >= minSupport && share(falseApp, len(s.neg)) <= maxRate {
			f.ApproveAt, f.FalseApproved, f.FalseApprove = t, falseApp, share(falseApp, len(s.neg))
			f.ApproveRecall = share(approved, len(s.pos))
			f.ApprovePrecision = share(approved, approved+falseApp)
			return true
		}
	}
	return false
}

// fitDeny always succeeds: if every candidate denies too many positives,
// deny_at goes below every PR.
func (f *Fit) fitDeny(s fitSets, cands []float64, maxRate float64) {
	positives := append(slices.Clone(s.pos), s.good...)
	for _, t := range slices.Backward(cands) {
		if t >= f.ApproveAt {
			continue
		}
		denied := countAt(positives, func(a float64) bool { return a <= t })
		if share(denied, len(positives)) <= maxRate {
			f.DenyAt, f.FalseDenied, f.FalseDeny = t, denied, share(denied, len(positives))
			f.DenyPrecision = share(countAt(s.neg, func(a float64) bool { return a <= t }), countAt(s.hist, func(a float64) bool { return a <= t }))
			return
		}
	}
	f.DenyAt = max(slices.Min(candidates(append(s.hist, s.good...)))/2, 1e-4)
}

func countAt(ps []Point, in func(float64) bool) int {
	n := 0
	for _, p := range ps {
		if in(p.Agg) {
			n++
		}
	}
	return n
}

func share(n, of int) float64 {
	if of == 0 {
		return 0
	}
	return float64(n) / float64(of)
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

// Reliability covers gate and veto questions. History labels whole PRs, so
// this shows how well each question tracks the PR outcome, not whether its
// own answer was right.
func Reliability(ps []Point, qs []rubric.Question) []QuestionReliability {
	var out []QuestionReliability
	for _, q := range qs {
		// A cap question is judged on PRs that try to game it (CapSweep),
		// not on whether a PR was merged.
		if !q.Role.Gating() {
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

// capSweepAt are the cap_at values the sweep reports, besides the
// configured one.
var capSweepAt = []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9}

// CapRow counts the PRs a cap question would hold at one cap_at.
type CapRow struct {
	At                 float64
	MergedHeld, Merged int // merged history PRs
	GoodHeld, Good     int // known-good listed PRs
	// Caught counts known-bad listed PRs held, by kind: held, then total.
	Caught map[string][2]int
}

// CapSweep is one cap question's rows. At is its configured cap_at.
type CapSweep struct {
	Question string
	At       float64
	Rows     []CapRow
}

// CapSweeps reports each cap question at each sweep value and at its own
// cap_at, which is capAt unless the question sets one. Cap questions are
// judged on PRs that try to game them, so the listed known-bad PRs matter
// more here than the merge outcome.
func CapSweeps(ps []Point, qs []rubric.Question, capAt float64) []CapSweep {
	var out []CapSweep
	for _, q := range qs {
		if q.Role != rubric.Cap {
			continue
		}
		at := capAt
		if q.CapAt > 0 {
			at = q.CapAt
		}
		ats := slices.Clone(capSweepAt)
		if !slices.Contains(ats, at) {
			ats = append(ats, at)
			slices.Sort(ats)
		}
		sw := CapSweep{Question: q.ID, At: at}
		for _, at := range ats {
			sw.Rows = append(sw.Rows, capRow(ps, q.ID, at))
		}
		out = append(out, sw)
	}
	return out
}

func capRow(ps []Point, id string, at float64) CapRow {
	row := CapRow{At: at, Caught: map[string][2]int{}}
	for _, p := range ps {
		i := slices.IndexFunc(p.Values, func(v verdict.Value) bool { return v.ID == id && v.Err == nil })
		if i < 0 || !p.fitted() {
			continue
		}
		held := p.Values[i].Value < at
		switch {
		case p.Listed && p.Label == Negative:
			c := row.Caught[p.Reason]
			c[1]++
			if held {
				c[0]++
			}
			row.Caught[p.Reason] = c
		case p.Listed:
			row.Good++
			if held {
				row.GoodHeld++
			}
		case p.Label == Positive:
			row.Merged++
			if held {
				row.MergedHeld++
			}
		}
	}
	return row
}
