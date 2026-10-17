// Package rubric defines the questions LGTM asks about a PR, how repo config
// changes them, and how each answer becomes a 0-1 value.
package rubric

import (
	"fmt"
	"slices"

	"github.com/Temikus/lgtm/internal/jev"
)

// Role decides how a question's value affects the verdict.
type Role string

const (
	Gate Role = "gate" // enters the aggregate
	Veto Role = "veto" // a gate that also denies below veto_at, whatever the aggregate
	Cap  Role = "cap"  // below cap_at caps approve at needs-human; never denies
	Info Role = "info" // reported only
)

// Gating reports whether the role enters the aggregate.
func (r Role) Gating() bool { return r == Gate || r == Veto }

// Context decides which part of the PR state a question sees.
type Context string

const (
	Full Context = "full" // the whole state
	Diff Context = "diff" // files and excluded only: no title, body, or issue titles
)

// Question is a jev.Question plus how LGTM uses its answer.
type Question struct {
	jev.Question
	ID      string
	Role    Role
	Context Context
	// DiffLocal questions can be asked over patch batches and min-combined
	// when the whole diff doesn't fit in one call.
	DiffLocal bool
	Pass      []string // choice: options whose probability mass counts as a pass
	Invert    bool     // score: 1 at the first level instead of the last
	Weight    float64  // weighted_mean aggregate; 0 means 1
	CapAt     float64  // cap questions: holds approve below this; 0 means the global cap_at
}

// Normalise maps an answer to [0,1], where 1 is the passing end.
func (q Question) Normalise(a jev.Answer) (float64, error) {
	switch q.Type {
	case jev.Noul:
		return a.Noul, nil
	case jev.Score:
		v := a.Score / float64(len(q.Levels)-1)
		if q.Invert {
			v = 1 - v
		}
		return clamp(v), nil
	case jev.Choice:
		if len(q.Pass) == 0 {
			return 0, fmt.Errorf("choice question %q has no pass options", q.ID)
		}
		if len(a.Probabilities) == 0 {
			if slices.Contains(q.Pass, a.Choice) {
				return 1, nil
			}
			return 0, nil
		}
		var mass float64
		for _, name := range q.Pass {
			mass += a.Probabilities[name]
		}
		return clamp(mass), nil
	}
	return 0, fmt.Errorf("question %q: unknown type %q", q.ID, q.Type)
}

// Reversed returns a copy with choice options in reverse order, for the
// borderline consistency check.
func (q Question) Reversed() Question {
	q.Choices = slices.Clone(q.Choices)
	slices.Reverse(q.Choices)
	return q
}

func clamp(v float64) float64 { return min(max(v, 0), 1) }

// Validate checks shapes and that every gate or cap choice has valid pass
// options. Repo-level limits (count, length) live in package config.
func Validate(qs []Question) error {
	m := make(map[string]jev.Question, len(qs))
	for _, q := range qs {
		if _, dup := m[q.ID]; dup {
			return fmt.Errorf("duplicate question %q", q.ID)
		}
		m[q.ID] = q.Question
		if err := validateUse(q); err != nil {
			return err
		}
	}
	return jev.ValidateQuestions(m)
}

func validateUse(q Question) error {
	switch q.Role {
	case Gate, Veto, Cap, Info:
	default:
		return fmt.Errorf("question %q: unknown role %q", q.ID, q.Role)
	}
	switch q.Context {
	case Full, Diff:
	default:
		return fmt.Errorf("question %q: context must be full or diff, got %q", q.ID, q.Context)
	}
	if q.Type != jev.Choice {
		if len(q.Pass) > 0 {
			return fmt.Errorf("question %q: pass only applies to choice questions", q.ID)
		}
		return nil
	}
	if q.Role != Info && len(q.Pass) == 0 {
		return fmt.Errorf("question %q: a gating choice question needs pass options", q.ID)
	}
	for _, p := range q.Pass {
		if !q.Has(p) {
			return fmt.Errorf("question %q: pass option %q is not an option", q.ID, p)
		}
	}
	return nil
}

// Override is one [questions.<id>] table from repo config. Nil fields keep
// the default.
type Override struct {
	Enabled      *bool        `toml:"enabled"`
	Type         *string      `toml:"type"`
	Instructions *string      `toml:"instructions"`
	Gate         *bool        `toml:"gate"`
	Veto         *bool        `toml:"veto"`
	Cap          *bool        `toml:"cap"`
	Context      *string      `toml:"context"`
	DiffLocal    *bool        `toml:"diff_local"`
	Options      []jev.Option `toml:"options"`
	Levels       []string     `toml:"levels"`
	Pass         []string     `toml:"pass"`
	Invert       *bool        `toml:"invert"`
	Weight       *float64     `toml:"weight"`
	CapAt        *float64     `toml:"cap_at"`
}

// Merge applies overrides to the defaults. Defaults keep their order; new
// questions follow, sorted by ID.
func Merge(defaults []Question, overrides map[string]Override) ([]Question, error) {
	out := make([]Question, 0, len(defaults)+len(overrides))
	known := make(map[string]bool, len(defaults))
	for _, d := range defaults {
		known[d.ID] = true
		ov, ok := overrides[d.ID]
		if !ok {
			out = append(out, d)
			continue
		}
		if ov.Enabled != nil && !*ov.Enabled {
			continue
		}
		q, err := apply(d, ov)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	for _, id := range jev.SortedIDs(overrides) {
		ov := overrides[id]
		if known[id] || (ov.Enabled != nil && !*ov.Enabled) {
			continue
		}
		if ov.Type == nil || ov.Instructions == nil {
			return nil, fmt.Errorf("question %q: a new question needs type and instructions", id)
		}
		q, err := apply(Question{ID: id, Role: Info, Context: Full}, ov)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, Validate(out)
}

func apply(q Question, ov Override) (Question, error) {
	if on(ov.Gate)+on(ov.Veto)+on(ov.Cap) > 1 {
		return q, fmt.Errorf("question %q: gate, veto, and cap are exclusive", q.ID)
	}
	if ov.Type != nil && jev.QuestionType(*ov.Type) != q.Type {
		q.Type = jev.QuestionType(*ov.Type)
		// The old shape doesn't fit the new type; it must come from the override.
		q.Choices, q.Levels, q.Pass = nil, nil, nil
	}
	setIf(&q.Instructions, ov.Instructions)
	setIf(&q.DiffLocal, ov.DiffLocal)
	setIf(&q.Invert, ov.Invert)
	setIf(&q.Weight, ov.Weight)
	setIf(&q.CapAt, ov.CapAt)
	if ov.Context != nil {
		q.Context = Context(*ov.Context)
	}
	q.Role = role(q.Role, ov)
	if ov.Options != nil {
		q.Choices = ov.Options
	}
	if ov.Levels != nil {
		q.Levels = ov.Levels
	}
	if ov.Pass != nil {
		q.Pass = ov.Pass
	}
	if q.Weight < 0 {
		return q, fmt.Errorf("question %q: weight must not be negative", q.ID)
	}
	if q.CapAt < 0 || q.CapAt >= 1 {
		return q, fmt.Errorf("question %q: cap_at must be in (0, 1), got %v", q.ID, q.CapAt)
	}
	return q, nil
}

// role applies gate/veto/cap overrides. Turning off a question's current
// role makes it informational.
func role(cur Role, ov Override) Role {
	switch {
	case on(ov.Gate) == 1:
		return Gate
	case on(ov.Veto) == 1:
		return Veto
	case on(ov.Cap) == 1:
		return Cap
	case (ov.Gate != nil && cur == Gate) || (ov.Veto != nil && cur == Veto) || (ov.Cap != nil && cur == Cap):
		return Info
	}
	return cur
}

func on(b *bool) int {
	if b != nil && *b {
		return 1
	}
	return 0
}

func setIf[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

// JevQuestions keys the questions by ID for a decision request.
func JevQuestions(qs []Question) map[string]jev.Question {
	m := make(map[string]jev.Question, len(qs))
	for _, q := range qs {
		m[q.ID] = q.Question
	}
	return m
}
