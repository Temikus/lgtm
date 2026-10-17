// Package ghout publishes results to GitHub: the LGTM check run, the sticky
// comment, and the needs-human label.
package ghout

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/Temikus/lgtm/internal/config"
	"github.com/Temikus/lgtm/internal/engine"
	"github.com/Temikus/lgtm/internal/prstate"
	"github.com/Temikus/lgtm/internal/report"
	"github.com/Temikus/lgtm/internal/verdict"
)

const (
	// CheckName is the check run's name; required-check settings match on it.
	CheckName = engine.OwnCheck
	// NeedsHumanLabel is applied in enforce mode when the verdict escalates.
	NeedsHumanLabel = "lgtm: needs-human"
	// ActionsBot posts comments made with the workflow's GITHUB_TOKEN.
	ActionsBot = "github-actions[bot]"
	// actionsApp is the app slug that owns check runs made with GITHUB_TOKEN.
	actionsApp = "github-actions"
	// GitHub rejects check-run summaries over 65535 characters.
	maxSummary = 65000
)

// Publisher writes to one PR.
type Publisher struct {
	GH *github.Client
	PR prstate.PR
	// CommentAuthor is whose sticky comment to update: ActionsBot in the
	// Action, the token's user for `lgtm score --post`.
	CommentAuthor string
}

// Publish posts the check run, sticky comment, and label for r.
func (p Publisher) Publish(ctx context.Context, r *engine.Result) error {
	if err := p.Check(ctx, r); err != nil {
		return err
	}
	if err := p.Comment(ctx, report.Comment(r)); err != nil {
		return err
	}
	if r.Mode == config.Enforce {
		return p.Label(ctx, r.Verdict.Verdict == verdict.NeedsHuman)
	}
	return nil
}

// Check creates a completed LGTM check run on the scored SHA.
func (p Publisher) Check(ctx context.Context, r *engine.Result) error {
	_, _, err := p.GH.Checks.CreateCheckRun(ctx, p.PR.Owner, p.PR.Repo, github.CreateCheckRunOptions{
		Name:        CheckName,
		HeadSHA:     p.PR.HeadSHA,
		ExternalID:  new(EncodeState(State{Mode: r.Mode, Verdict: r.Verdict.Verdict})),
		Status:      new("completed"),
		Conclusion:  new(report.Conclusion(r)),
		CompletedAt: &github.Timestamp{Time: time.Now()},
		Output: &github.CheckRunOutput{
			Title:   new(report.Title(r)),
			Summary: new(capSummary(report.Summary(r))),
		},
	})
	if err != nil {
		return fmt.Errorf("creating check run: %w", err)
	}
	return nil
}

// Comment updates the sticky comment in place, or creates it. Only a comment
// with the marker *and* the expected author counts: anyone can post the marker.
func (p Publisher) Comment(ctx context.Context, body string) error {
	for c, err := range p.GH.Issues.ListCommentsIter(ctx, p.PR.Owner, p.PR.Repo, p.PR.Number, &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}) {
		if err != nil {
			return fmt.Errorf("listing comments: %w", err)
		}
		if c.GetUser().GetLogin() != p.CommentAuthor || !strings.HasPrefix(c.GetBody(), report.Marker) {
			continue
		}
		if _, _, err := p.GH.Issues.UpdateComment(ctx, p.PR.Owner, p.PR.Repo, c.GetID(), github.IssueCommentRequest{Body: body}); err != nil {
			return fmt.Errorf("updating comment: %w", err)
		}
		return nil
	}
	if _, _, err := p.GH.Issues.CreateComment(ctx, p.PR.Owner, p.PR.Repo, p.PR.Number, github.IssueCommentRequest{Body: body}); err != nil {
		return fmt.Errorf("creating comment: %w", err)
	}
	return nil
}

// Label adds or removes the needs-human label.
func (p Publisher) Label(ctx context.Context, needsHuman bool) error {
	if needsHuman {
		_, _, err := p.GH.Issues.AddLabelsToIssue(ctx, p.PR.Owner, p.PR.Repo, p.PR.Number, []string{NeedsHumanLabel})
		if err != nil {
			return fmt.Errorf("adding label: %w", err)
		}
		return nil
	}
	resp, err := p.GH.Issues.RemoveLabelForIssue(ctx, p.PR.Owner, p.PR.Repo, p.PR.Number, NeedsHumanLabel)
	if err != nil && (resp == nil || resp.StatusCode != http.StatusNotFound) {
		return fmt.Errorf("removing label: %w", err)
	}
	return nil
}

// State is what a check run records for the human-approval flip. It lives in
// the check run's external_id, which PR authors can't edit.
type State struct {
	Mode    config.Mode
	Verdict verdict.Verdict
}

const statePrefix = "lgtm/v1"

// EncodeState formats a State for external_id.
func EncodeState(s State) string {
	return fmt.Sprintf("%s mode=%s verdict=%s", statePrefix, s.Mode, s.Verdict)
}

// DecodeState parses external_id; ok is false for anything else.
func DecodeState(s string) (State, bool) {
	var st State
	var mode, v string
	if _, err := fmt.Sscanf(s, statePrefix+" mode=%s verdict=%s", &mode, &v); err != nil {
		return st, false
	}
	st.Mode, st.Verdict = config.Mode(mode), verdict.Verdict(v)
	switch st.Verdict {
	case verdict.Approve, verdict.Deny, verdict.NeedsHuman:
	default:
		return st, false
	}
	return st, st.Mode == config.Shadow || st.Mode == config.Enforce
}

// FindCheck returns the latest LGTM check run on sha made by GitHub Actions,
// or nil if there is none.
func FindCheck(ctx context.Context, gh *github.Client, owner, repo, sha string) (*github.CheckRun, error) {
	var latest *github.CheckRun
	opts := &github.ListCheckRunsOptions{CheckName: new(CheckName), Filter: new("all"), ListOptions: github.ListOptions{PerPage: 100}}
	for run, err := range gh.Checks.ListCheckRunsForRefIter(ctx, owner, repo, sha, opts) {
		if err != nil {
			return nil, fmt.Errorf("listing check runs: %w", err)
		}
		if run.GetApp().GetSlug() != actionsApp || run.GetHeadSHA() != sha {
			continue
		}
		if _, ok := DecodeState(run.GetExternalID()); !ok {
			continue
		}
		if latest == nil || run.GetID() > latest.GetID() {
			latest = run
		}
	}
	return latest, nil
}

// MarkApproved flips a needs-human check run to success after a qualifying
// human approval. No model call is involved.
func MarkApproved(ctx context.Context, gh *github.Client, owner, repo string, run *github.CheckRun, by string) error {
	st, _ := DecodeState(run.GetExternalID())
	st.Verdict = verdict.Approve
	title := fmt.Sprintf("approved by %s · LGTM had asked for human review", by)
	summary := fmt.Sprintf("A human with write access (%s) approved this commit, so the check passes.\n\n%s",
		report.Escape(by), run.GetOutput().GetSummary())
	_, _, err := gh.Checks.UpdateCheckRun(ctx, owner, repo, run.GetID(), github.UpdateCheckRunOptions{
		Name:        CheckName,
		ExternalID:  new(EncodeState(st) + " human=1"),
		Status:      new("completed"),
		Conclusion:  new("success"),
		CompletedAt: &github.Timestamp{Time: time.Now()},
		Output:      &github.CheckRunOutput{Title: new(title), Summary: new(capSummary(summary))},
	})
	if err != nil {
		return fmt.Errorf("updating check run: %w", err)
	}
	return nil
}

func capSummary(s string) string {
	if len(s) <= maxSummary {
		return s
	}
	return s[:maxSummary] + "\n\n…(truncated)"
}
