// Package action is the GitHub Action entrypoint: it routes a workflow event
// to scoring or to the human-approval flip.
package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/go-github/v92/github"

	"github.com/Temikus/lgtm/internal/config"
	"github.com/Temikus/lgtm/internal/engine"
	"github.com/Temikus/lgtm/internal/ghout"
	"github.com/Temikus/lgtm/internal/jev"
	"github.com/Temikus/lgtm/internal/prstate"
	"github.com/Temikus/lgtm/internal/report"
	"github.com/Temikus/lgtm/internal/trust"
	"github.com/Temikus/lgtm/internal/verdict"
)

// Env is everything Run needs from the runner.
type Env struct {
	EventName string
	Event     []byte
	GH        *github.Client
	// NewDecider builds the model client once config is known. An error
	// becomes a needs-human result, not a crash.
	NewDecider    func(*config.Config) (jev.Decider, error)
	CommentAuthor string
	Log           io.Writer
	StepSummary   io.Writer // may be nil
	SetOutput     func(name, value string)
}

type payload struct {
	Action      string              `json:"action"`
	PullRequest *github.PullRequest `json:"pull_request"`
	Repository  *github.Repository  `json:"repository"`
	WorkflowRun *struct {
		Event   string `json:"event"`
		HeadSHA string `json:"head_sha"`
	} `json:"workflow_run"`
}

var scoreActions = map[string]bool{"opened": true, "synchronize": true, "reopened": true, "edited": true, "ready_for_review": true}

// Run handles one event and returns the job's exit code. In enforce mode it
// is non-zero for anything but approve, so a required job gates like the
// check run does.
func Run(ctx context.Context, env Env) (int, error) {
	var p payload
	if err := json.Unmarshal(env.Event, &p); err != nil {
		return 1, fmt.Errorf("parsing event payload: %w", err)
	}
	owner, repo := p.Repository.GetOwner().GetLogin(), p.Repository.GetName()

	switch env.EventName {
	case "pull_request_target", "pull_request":
		if !scoreActions[p.Action] || p.PullRequest == nil {
			return env.skip("action %q", p.Action)
		}
		return env.score(ctx, prstate.FromPullRequest(owner, repo, p.PullRequest))
	case "pull_request_review":
		if p.Action != "submitted" || p.PullRequest == nil {
			return env.skip("review action %q", p.Action)
		}
		return env.review(ctx, prstate.FromPullRequest(owner, repo, p.PullRequest), true)
	case "workflow_run":
		if p.WorkflowRun == nil || p.WorkflowRun.Event != "pull_request_review" {
			return env.skip("workflow_run not from a review")
		}
		pr, err := findOpenPR(ctx, env.GH, owner, repo, p.WorkflowRun.HeadSHA)
		if err != nil || pr == nil {
			return 0, err
		}
		return env.review(ctx, *pr, false)
	}
	return env.skip("event %q", env.EventName)
}

func (env Env) skip(format string, args ...any) (int, error) {
	_, _ = fmt.Fprintf(env.Log, "lgtm: nothing to do for "+format+"\n", args...)
	return 0, nil
}

func (env Env) score(ctx context.Context, pr prstate.PR) (int, error) {
	cfg, err := config.FetchAtSHA(ctx, env.GH, pr.Owner, pr.Repo, pr.BaseSHA)
	if err != nil {
		return 1, env.configFailure(ctx, pr, err)
	}
	d, err := env.NewDecider(cfg)
	if err != nil {
		d = failing{err}
	}
	r := engine.Score(ctx, env.GH, pr, engine.Options{Config: cfg, Decider: d})
	if err := env.floor(ctx, cfg, r); err != nil {
		// Publishing nothing leaves a required LGTM check missing, not passed.
		return 1, err
	}
	_, _ = fmt.Fprint(env.Log, report.Text(r))

	pub := ghout.Publisher{GH: env.GH, PR: pr, CommentAuthor: env.CommentAuthor}
	// A re-score of the same commit, such as after an edit or a reopen, keeps
	// an approval it already has. If the check fails, the PR stays held.
	if cfg.Mode == config.Enforce && r.Verdict.Verdict == verdict.NeedsHuman {
		ok, by, err := trust.HumanApproval(ctx, env.GH, pr)
		if err != nil {
			_, _ = fmt.Fprintln(env.Log, "lgtm: can't read approvals, so the PR stays held:", err)
		} else if ok {
			pub.ApprovedBy = by
		}
	}
	if err := pub.Publish(ctx, r); err != nil {
		return 1, err
	}
	if env.StepSummary != nil {
		_, _ = io.WriteString(env.StepSummary, report.Summary(r))
	}
	if env.SetOutput != nil && !r.Blind {
		env.SetOutput("verdict", string(r.Verdict.Verdict))
		if cfg.ShowScores {
			env.SetOutput("score", fmt.Sprintf("%.4f", r.Verdict.Aggregate))
		}
	}
	if pub.ApprovedBy != "" {
		_, _ = fmt.Fprintf(env.Log, "lgtm: %s already approved %s\n", pub.ApprovedBy, pr.HeadSHA)
		return 0, nil
	}
	return exitCode(cfg.Mode, r.Verdict.Verdict), nil
}

// floor applies cfg.Rescore against LGTM's earlier check run on the same
// commit, if any.
func (env Env) floor(ctx context.Context, cfg *config.Config, r *engine.Result) error {
	if cfg.Rescore == verdict.RescoreLatest {
		return nil
	}
	prev, err := ghout.FindCheck(ctx, env.GH, r.PR.Owner, r.PR.Repo, r.PR.HeadSHA)
	if err != nil || prev == nil {
		return err
	}
	st, _ := ghout.DecodeState(prev.GetExternalID())
	earlier := st.Verdict
	if st.Human {
		earlier = verdict.NeedsHuman // LGTM itself didn't approve
	}
	r.Verdict = verdict.Floor(cfg.Rescore, earlier, r.Verdict)
	return nil
}

// configFailure reports a broken base-branch config on the check run. The
// mode is unknown, so the check fails closed.
func (env Env) configFailure(ctx context.Context, pr prstate.PR, cause error) error {
	_, _, err := env.GH.Checks.CreateCheckRun(ctx, pr.Owner, pr.Repo, github.CreateCheckRunOptions{
		Name: ghout.CheckName, HeadSHA: pr.HeadSHA,
		Status: new("completed"), Conclusion: new("failure"),
		Output: &github.CheckRunOutput{
			Title:   new("LGTM config is invalid"),
			Summary: new("Fix " + config.Path + " on the base branch: " + report.Escape(cause.Error())),
		},
	})
	return errors.Join(cause, err)
}

// review flips a needs-human check to success when a qualifying human has
// approved the scored SHA. It never calls the model. With no LGTM check run
// on the SHA yet, a review event scores normally; workflow_run (no model
// key) does nothing.
func (env Env) review(ctx context.Context, pr prstate.PR, scoreIfMissing bool) (int, error) {
	run, err := ghout.FindCheck(ctx, env.GH, pr.Owner, pr.Repo, pr.HeadSHA)
	if err != nil {
		return 1, err
	}
	if run == nil {
		if scoreIfMissing {
			return env.score(ctx, pr)
		}
		return env.skip("no LGTM check on %s", pr.HeadSHA)
	}
	st, _ := ghout.DecodeState(run.GetExternalID())
	if st.Mode == config.Shadow {
		return env.skip("shadow mode review")
	}
	if st.Verdict != verdict.NeedsHuman || run.GetConclusion() == "success" {
		return exitCode(st.Mode, st.Verdict), nil
	}
	ok, by, err := trust.HumanApproval(ctx, env.GH, pr)
	if err != nil {
		return 1, err
	}
	if !ok {
		_, _ = fmt.Fprintln(env.Log, "lgtm: still needs a human approval on", pr.HeadSHA)
		return 1, nil
	}
	_, _ = fmt.Fprintf(env.Log, "lgtm: %s approved %s\n", by, pr.HeadSHA)
	if err := ghout.MarkApproved(ctx, env.GH, pr.Owner, pr.Repo, run, by); err != nil {
		if pr.Fork && forbidden(err) {
			// A fork's review event gets a read-only token. The approval
			// qualifies; the lgtm-review workflow_run job flips the check.
			_, _ = fmt.Fprintln(env.Log, "lgtm: read-only token on a fork review; the workflow_run job will update the check")
			return 0, nil
		}
		return 1, err
	}
	pub := ghout.Publisher{GH: env.GH, PR: pr}
	return 0, pub.Label(ctx, false)
}

// findOpenPR maps a workflow_run head SHA to its PR. workflow_run payloads
// leave pull_requests empty for forks, so search open PRs instead.
func findOpenPR(ctx context.Context, gh *github.Client, owner, repo, sha string) (*prstate.PR, error) {
	opts := &github.PullRequestListOptions{State: "open", ListOptions: github.ListOptions{PerPage: 100}}
	for p, err := range gh.PullRequests.ListIter(ctx, owner, repo, opts) {
		if err != nil {
			return nil, fmt.Errorf("listing open PRs: %w", err)
		}
		if p.GetHead().GetSHA() == sha {
			pr := prstate.FromPullRequest(owner, repo, p)
			return &pr, nil
		}
	}
	return nil, nil
}

func exitCode(mode config.Mode, v verdict.Verdict) int {
	if mode == config.Enforce && v != verdict.Approve {
		return 1
	}
	return 0
}

func forbidden(err error) bool {
	var ge *github.ErrorResponse
	return errors.As(err, &ge) && ge.Response != nil &&
		(ge.Response.StatusCode == http.StatusForbidden || strings.Contains(ge.Message, "Resource not accessible"))
}

type failing struct{ err error }

func (f failing) Decide(context.Context, jev.Request) (*jev.Response, error) { return nil, f.err }
