// Package verdict turns normalised answers into approve, deny, or
// needs-human. Every failure, ambiguity, or missing input resolves to
// needs-human; nothing resolves to approve by default.
package verdict

import (
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/Temikus/lgtm/internal/rubric"
)

// Verdict is the outcome for a PR.
type Verdict string

const (
	Approve    Verdict = "approve"
	Deny       Verdict = "deny"
	NeedsHuman Verdict = "needs-human"
)

// Aggregate combines gating values.
type Aggregate string

const (
	Min          Aggregate = "min"
	WeightedMean Aggregate = "weighted_mean"
)

// CapKind names a rule that turned, or would turn, approve into needs-human.
type CapKind string

const (
	CapQuestion     CapKind = "cap-question"
	PartialView     CapKind = "partial-view"
	LockfileSource  CapKind = "lockfile-source"
	UntrustedAuthor CapKind = "untrusted-author"
	ModelChanged    CapKind = "model-changed"
	Borderline      CapKind = "borderline-inconsistent"
)

// Policy holds the thresholds in effect.
type Policy struct {
	ApproveAt float64
	DenyAt    float64
	CapAt     float64 // a cap question below this holds approve
	VetoAt    float64 // a veto question below this denies
	Margin    float64 // borderline band above ApproveAt
	Aggregate Aggregate
}

// Value is one question's normalised answer. Err marks a missing or unusable
// answer.
type Value struct {
	ID     string
	Role   rubric.Role
	Value  float64
	Weight float64 // weighted_mean only; 0 means 1
	CapAt  float64 // cap questions: 0 means Policy.CapAt
	Err    error
}

// Input is everything the verdict depends on.
type Input struct {
	Values  []Value
	Failure error // provider error, timeout, oversized input, empty file set
	Partial bool
	// LockSource means a lockfile change pulls a package from outside the
	// allowed registries.
	LockSource bool
	Untrusted  bool
	// ModelChanged means a reply named a dated model version other than
	// the one the thresholds were calibrated on.
	ModelChanged bool
	// Consistency re-asks gating and cap choice questions with options
	// reversed and reports whether every answer held. It runs only for a
	// borderline approve that no earlier cap stopped. Nil counts as failed.
	Consistency func() (bool, error)
}

// Result is the decision and why.
type Result struct {
	Verdict   Verdict
	Raw       Verdict   // from thresholds alone, before caps
	Aggregate float64   // over gating questions
	Caps      []CapKind // every cap whose condition held, in rule order
	Vetoed    string    // the veto question that forced deny, if any
	Failure   error
}

// Decide applies the aggregate and thresholds, then a veto, then caps in
// order: cap question, partial view, lockfile source, untrusted author, model change,
// borderline consistency. A veto question below VetoAt denies outright. A
// cap turns approve into needs-human and leaves deny alone.
func Decide(p Policy, in Input) Result {
	if in.Failure != nil {
		return Result{Verdict: NeedsHuman, Raw: NeedsHuman, Failure: in.Failure}
	}
	agg, err := aggregate(p.Aggregate, in.Values)
	if err != nil {
		return Result{Verdict: NeedsHuman, Raw: NeedsHuman, Failure: err}
	}
	r := Result{Aggregate: agg, Raw: threshold(p, agg)}
	r.Verdict = r.Raw
	if r.Vetoed = vetoed(p, in.Values); r.Vetoed != "" {
		r.Verdict = Deny
	}
	r.Caps = caps(p, in)
	if r.Verdict == Approve && len(r.Caps) == 0 && agg < p.ApproveAt+p.Margin && !holds(in.Consistency) {
		r.Caps = append(r.Caps, Borderline)
	}
	if r.Verdict == Approve && len(r.Caps) > 0 {
		r.Verdict = NeedsHuman
	}
	return r
}

// caps returns the caps that don't need a model call, in rule order.
func caps(p Policy, in Input) []CapKind {
	var out []CapKind
	if slices.ContainsFunc(in.Values, func(v Value) bool { return v.Role == rubric.Cap && v.Value < v.capAt(p) }) {
		out = append(out, CapQuestion)
	}
	if in.Partial {
		out = append(out, PartialView)
	}
	if in.LockSource {
		out = append(out, LockfileSource)
	}
	if in.Untrusted {
		out = append(out, UntrustedAuthor)
	}
	if in.ModelChanged {
		out = append(out, ModelChanged)
	}
	return out
}

func (v Value) capAt(p Policy) float64 {
	if v.CapAt > 0 {
		return v.CapAt
	}
	return p.CapAt
}

// vetoed returns the lowest veto question below p.VetoAt.
func vetoed(p Policy, values []Value) string {
	id, low := "", p.VetoAt
	for _, v := range values {
		if v.Role == rubric.Veto && v.Value < low {
			id, low = v.ID, v.Value
		}
	}
	return id
}

func holds(check func() (bool, error)) bool {
	if check == nil {
		return false
	}
	ok, err := check()
	return err == nil && ok
}

func threshold(p Policy, agg float64) Verdict {
	switch {
	case agg >= p.ApproveAt:
		return Approve
	case agg <= p.DenyAt:
		return Deny
	}
	return NeedsHuman
}

// aggregate fails on any missing or invalid gate or cap value, or when there
// are no gates at all.
func aggregate(kind Aggregate, values []Value) (float64, error) {
	var gates []Value
	for _, v := range values {
		if v.Role == rubric.Info {
			continue
		}
		if v.Err != nil {
			return 0, fmt.Errorf("question %q: %w", v.ID, v.Err)
		}
		if math.IsNaN(v.Value) || v.Value < 0 || v.Value > 1 {
			return 0, fmt.Errorf("question %q: value %v outside [0,1]", v.ID, v.Value)
		}
		if v.Role.Gating() {
			gates = append(gates, v)
		}
	}
	if len(gates) == 0 {
		return 0, errors.New("no gating questions answered")
	}
	switch kind {
	case WeightedMean:
		var sum, wsum float64
		for _, g := range gates {
			w := g.Weight
			if w == 0 {
				w = 1
			}
			sum += w * g.Value
			wsum += w
		}
		return sum / wsum, nil
	default:
		m := 1.0
		for _, g := range gates {
			m = min(m, g.Value)
		}
		return m, nil
	}
}
