// Package verdict turns normalised answers into approve, deny, or
// needs-human. Every failure, ambiguity, or missing input resolves to
// needs-human; nothing resolves to approve by default.
package verdict

import (
	"errors"
	"fmt"
	"math"

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
	UntrustedAuthor CapKind = "untrusted-author"
	ModelChanged    CapKind = "model-changed"
	Borderline      CapKind = "borderline-inconsistent"
)

// Policy holds the thresholds in effect.
type Policy struct {
	ApproveAt float64
	DenyAt    float64
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
	Err    error
}

// Input is everything the verdict depends on.
type Input struct {
	Values    []Value
	Failure   error // provider error, timeout, oversized input, empty file set
	Partial   bool
	Untrusted bool
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
	Failure   error
}

// Decide applies the aggregate, thresholds, then caps in order: cap
// question, partial view, untrusted author, model change, borderline
// consistency. A cap
// turns approve into needs-human and leaves deny alone.
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

	for _, v := range in.Values {
		if v.Role == rubric.Cap && v.Value < p.ApproveAt {
			r.Caps = append(r.Caps, CapQuestion)
			break
		}
	}
	if in.Partial {
		r.Caps = append(r.Caps, PartialView)
	}
	if in.Untrusted {
		r.Caps = append(r.Caps, UntrustedAuthor)
	}
	if in.ModelChanged {
		r.Caps = append(r.Caps, ModelChanged)
	}
	if r.Verdict == Approve && len(r.Caps) == 0 && agg < p.ApproveAt+p.Margin && !holds(in.Consistency) {
		r.Caps = append(r.Caps, Borderline)
	}
	if r.Verdict == Approve && len(r.Caps) > 0 {
		r.Verdict = NeedsHuman
	}
	return r
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
		if v.Role == rubric.Gate {
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
