// Package engine scores one PR: fetch, plan calls, ask, and decide. The
// score, action, and replay commands all go through Score.
package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/go-github/v92/github"

	"github.com/Temikus/lgtm/internal/config"
	"github.com/Temikus/lgtm/internal/jev"
	"github.com/Temikus/lgtm/internal/prstate"
	"github.com/Temikus/lgtm/internal/rubric"
	"github.com/Temikus/lgtm/internal/trust"
	"github.com/Temikus/lgtm/internal/verdict"
)

// Options configure a scoring run.
type Options struct {
	Config  *config.Config
	Decider jev.Decider
	Limits  prstate.Limits // zero means prstate.DefaultLimits
	// Fetch reads the PR's diff. Nil reads it from GitHub.
	Fetch prstate.Fetcher
	// Trusted reports whether a fork PR's author has write access. Nil
	// asks GitHub; replay passes a stored answer.
	Trusted func(ctx context.Context, pr prstate.PR) (bool, error)
}

// QuestionResult is one question's outcome.
type QuestionResult struct {
	rubric.Question
	Value  float64
	Err    error
	Answer *jev.Answer // from the lowest-scoring call when batched
	Calls  int
}

// Result is everything a report needs.
type Result struct {
	PR        prstate.PR
	Mode      config.Mode
	Model     string
	ApproveAt float64
	DenyAt    float64
	VetoAt    float64
	Verdict   verdict.Result
	Questions []QuestionResult // rubric order
	Excluded  []prstate.File
	Unseen    []prstate.Unseen
	Partial   []string
	Untrusted bool
	TrustErr  error
	Served    []string // distinct model versions the replies named
	Usage     jev.Usage
	CostUSD   float64
	Calls     int
}

// Score never returns an error: every failure becomes a needs-human result
// whose Verdict.Failure says why.
func Score(ctx context.Context, gh *github.Client, pr prstate.PR, opts Options) *Result {
	var snap *prstate.Snapshot
	raw, err := fetchRaw(ctx, gh, pr, opts)
	if err == nil {
		snap, err = raw.Snapshot(opts.Config.Exclude)
	}
	if err != nil {
		r := newResult(pr, opts.Config)
		if snap != nil {
			r.Excluded, r.Unseen, r.Partial = snap.Excluded, snap.Unseen, snap.Partial
		}
		r.Verdict = verdict.Decide(opts.Config.Policy(), verdict.Input{Failure: err})
		return r
	}
	return ScoreSnapshot(ctx, gh, snap, opts)
}

// ScoreSnapshot scores a fetched snapshot without changing it, so replay can
// score edited copies of one PR.
func ScoreSnapshot(ctx context.Context, gh *github.Client, snap *prstate.Snapshot, opts Options) *Result {
	cfg := opts.Config
	r := newResult(snap.PR, cfg)
	r.Excluded, r.Unseen, r.Partial = snap.Excluded, slices.Clone(snap.Unseen), slices.Clone(snap.Partial)
	in := verdict.Input{}
	defer func() { r.Verdict = verdict.Decide(cfg.Policy(), in) }()

	r.Untrusted, r.TrustErr = untrusted(ctx, gh, snap.PR, opts)
	in.Untrusted = r.Untrusted

	lim := opts.Limits
	if lim == (prstate.Limits{}) {
		lim = prstate.DefaultLimits
	}
	plan, err := snap.Plan(cfg.Rubric, lim)
	if err != nil {
		in.Failure = err
		return r
	}
	r.Unseen = append(r.Unseen, plan.Unseen...)
	r.Partial = append(r.Partial, plan.Partial...)
	in.Partial = len(r.Partial) > 0

	answers, err := r.ask(ctx, opts.Decider, plan.Calls)
	r.collect(answers)
	if err != nil {
		in.Failure = err
		return r
	}
	if snap := cfg.Snapshot(); snap != "" {
		in.ModelChanged = slices.ContainsFunc(r.Served, func(m string) bool { return m != snap })
	}
	in.Values = r.Values()
	in.Consistency = func() (bool, error) { return r.consistent(ctx, opts.Decider, plan.Calls, answers) }
	return r
}

func newResult(pr prstate.PR, cfg *config.Config) *Result {
	approve, deny := cfg.Thresholds()
	r := &Result{PR: pr, Mode: cfg.Mode, Model: cfg.Model, ApproveAt: approve, DenyAt: deny, VetoAt: cfg.VetoAt}
	for _, q := range cfg.Rubric {
		r.Questions = append(r.Questions, QuestionResult{Question: q, Err: errors.New("not asked")})
	}
	return r
}

// Values are the normalised answers in the form verdict.Decide takes.
func (r *Result) Values() []verdict.Value {
	out := make([]verdict.Value, 0, len(r.Questions))
	for _, q := range r.Questions {
		out = append(out, verdict.Value{ID: q.ID, Role: q.Role, Value: q.Value, Weight: q.Weight, CapAt: q.CapAt, Err: q.Err})
	}
	return out
}

func fetchRaw(ctx context.Context, gh *github.Client, pr prstate.PR, opts Options) (*prstate.Raw, error) {
	if opts.Fetch != nil {
		return opts.Fetch(ctx, pr)
	}
	return prstate.FetchRaw(ctx, gh, pr)
}

// untrusted applies the fork cap: a fork PR is untrusted unless trust_forks
// is set or its author has write access. A failed lookup is untrusted.
func untrusted(ctx context.Context, gh *github.Client, pr prstate.PR, opts Options) (bool, error) {
	if !pr.Fork || opts.Config.TrustForks {
		return false, nil
	}
	if opts.Trusted != nil {
		ok, err := opts.Trusted(ctx, pr)
		return !ok, err
	}
	ok, err := trust.AuthorTrusted(ctx, gh, pr.Owner, pr.Repo, pr.Author)
	return !ok, err
}

// callAnswers are one call's answers, aligned with plan.Calls.
type callAnswers map[string]jev.Answer

func (r *Result) ask(ctx context.Context, d jev.Decider, calls []prstate.Call) ([]callAnswers, error) {
	out := make([]callAnswers, len(calls))
	for i, c := range calls {
		resp, err := d.Decide(ctx, jev.Request{Model: r.Model, State: c.State, Questions: rubric.JevQuestions(c.Questions)})
		r.Calls++
		if err != nil {
			return out, fmt.Errorf("decision call %d of %d (%s context): %w", i+1, len(calls), c.Context, err)
		}
		r.served(resp.Model)
		r.Usage.InputTokens += resp.Usage.InputTokens
		r.Usage.OutputTokens += resp.Usage.OutputTokens
		r.CostUSD += resp.CostUSD
		out[i] = resp.Answers
	}
	return out, nil
}

func (r *Result) served(model string) {
	if !slices.Contains(r.Served, model) {
		r.Served = append(r.Served, model)
	}
}

// collect normalises answers and takes the min across a question's calls.
func (r *Result) collect(answers []callAnswers) {
	for i := range r.Questions {
		q := &r.Questions[i]
		for _, ca := range answers {
			a, ok := ca[q.ID]
			if !ok {
				continue
			}
			v, err := q.Normalise(a)
			if err != nil {
				q.Err = err
				break
			}
			if q.Calls == 0 || v < q.Value {
				q.Value, q.Answer, q.Err = v, &a, nil
			}
			q.Calls++
		}
	}
}

// consistent re-asks every gating or cap choice question with its options
// reversed, over the same states, and reports whether every choice held.
func (r *Result) consistent(ctx context.Context, d jev.Decider, calls []prstate.Call, answers []callAnswers) (bool, error) {
	for i, c := range calls {
		var reversed []rubric.Question
		for _, q := range c.Questions {
			if q.Type == jev.Choice && q.Role != rubric.Info {
				reversed = append(reversed, q.Reversed())
			}
		}
		if len(reversed) == 0 {
			continue
		}
		resp, err := d.Decide(ctx, jev.Request{Model: r.Model, State: c.State, Questions: rubric.JevQuestions(reversed)})
		r.Calls++
		if err != nil {
			return false, err
		}
		r.served(resp.Model)
		r.CostUSD += resp.CostUSD
		for _, q := range reversed {
			if resp.Answers[q.ID].Choice != answers[i][q.ID].Choice {
				return false, nil
			}
		}
	}
	return true, nil
}
