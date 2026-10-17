package prstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Temikus/lgtm/internal/jev"
	"github.com/Temikus/lgtm/internal/rubric"
)

// MaxBatches caps patch batches per context, bounding cost per PR.
const MaxBatches = 16

const trimMarker = "\n…[trimmed]"

// PRState is the `pr` field of state.
type PRState struct {
	Title        string   `json:"title"`
	Body         string   `json:"body"`
	Base         string   `json:"base"`
	Head         string   `json:"head"`
	Labels       []string `json:"labels,omitempty"`
	LinkedIssues []string `json:"linked_issues,omitempty"`
}

// State is the JSON object sent as the decision request's state. The diff
// context has no `pr` field, so author-written prose can't sway it.
type State struct {
	PR       *PRState `json:"pr,omitempty"`
	Files    []File   `json:"files"`
	Excluded []File   `json:"excluded"`
	Note     string   `json:"note,omitempty"`
}

// State builds the full, untrimmed state for a context.
func (s *Snapshot) State(c rubric.Context) State {
	st := State{Files: s.Files, Excluded: s.Excluded}
	if st.Excluded == nil {
		st.Excluded = []File{}
	}
	if c == rubric.Full {
		st.PR = &PRState{
			Title: s.PR.Title, Body: s.PR.Body, Base: s.PR.BaseRef, Head: s.PR.HeadRef,
			Labels: s.PR.Labels, LinkedIssues: s.LinkedIssues,
		}
	}
	return st
}

// Limits are token budgets: state plus any one question, and the whole call.
type Limits struct {
	Question int
	Request  int
}

// DefaultLimits are the provider's limits.
var DefaultLimits = Limits{Question: jev.MaxQuestionTokens, Request: jev.MaxRequestTokens}

// Call is one decision request. Batch and Batches are 1-based; an unbatched
// call has Batches == 1. Values from batches of one question are min-combined.
type Call struct {
	Context   rubric.Context
	Questions []rubric.Question
	State     State
	Batch     int
	Batches   int
}

// Plan is the calls for one scoring run and what they couldn't show.
type Plan struct {
	Calls   []Call
	Partial []string
	Unseen  []Unseen
}

// Plan groups questions by context, one call per context when it fits. When
// it doesn't, diff-local questions go over patch batches and the rest over
// trimmed patches, and the plan is a partial view.
func (s *Snapshot) Plan(qs []rubric.Question, lim Limits) (*Plan, error) {
	p := &Plan{}
	for _, c := range []rubric.Context{rubric.Full, rubric.Diff} {
		var group []rubric.Question
		for _, q := range qs {
			if q.Context == c {
				group = append(group, q)
			}
		}
		if len(group) == 0 {
			continue
		}
		if err := s.planContext(p, c, group, lim); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (s *Snapshot) planContext(p *Plan, c rubric.Context, group []rubric.Question, lim Limits) error {
	st := s.State(c)
	ok, err := fits(st, group, lim)
	if err != nil {
		return err
	}
	if ok {
		p.Calls = append(p.Calls, Call{Context: c, Questions: group, State: st, Batch: 1, Batches: 1})
		return nil
	}
	var local, global []rubric.Question
	for _, q := range group {
		if q.DiffLocal {
			local = append(local, q)
		} else {
			global = append(global, q)
		}
	}
	if len(local) > 0 {
		if err := s.planBatches(p, c, local, lim); err != nil {
			return err
		}
	}
	if len(global) > 0 {
		if err := s.planTrimmed(p, c, global, lim); err != nil {
			return err
		}
	}
	return nil
}

// planBatches packs files greedily into batches that each fit.
func (s *Snapshot) planBatches(p *Plan, c rubric.Context, qs []rubric.Question, lim Limits) error {
	base := s.State(c)
	base.Files = nil
	base.Note = "This is part 99 of 99 of the diff. Other files are not shown."
	budget, err := fileBudget(base, qs, lim)
	if err != nil {
		return err
	}

	var batches [][]File
	var cur []File
	used := 0
	for _, f := range s.Files {
		cost := fileTokens(f)
		if cost > budget {
			f.Patch = trimPatch(f.Patch, (budget-fileTokens(File{Path: f.Path, Status: f.Status}))*bytesPerToken)
			cost = fileTokens(f)
			p.Unseen = append(p.Unseen, Unseen{Path: f.Path, Reason: "patch trimmed to fit one call"})
		}
		if used+cost > budget && len(cur) > 0 {
			batches = append(batches, cur)
			cur, used = nil, 0
		}
		cur = append(cur, f)
		used += cost
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	if len(batches) > MaxBatches {
		for _, b := range batches[MaxBatches:] {
			for _, f := range b {
				p.Unseen = append(p.Unseen, Unseen{Path: f.Path, Reason: fmt.Sprintf("beyond the %d-batch cap", MaxBatches)})
			}
		}
		batches = batches[:MaxBatches]
	}

	n := len(batches)
	p.Partial = appendOnce(p.Partial, fmt.Sprintf("diff too large for one call; diff-local questions asked over %d patch batches", n))
	for i, b := range batches {
		st := base
		st.Files = b
		st.Note = fmt.Sprintf("This is part %d of %d of the diff. Other files are not shown.", i+1, n)
		if ok, err := fits(st, qs, lim); err != nil || !ok {
			return fmt.Errorf("batch %d of %d does not fit: %w", i+1, n, errors.Join(err, jev.ErrTooLarge))
		}
		p.Calls = append(p.Calls, Call{Context: c, Questions: qs, State: st, Batch: i + 1, Batches: n})
	}
	return nil
}

// planTrimmed keeps every file's stats and trims patches evenly to fit.
func (s *Snapshot) planTrimmed(p *Plan, c rubric.Context, qs []rubric.Question, lim Limits) error {
	st := s.State(c)
	st.Files = nil
	st.Note = "Patches are trimmed to fit; …[trimmed] marks a cut."
	budget, err := fileBudget(st, qs, lim)
	if err != nil {
		return err
	}
	statsCost := 0
	for _, f := range s.Files {
		statsCost += fileTokens(File{Path: f.Path, PreviousPath: f.PreviousPath, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions})
	}
	if statsCost > budget {
		return fmt.Errorf("the file list alone is too large for one call: %w", jev.ErrTooLarge)
	}
	// Each patch also adds its JSON key and a token of rounding.
	perFile := (budget-statsCost-len(s.Files))*bytesPerToken/max(len(s.Files), 1) - len(`,"patch":""`)
	trimmed := make([]File, len(s.Files))
	for i, f := range s.Files {
		f.Patch = trimPatch(f.Patch, perFile)
		trimmed[i] = f
	}
	st.Files = trimmed
	if ok, err := fits(st, qs, lim); err != nil || !ok {
		return fmt.Errorf("trimmed diff does not fit: %w", errors.Join(err, jev.ErrTooLarge))
	}
	p.Partial = appendOnce(p.Partial, "diff too large for one call; PR-level questions saw trimmed patches")
	p.Calls = append(p.Calls, Call{Context: c, Questions: qs, State: st, Batch: 1, Batches: 1})
	return nil
}

const bytesPerToken = 3

// fileBudget is the tokens left for files once the base state and the
// questions are counted against both limits.
func fileBudget(base State, qs []rubric.Question, lim Limits) (int, error) {
	baseTok, err := jev.EstimateTokens(base, nil)
	if err != nil {
		return 0, err
	}
	allTok, err := jev.EstimateTokens(nil, rubric.JevQuestions(qs))
	if err != nil {
		return 0, err
	}
	maxOne := 0
	for _, q := range qs {
		t, err := jev.EstimateTokens(nil, rubric.JevQuestions([]rubric.Question{q}))
		if err != nil {
			return 0, err
		}
		maxOne = max(maxOne, t)
	}
	budget := min(lim.Question-maxOne, lim.Request-allTok) - baseTok
	if budget <= 0 {
		return 0, fmt.Errorf("questions alone exceed the token limit: %w", jev.ErrTooLarge)
	}
	return budget, nil
}

func fits(st State, qs []rubric.Question, lim Limits) (bool, error) {
	all, err := jev.EstimateTokens(st, rubric.JevQuestions(qs))
	if err != nil || all > lim.Request {
		return false, err
	}
	for _, q := range qs {
		one, err := jev.EstimateTokens(st, rubric.JevQuestions([]rubric.Question{q}))
		if err != nil || one > lim.Question {
			return false, err
		}
	}
	return true, nil
}

// fileTokens is a file's share of the state, comma included.
func fileTokens(f File) int {
	b, _ := json.Marshal(f)
	return (len(b) + 1 + bytesPerToken - 1) / bytesPerToken
}

// trimPatch keeps whole lines while the JSON-escaped size stays under
// maxBytes, then appends a marker.
func trimPatch(patch string, maxBytes int) string {
	if escapedLen(patch) <= maxBytes {
		return patch
	}
	budget := maxBytes - escapedLen(trimMarker)
	var b strings.Builder
	used := 0
	for line := range strings.SplitAfterSeq(patch, "\n") {
		n := escapedLen(line)
		if used+n > budget {
			break
		}
		b.WriteString(line)
		used += n
	}
	return strings.TrimSuffix(b.String(), "\n") + trimMarker
}

func escapedLen(s string) int {
	b, _ := json.Marshal(s)
	return len(b) - 2
}
