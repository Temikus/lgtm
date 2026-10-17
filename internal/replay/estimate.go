package replay

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/go-github/v92/github"

	"github.com/Temikus/lgtm/internal/config"
	"github.com/Temikus/lgtm/internal/engine"
	"github.com/Temikus/lgtm/internal/jev"
	"github.com/Temikus/lgtm/internal/prstate"
	"github.com/Temikus/lgtm/internal/rubric"
)

// Estimate is what a replay would send to the model, worked out from the
// diffs without calling it.
type Estimate struct {
	PRs    int // labelled PRs that would be scored
	Calls  int
	Tokens int
	USD    float64 // at fallbackUSDPerMTok
}

// perturbScorings is how often --perturb scores each sampled PR: twice for
// the baseline and once per edit.
var perturbScorings = 2 + len(Edits)

// EstimateCost plans every labelled PR's calls, plus the probe and, if asked,
// the perturbation suite, and prices the input tokens.
func EstimateCost(ctx context.Context, gh *github.Client, data Data, cfg *config.Config, ss []Sample, perturb bool, perturbSample int) (Estimate, error) {
	eo := data.engineOptions(gh, engine.Options{Config: cfg})
	var labelled []Sample
	for _, s := range ss {
		if s.Label != Unlabelled {
			labelled = append(labelled, s)
		}
	}
	var (
		mu  sync.Mutex
		est Estimate
	)
	err := parallel(ctx, len(labelled), historyWorkers, func(i int) error {
		snap, err := fetchSnapshot(ctx, gh, eo, labelled[i].PR)
		if err != nil {
			return nil // replay would record it as not scored
		}
		plan, err := snap.Plan(cfg.Rubric, prstate.DefaultLimits)
		if err != nil {
			return nil
		}
		tokens := 0
		for _, c := range plan.Calls {
			tokens += requestTokens(jev.Request{State: c.State, Questions: rubric.JevQuestions(c.Questions)})
		}
		mu.Lock()
		defer mu.Unlock()
		est.PRs++
		est.Calls += len(plan.Calls)
		est.Tokens += tokens
		return nil
	})
	if err != nil || est.PRs == 0 {
		return est, err
	}
	// The probe scores two PRs; perturbation rescores the sample many times.
	extra := 2
	if perturb {
		extra += perturbScorings * min(perturbSample, est.PRs)
	}
	est.Calls += extra * est.Calls / est.PRs
	est.Tokens += extra * est.Tokens / est.PRs
	est.USD = float64(est.Tokens) * fallbackUSDPerMTok / 1e6
	return est, nil
}

// Text says what the run would cost against the budget.
func (e Estimate) Text(budget float64) string {
	s := fmt.Sprintf("Scoring %d PRs would take about %d model calls and %d input tokens: about $%.2f at $%.2f per million (Jev 1.13 lists $0.042).\n",
		e.PRs, e.Calls, e.Tokens, e.USD, fallbackUSDPerMTok)
	if e.USD > budget {
		s += fmt.Sprintf("That is more than --budget-usd $%.2f, so replay would stop early and fit on the PRs scored before it.\n", budget)
	} else {
		s += fmt.Sprintf("--budget-usd $%.2f stops model calls before spend passes it.\n", budget)
	}
	return s
}
