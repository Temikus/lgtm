package replay

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/pelletier/go-toml/v2"

	"github.com/Temikus/lgtm/internal/config"
	"github.com/Temikus/lgtm/internal/engine"
	"github.com/Temikus/lgtm/internal/ghout"
	"github.com/Temikus/lgtm/internal/jev"
	"github.com/Temikus/lgtm/internal/prstate"
	"github.com/Temikus/lgtm/internal/verdict"
)

// Options configure a replay.
type Options struct {
	Owner, Repo     string
	Config          *config.Config
	Decider         jev.Decider
	BudgetUSD       float64
	History         HistoryOptions
	TargetPrecision float64
	Perturb         bool
	PerturbSample   int // PRs to perturb; 0 means 10
	Concurrency     int // 0 means 4
	Progress        io.Writer
	Now             func() time.Time
}

// Report is everything replay found.
type Report struct {
	Repo        string
	Labels      LabelMode
	Model       string
	Served      []string
	Samples     []Sample
	Points      []Point
	Failed      []Failure
	BudgetHit   bool
	Fit         Fit
	Current     Metrics // at the thresholds in config
	Fitted      *Metrics
	CurrentPol  verdict.Policy
	Reliability []QuestionReliability
	Probe       []Baseline
	Baseline    *FlipRate
	Edits       []FlipRate
	Shadow      *Shadow
	CostUSD     float64
	// Calibration is what --write stores, or nil with WhyNot saying why not.
	Calibration *config.Calibration
	WhyNot      string
}

// Failure is a PR that couldn't be scored.
type Failure struct {
	Number int
	Err    error
}

// Run labels history, scores each labelled PR, and fits thresholds.
func Run(ctx context.Context, gh *github.Client, o Options) (*Report, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	if o.PerturbSample <= 0 {
		o.PerturbSample = 10
	}
	budget := &Budget{Decider: o.Decider, LimitUSD: o.BudgetUSD}
	cfg := replayConfig(o.Config)
	eo := engine.Options{Config: cfg, Decider: budget}

	rep := &Report{Repo: o.Owner + "/" + o.Repo, Labels: o.History.Mode, Model: cfg.Model, CurrentPol: o.Config.Policy()}
	o.progress("budget $%.2f: model calls stop before spend passes it", o.BudgetUSD)
	o.progress("reading closed PRs since %s", o.History.Since.Format(time.DateOnly))
	samples, err := History(ctx, gh, o.Owner, o.Repo, o.History)
	if err != nil {
		return nil, err
	}
	rep.Samples = samples

	var labelled []Sample
	for _, s := range samples {
		if s.Label != Unlabelled {
			labelled = append(labelled, s)
		}
	}
	o.progress("%d PRs, %d labelled; scoring the labelled ones", len(samples), len(labelled))
	rep.collect(labelled, o.scoreAll(ctx, gh, eo, labelled))

	rep.Fit = FitThresholds(rep.Points, o.TargetPrecision)
	rep.Current = Measure(rep.Points, rep.CurrentPol)
	if rep.Fit.OK {
		pol := rep.CurrentPol
		pol.ApproveAt, pol.DenyAt = rep.Fit.ApproveAt, rep.Fit.DenyAt
		m := Measure(rep.Points, pol)
		rep.Fitted = &m
	}
	rep.Reliability = Reliability(rep.Points, cfg.Rubric)
	if err := o.extras(ctx, gh, eo, rep, labelled); err != nil {
		return nil, err
	}
	rep.CostUSD = budget.Spent()
	rep.Calibration, rep.WhyNot = calibration(rep, o.Now())
	return rep, nil
}

func (rep *Report) collect(labelled []Sample, results []*engine.Result) {
	for i, r := range results {
		s := labelled[i]
		switch {
		case r == nil || errors.Is(r.Verdict.Failure, ErrBudget):
			rep.BudgetHit = true
		case r.Verdict.Failure != nil:
			rep.Failed = append(rep.Failed, Failure{Number: s.PR.Number, Err: r.Verdict.Failure})
		default:
			for _, m := range r.Served {
				if !slices.Contains(rep.Served, m) {
					rep.Served = append(rep.Served, m)
				}
			}
			p := point(r)
			p.PR, p.Number, p.Title, p.Label, p.Low, p.Reason = s.PR, s.PR.Number, s.PR.Title, s.Label, s.Low, s.Reason
			rep.Points = append(rep.Points, p)
		}
	}
}

// extras runs the probe, the perturbation suite, and the shadow-mode read.
func (o Options) extras(ctx context.Context, gh *github.Client, eo engine.Options, rep *Report, labelled []Sample) error {
	cfg := eo.Config
	if sample := o.probeSample(ctx, gh, cfg, labelled); sample != nil && !rep.BudgetHit {
		o.progress("probing each question with content-free PRs")
		rep.Probe = Probe(ctx, gh, eo, sample, rep.CurrentPol.ApproveAt)
	}
	if o.Perturb && !rep.BudgetHit {
		snaps := o.perturbSample(ctx, gh, cfg, rep.Points)
		o.progress("perturbing %d PRs", len(snaps))
		base, edits := Perturb(ctx, gh, eo, snaps, rep.CurrentPol)
		rep.Baseline, rep.Edits = &base, edits
	}
	o.progress("reading earlier LGTM check runs")
	var err error
	rep.Shadow, err = ShadowMetrics(ctx, gh, o.Owner, o.Repo, rep.Samples)
	return err
}

// replayConfig turns off the borderline re-ask: replay judges thresholds
// on the aggregate, so the extra calls would buy nothing.
func replayConfig(c *config.Config) *config.Config {
	cc := *c
	cc.BorderlineMargin = 0
	cc.Calibration = nil
	return &cc
}

func (o Options) scoreAll(ctx context.Context, gh *github.Client, eo engine.Options, ss []Sample) []*engine.Result {
	out := make([]*engine.Result, len(ss))
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		stopped bool
		done    int
	)
	sem := make(chan struct{}, o.Concurrency)
	for i, s := range ss {
		mu.Lock()
		stop := stopped
		mu.Unlock()
		if stop || ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			r := engine.Score(ctx, gh, s.PR, eo)
			mu.Lock()
			defer mu.Unlock()
			out[i] = r
			done++
			if errors.Is(r.Verdict.Failure, ErrBudget) {
				stopped = true
			}
			if done%10 == 0 {
				o.progress("scored %d of %d", done, len(ss))
			}
		})
	}
	wg.Wait()
	return out
}

func (o Options) probeSample(ctx context.Context, gh *github.Client, cfg *config.Config, ss []Sample) *prstate.Snapshot {
	for _, s := range ss {
		if snap, err := prstate.Fetch(ctx, gh, s.PR, cfg.Exclude); err == nil {
			return snap
		}
	}
	return nil
}

// perturbSample takes the labelled PRs nearest the approve threshold, where
// a flip would change the verdict.
func (o Options) perturbSample(ctx context.Context, gh *github.Client, cfg *config.Config, ps []Point) []*prstate.Snapshot {
	ps = slices.Clone(ps)
	at := o.Config.Policy().ApproveAt
	slices.SortStableFunc(ps, func(a, b Point) int { return cmp.Compare(math.Abs(a.Agg-at), math.Abs(b.Agg-at)) })
	var out []*prstate.Snapshot
	for _, p := range ps {
		if len(out) == o.PerturbSample {
			break
		}
		if snap, err := prstate.Fetch(ctx, gh, p.PR, cfg.Exclude); err == nil {
			out = append(out, snap)
		}
	}
	return out
}

func (o Options) progress(format string, args ...any) {
	if o.Progress != nil {
		_, _ = fmt.Fprintf(o.Progress, format+"\n", args...)
	}
}

// calibration is what --write may store: a successful fit from replies that
// all named one model version.
func calibration(rep *Report, now time.Time) (*config.Calibration, string) {
	switch {
	case !rep.Fit.OK:
		return nil, rep.Fit.Why
	case len(rep.Served) != 1:
		return nil, fmt.Sprintf("replies named %d model versions (%v); thresholds fitted across versions don't hold for any one of them", len(rep.Served), rep.Served)
	}
	y, m, d := now.Date()
	return &config.Calibration{
		Model:     rep.Model,
		Snapshot:  rep.Served[0],
		Date:      toml.LocalDate{Year: y, Month: int(m), Day: d},
		NLabelled: rep.Fit.NLabelled,
		// Round towards needs-human, so rounding never widens approve or deny.
		ApproveAt:        math.Ceil(rep.Fit.ApproveAt*1e4) / 1e4,
		DenyAt:           math.Floor(rep.Fit.DenyAt*1e4) / 1e4,
		ApprovePrecision: math.Floor(rep.Fit.ApprovePrecision*1e4) / 1e4,
	}, ""
}

// Shadow compares earlier shadow-mode LGTM check runs with what humans did.
type Shadow struct {
	N         int // labelled PRs with a shadow check run
	Agreed    int // approve on positive or deny on negative
	Decisive  int // labelled PRs where LGTM said approve or deny
	ByVerdict map[verdict.Verdict]ShadowGroup
}

// ShadowGroup describes PRs that got one shadow verdict.
type ShadowGroup struct {
	N                 int
	MedianTimeToMerge time.Duration
	MeanComments      float64
}

// ShadowMetrics reads the LGTM check run on each sample's head. It returns
// nil when no sample has a shadow-mode run.
func ShadowMetrics(ctx context.Context, gh *github.Client, owner, repo string, ss []Sample) (*Shadow, error) {
	type row struct {
		s Sample
		v verdict.Verdict
	}
	var rows []row
	for _, s := range ss {
		run, err := ghout.FindCheck(ctx, gh, owner, repo, s.PR.HeadSHA)
		if err != nil {
			return nil, err
		}
		if run == nil {
			continue
		}
		if st, ok := ghout.DecodeState(run.GetExternalID()); ok && st.Mode == config.Shadow {
			rows = append(rows, row{s, st.Verdict})
		}
	}
	if len(rows) == 0 {
		return nil, nil
	}
	sh := &Shadow{ByVerdict: map[verdict.Verdict]ShadowGroup{}}
	merge := map[verdict.Verdict][]time.Duration{}
	comments := map[verdict.Verdict]int{}
	for _, r := range rows {
		g := sh.ByVerdict[r.v]
		g.N++
		sh.ByVerdict[r.v] = g
		comments[r.v] += r.s.Comments
		if !r.s.Merged.IsZero() {
			merge[r.v] = append(merge[r.v], r.s.Merged.Sub(r.s.Created))
		}
		if r.s.Label == Unlabelled || r.s.Low {
			continue
		}
		sh.N++
		if r.v == verdict.NeedsHuman {
			continue
		}
		sh.Decisive++
		if (r.v == verdict.Approve) == (r.s.Label == Positive) {
			sh.Agreed++
		}
	}
	for v, g := range sh.ByVerdict {
		g.MeanComments = float64(comments[v]) / float64(g.N)
		if ds := merge[v]; len(ds) > 0 {
			slices.Sort(ds)
			g.MedianTimeToMerge = ds[len(ds)/2]
		}
		sh.ByVerdict[v] = g
	}
	return sh, nil
}
