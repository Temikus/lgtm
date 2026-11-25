// Package ghout publishes results to GitHub: the LGTM check run, the sticky
// comment, and the needs-human label.
package ghout

import (
	"context"
	"fmt"
	"net/http"
	"slices"
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
	// ApprovedBy is a qualifying human who already approved the scored SHA.
	// It passes a needs-human verdict in enforce mode, never a deny.
	ApprovedBy string
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
		return p.Label(ctx, r.Verdict.Verdict == verdict.NeedsHuman && !p.approved(r))
	}
	return nil
}

func (p Publisher) approved(r *engine.Result) bool {
	return p.ApprovedBy != "" && r.Mode == config.Enforce && r.Verdict.Verdict == verdict.NeedsHuman
}

// Check creates a completed LGTM check run on the scored SHA.
func (p Publisher) Check(ctx context.Context, r *engine.Result) error {
	st := State{Mode: r.Mode, Verdict: r.Verdict.Verdict, Blind: r.Blind}
	ext, conclusion, title, summary := EncodeState(st), report.Conclusion(r), report.Title(r), report.Summary(r)
	if p.approved(r) {
		ext, title, summary = approvedOutput(st, p.ApprovedBy, summary)
		conclusion = "success"
	}
	_, _, err := p.GH.Checks.CreateCheckRun(ctx, p.PR.Owner, p.PR.Repo, github.CreateCheckRunOptions{
		Name:        CheckName,
		HeadSHA:     p.PR.HeadSHA,
		ExternalID:  new(ext),
		Status:      new("completed"),
		Conclusion:  new(conclusion),
		CompletedAt: &github.Timestamp{Time: time.Now()},
		Output: &github.CheckRunOutput{
			Title:   new(title),
			Summary: new(capSummary(summary)),
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
	// Human means a human approval turned LGTM's needs-human into approve.
	Human bool
	// Blind means the verdict was recorded but not shown.
	Blind bool
	// CIHeld means LGTM approved, then held the PR only because a required
	// check failed afterwards. A passing re-run undoes it.
	CIHeld bool
}

const (
	statePrefix = "lgtm/v1"
	humanSuffix = " human=1"
	blindSuffix = " blind=1" // shadow only, so never with humanSuffix
	ciSuffix    = " ci=1"
)

// EncodeState formats a State for external_id.
func EncodeState(s State) string {
	out := fmt.Sprintf("%s mode=%s verdict=%s", statePrefix, s.Mode, s.Verdict)
	if s.CIHeld {
		out += ciSuffix
	}
	if s.Blind {
		out += blindSuffix
	}
	return out
}

// DecodeState parses external_id; ok is false for anything else.
func DecodeState(s string) (State, bool) {
	var st State
	var mode, v string
	if _, err := fmt.Sscanf(s, statePrefix+" mode=%s verdict=%s", &mode, &v); err != nil {
		return st, false
	}
	st.Mode, st.Verdict = config.Mode(mode), verdict.Verdict(v)
	flags := strings.Fields(s)
	has := func(suffix string) bool { return slices.Contains(flags, strings.TrimSpace(suffix)) }
	st.Human, st.Blind, st.CIHeld = has(humanSuffix), has(blindSuffix), has(ciSuffix)
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
	ext, title, summary := approvedOutput(st, by, run.GetOutput().GetSummary())
	_, _, err := gh.Checks.UpdateCheckRun(ctx, owner, repo, run.GetID(), github.UpdateCheckRunOptions{
		Name:        CheckName,
		ExternalID:  new(ext),
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

// MarkCIFailed turns an approve into needs-human when a required check
// failed after LGTM scored the commit. No model call is involved. A blind
// check keeps its title and summary, which show no verdict.
func MarkCIFailed(ctx context.Context, gh *github.Client, owner, repo string, run *github.CheckRun, failed []string) error {
	st, _ := DecodeState(run.GetExternalID())
	st.Verdict, st.CIHeld = verdict.NeedsHuman, true
	// GitHub counts neutral as passing, so enforce mode fails the check.
	conclusion, title := "failure", "needs human review (a required check failed)"
	if st.Mode == config.Shadow {
		conclusion, title = "neutral", "would have: "+title
	}
	opts := github.UpdateCheckRunOptions{
		Name:        CheckName,
		ExternalID:  new(EncodeState(st)),
		Status:      new("completed"),
		Conclusion:  new(conclusion),
		CompletedAt: &github.Timestamp{Time: time.Now()},
	}
	if !st.Blind {
		summary := CIFailedNote(failed) + "\n\n" + run.GetOutput().GetSummary()
		opts.Output = &github.CheckRunOutput{Title: new(title), Summary: new(capSummary(summary))}
	}
	if _, _, err := gh.Checks.UpdateCheckRun(ctx, owner, repo, run.GetID(), opts); err != nil {
		return fmt.Errorf("updating check run: %w", err)
	}
	return nil
}

// MarkCIPassed undoes MarkCIFailed once no required check is failing: the
// approve the model gave this commit stands again. The original title isn't
// stored, so the new one says what happened.
func MarkCIPassed(ctx context.Context, gh *github.Client, owner, repo string, run *github.CheckRun) error {
	st, _ := DecodeState(run.GetExternalID())
	st.Verdict, st.CIHeld = verdict.Approve, false
	conclusion, title := "success", "approve (required checks passed on a re-run)"
	if st.Mode == config.Shadow {
		conclusion, title = "neutral", "would have: "+title
	}
	opts := github.UpdateCheckRunOptions{
		Name:        CheckName,
		ExternalID:  new(EncodeState(st)),
		Status:      new("completed"),
		Conclusion:  new(conclusion),
		CompletedAt: &github.Timestamp{Time: time.Now()},
	}
	if !st.Blind {
		opts.Output = &github.CheckRunOutput{Title: new(title), Summary: new(dropCINote(run.GetOutput().GetSummary()))}
	}
	if _, _, err := gh.Checks.UpdateCheckRun(ctx, owner, repo, run.GetID(), opts); err != nil {
		return fmt.Errorf("updating check run: %w", err)
	}
	return nil
}

const ciNotePrefix = "**Update:** a required check failed after LGTM scored this commit ("

// dropCINote removes the lines CIFailedNote added, and the blank line after
// one that leads the text.
func dropCINote(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for i := 0; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], ciNotePrefix) {
			if len(out) == 0 && i+1 < len(lines) && lines[i+1] == "" {
				i++
			}
			continue
		}
		out = append(out, lines[i])
	}
	return strings.Join(out, "\n")
}

// CIFailedNote is the line MarkCIFailed and Publisher.Note add.
func CIFailedNote(failed []string) string {
	names := make([]string, len(failed))
	for i, f := range failed {
		names[i] = report.Escape(f)
	}
	return ciNotePrefix + strings.Join(names, ", ") + "), so it now needs human review."
}

// Note adds a line under the marker of the sticky comment, once. The full
// comment can't be rendered again without the scores; the next scoring
// replaces it whole. With no sticky comment there is nothing to do.
func (p Publisher) Note(ctx context.Context, note string) error {
	for c, err := range p.GH.Issues.ListCommentsIter(ctx, p.PR.Owner, p.PR.Repo, p.PR.Number, &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}) {
		if err != nil {
			return fmt.Errorf("listing comments: %w", err)
		}
		rest, ok := strings.CutPrefix(c.GetBody(), report.Marker)
		if c.GetUser().GetLogin() != p.CommentAuthor || !ok {
			continue
		}
		if strings.Contains(rest, note) {
			return nil
		}
		body := report.Marker + "\n" + note + "\n" + rest
		if _, _, err := p.GH.Issues.UpdateComment(ctx, p.PR.Owner, p.PR.Repo, c.GetID(), github.IssueCommentRequest{Body: body}); err != nil {
			return fmt.Errorf("updating comment: %w", err)
		}
		return nil
	}
	return nil
}

// Unnote removes the CI-failed line from the sticky comment, which leaves
// the comment as scoring wrote it.
func (p Publisher) Unnote(ctx context.Context) error {
	for c, err := range p.GH.Issues.ListCommentsIter(ctx, p.PR.Owner, p.PR.Repo, p.PR.Number, &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}) {
		if err != nil {
			return fmt.Errorf("listing comments: %w", err)
		}
		rest, ok := strings.CutPrefix(c.GetBody(), report.Marker+"\n")
		if c.GetUser().GetLogin() != p.CommentAuthor || !ok {
			continue
		}
		body := report.Marker + "\n" + dropCINote(rest)
		if body == c.GetBody() {
			return nil
		}
		if _, _, err := p.GH.Issues.UpdateComment(ctx, p.PR.Owner, p.PR.Repo, c.GetID(), github.IssueCommentRequest{Body: body}); err != nil {
			return fmt.Errorf("updating comment: %w", err)
		}
		return nil
	}
	return nil
}

// approvedOutput is the check run state, title, and summary for a
// needs-human verdict that a qualifying human approved.
func approvedOutput(st State, by, summary string) (ext, title, sum string) {
	// A human lifted the hold, so a later CI run changes nothing.
	st.Verdict, st.CIHeld = verdict.Approve, false
	return EncodeState(st) + humanSuffix,
		fmt.Sprintf("approved by %s · LGTM had asked for human review", by),
		fmt.Sprintf("A human with write access (%s) approved this commit, so the check passes.\n\n%s", report.Escape(by), summary)
}

func capSummary(s string) string {
	if len(s) <= maxSummary {
		return s
	}
	return s[:maxSummary] + "\n\n…(truncated)"
}
