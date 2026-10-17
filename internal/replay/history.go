// Package replay scores past PRs, compares the scores with what humans
// decided, and fits the approve and deny thresholds.
package replay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/Temikus/lgtm/internal/prstate"
	"github.com/Temikus/lgtm/internal/trust"
)

// LabelMode picks how history becomes labels.
type LabelMode string

const (
	// Strict uses other people's reviews: approved and merged is positive,
	// changes requested and closed is negative.
	Strict LabelMode = "strict"
	// Merged is for repos with one maintainer and no reviews: merged by a
	// writer is positive, closed unmerged is negative, and bot PRs are
	// skipped. The labels are weaker than Strict's.
	Merged LabelMode = "merged"
	// Maintainer adds two signals to Strict for repos that don't review
	// through GitHub, such as Prow repos: merged with an lgtm label is
	// positive, and closed unmerged by someone other than the author
	// (directly, or by a bot after their /close) is negative.
	Maintainer LabelMode = "maintainer"
)

// historyWorkers bounds concurrent GitHub reads while labelling.
const historyWorkers = 8

// Label is the human outcome for a PR.
type Label string

const (
	Unlabelled Label = ""
	Positive   Label = "positive"
	Negative   Label = "negative"
)

const (
	revertWindow     = 14 * 24 * time.Hour
	rubberStampDelay = 5 * time.Minute
	rubberStampLines = 200
)

// Sample is one past PR and its label.
type Sample struct {
	PR       prstate.PR
	Label    Label
	Reason   string
	Low      bool // low-confidence: reported but not used for the fit
	Created  time.Time
	Merged   time.Time // zero when closed unmerged
	Comments int       // review comments plus PR comments
}

// HistoryOptions bound the history read.
type HistoryOptions struct {
	Since  time.Time
	MaxPRs int
	Mode   LabelMode
	Data   Data
}

// History lists PRs closed since opts.Since, newest first, and labels them.
func History(ctx context.Context, gh *github.Client, owner, repo string, opts HistoryOptions) ([]Sample, error) {
	if opts.Data.Offline {
		return opts.Data.stored(owner+"/"+repo, opts)
	}
	closed, err := listClosed(ctx, gh, owner, repo, opts.Since)
	if err != nil {
		return nil, err
	}
	reverted := findReverts(owner, repo, closed)
	h := &historian{gh: gh, owner: owner, repo: repo, mode: opts.Mode, writers: map[string]bool{}}

	var todo []*github.PullRequest
	for _, p := range closed {
		if opts.MaxPRs > 0 && len(todo) >= opts.MaxPRs {
			break
		}
		if opts.Mode != Merged || !isBot(p.GetUser()) {
			todo = append(todo, p)
		}
	}
	out := make([]Sample, len(todo))
	err = parallel(ctx, len(todo), historyWorkers, func(i int) error {
		n := todo[i].GetNumber()
		if s, ok := opts.Data.cached(todo[i], owner+"/"+repo, opts.Mode, reverted[n]); ok {
			out[i] = s
			return nil
		}
		full, _, err := gh.PullRequests.Get(ctx, owner, repo, n)
		if err != nil {
			return fmt.Errorf("reading #%d: %w", n, err)
		}
		if out[i], err = h.label(ctx, full, reverted[n]); err != nil {
			return err
		}
		opts.Data.keep(full, out[i], opts.Mode)
		return nil
	})
	return out, err
}

// parallel runs f(0..n-1) on up to workers goroutines and returns the first
// error. Work not yet started stops after an error.
func parallel(ctx context.Context, n, workers int, f func(int) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := range n {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := f(i); err != nil {
				cancel(err)
			}
		})
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return ctx.Err()
}

// listClosed pages by update time, which is never before close time, so it
// can stop at the first PR last updated before since.
func listClosed(ctx context.Context, gh *github.Client, owner, repo string, since time.Time) ([]*github.PullRequest, error) {
	var out []*github.PullRequest
	opts := &github.PullRequestListOptions{State: "closed", Sort: "updated", Direction: "desc", ListOptions: github.ListOptions{PerPage: 100}}
	for p, err := range gh.PullRequests.ListIter(ctx, owner, repo, opts) {
		if err != nil {
			return nil, fmt.Errorf("listing closed PRs: %w", err)
		}
		if p.GetUpdatedAt().Before(since) {
			break
		}
		if !p.GetClosedAt().Before(since) {
			out = append(out, p)
		}
	}
	return out, nil
}

var (
	revertTitle  = regexp.MustCompile(`^Revert "(.+)"$`)
	revertsPR    = regexp.MustCompile(`(?i)\breverts\s+([\w.-]+/[\w.-]+)?#(\d+)\b`)
	revertCommit = regexp.MustCompile(`(?i)\bthis reverts commit ([0-9a-f]{7,40})\b`)
)

// findReverts maps a PR number to the merged PR that reverted it within
// revertWindow. It matches GitHub's revert button ("Reverts o/r#N"), git's
// message ("This reverts commit <sha>"), and the title `Revert "<title>"`.
func findReverts(owner, repo string, prs []*github.PullRequest) map[int]int {
	byTitle, byNumber := map[string]*github.PullRequest{}, map[int]*github.PullRequest{}
	var merged []*github.PullRequest
	for _, p := range prs {
		if p.MergedAt == nil {
			continue
		}
		merged = append(merged, p)
		byTitle[p.GetTitle()] = p
		byNumber[p.GetNumber()] = p
	}
	out := map[int]int{}
	for _, rv := range merged {
		for _, t := range revertTargets(owner+"/"+repo, rv, merged, byTitle, byNumber) {
			if t == nil || t.GetNumber() == rv.GetNumber() {
				continue
			}
			gap := rv.GetMergedAt().Sub(t.GetMergedAt().Time)
			if gap >= 0 && gap <= revertWindow {
				out[t.GetNumber()] = rv.GetNumber()
			}
		}
	}
	return out
}

func revertTargets(fullName string, rv *github.PullRequest, merged []*github.PullRequest, byTitle map[string]*github.PullRequest, byNumber map[int]*github.PullRequest) []*github.PullRequest {
	var out []*github.PullRequest
	if m := revertTitle.FindStringSubmatch(rv.GetTitle()); m != nil {
		out = append(out, byTitle[m[1]])
	}
	for _, m := range revertsPR.FindAllStringSubmatch(rv.GetBody(), -1) {
		if m[1] == "" || strings.EqualFold(m[1], fullName) {
			n, _ := strconv.Atoi(m[2])
			out = append(out, byNumber[n])
		}
	}
	for _, m := range revertCommit.FindAllStringSubmatch(rv.GetBody(), -1) {
		for _, p := range merged {
			if sha := p.GetMergeCommitSHA(); sha != "" && strings.HasPrefix(sha, m[1]) {
				out = append(out, p)
			}
		}
	}
	return out
}

type historian struct {
	gh          *github.Client
	owner, repo string
	mode        LabelMode
	mu          sync.Mutex
	writers     map[string]bool
	noPermAPI   bool
}

func (h *historian) label(ctx context.Context, p *github.PullRequest, revertedBy int) (Sample, error) {
	s := Sample{
		PR:       prstate.FromPullRequest(h.owner, h.repo, p),
		Created:  p.GetCreatedAt().Time,
		Merged:   p.GetMergedAt().Time,
		Comments: p.GetComments() + p.GetReviewComments(),
	}
	if revertedBy != 0 {
		s.Label, s.Reason = Negative, fmt.Sprintf("reverted by #%d within 14 days", revertedBy)
		return s, nil
	}
	if h.mode == Merged {
		return labelMerged(p, s), nil
	}
	return h.labelStrict(ctx, p, s)
}

// labelMerged needs no permission check: merging takes write access.
func labelMerged(p *github.PullRequest, s Sample) Sample {
	if !p.GetMerged() {
		s.Label, s.Reason = Negative, "closed without merging"
		return s
	}
	s.Label, s.Reason = Positive, "merged by "+p.GetMergedBy().GetLogin()
	return s
}

func (h *historian) labelStrict(ctx context.Context, p *github.PullRequest, s Sample) (Sample, error) {
	latest, changes, err := h.reviews(ctx, p)
	if err != nil {
		return s, err
	}
	if !p.GetMerged() {
		if changes != "" {
			s.Label, s.Reason = Negative, "closed after "+changes+" requested changes"
			return s, nil
		}
		if h.mode == Maintainer {
			return h.labelClosed(ctx, p, s)
		}
		return s, nil
	}
	for _, r := range latest {
		if r.GetState() != "APPROVED" {
			continue
		}
		s.Label, s.Reason = Positive, "approved by "+r.GetUser().GetLogin()
		low, err := h.rubberStamp(ctx, p, r)
		if err != nil {
			return s, err
		}
		if low {
			s.Low, s.Reason = true, s.Reason+" within 5 minutes of a large diff, without comments"
		}
		return s, nil
	}
	if h.mode == Maintainer && hasLabel(p, "lgtm") {
		s.Label, s.Reason = Positive, "merged with the lgtm label"
	}
	return s, nil
}

var closeCommand = regexp.MustCompile(`(?m)^/close\s*$`)

// labelClosed makes a PR negative when someone other than its author closed
// it: directly, or through a bot (Prow) after commenting /close. Stale
// closures and authors closing their own PRs stay unlabelled.
func (h *historian) labelClosed(ctx context.Context, p *github.PullRequest, s Sample) (Sample, error) {
	if hasLabelPrefix(p, "lifecycle/") {
		s.Reason = "closed as stale"
		return s, nil
	}
	author := p.GetUser().GetLogin()
	closer, err := h.closer(ctx, p.GetNumber())
	switch {
	case err != nil || closer == nil:
		return s, err
	case closer.GetLogin() == author:
		s.Reason = "closed by its author"
		return s, nil
	case !isBot(closer):
		s.Label, s.Reason = Negative, "closed by "+closer.GetLogin()
		return s, nil
	}
	for c, err := range h.gh.Issues.ListCommentsIter(ctx, h.owner, h.repo, p.GetNumber(), &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}) {
		if err != nil {
			return s, fmt.Errorf("listing comments on #%d: %w", p.GetNumber(), err)
		}
		by := c.GetUser()
		if by.GetLogin() != author && !isBot(by) && closeCommand.MatchString(c.GetBody()) {
			s.Label, s.Reason = Negative, "closed after "+by.GetLogin()+" commented /close"
		}
	}
	return s, nil
}

// closer is who last closed the PR, or nil if no event says.
func (h *historian) closer(ctx context.Context, n int) (*github.User, error) {
	var who *github.User
	for e, err := range h.gh.Issues.ListIssueEventsIter(ctx, h.owner, h.repo, n, &github.ListOptions{PerPage: 100}) {
		if err != nil {
			return nil, fmt.Errorf("listing events on #%d: %w", n, err)
		}
		if e.GetEvent() == "closed" {
			who = e.GetActor()
		}
	}
	return who, nil
}

func hasLabel(p *github.PullRequest, name string) bool {
	return slices.ContainsFunc(p.Labels, func(l *github.Label) bool { return l.GetName() == name })
}

func hasLabelPrefix(p *github.PullRequest, prefix string) bool {
	return slices.ContainsFunc(p.Labels, func(l *github.Label) bool { return strings.HasPrefix(l.GetName(), prefix) })
}

// reviews returns each writer's latest approving or changes-requested
// review, ignoring the author, and the first writer who requested changes.
func (h *historian) reviews(ctx context.Context, p *github.PullRequest) ([]*github.PullRequestReview, string, error) {
	author := p.GetUser().GetLogin()
	latest := map[string]*github.PullRequestReview{}
	var order []string
	changes := ""
	for r, err := range h.gh.PullRequests.ListReviewsIter(ctx, h.owner, h.repo, p.GetNumber(), &github.ListOptions{PerPage: 100}) {
		if err != nil {
			return nil, "", fmt.Errorf("listing reviews on #%d: %w", p.GetNumber(), err)
		}
		state, login := r.GetState(), r.GetUser().GetLogin()
		if (state != "APPROVED" && state != "CHANGES_REQUESTED" && state != "DISMISSED") || login == author {
			continue
		}
		ok, err := h.writer(ctx, login, r.GetAuthorAssociation())
		if err != nil {
			return nil, "", err
		}
		if !ok {
			continue
		}
		if state == "CHANGES_REQUESTED" && changes == "" {
			changes = login
		}
		if _, seen := latest[login]; !seen {
			order = append(order, login)
		}
		latest[login] = r
	}
	out := make([]*github.PullRequestReview, 0, len(order))
	for _, l := range order {
		out = append(out, latest[l])
	}
	return out, changes, nil
}

// rubberStamp: no review comments, over 200 changed lines, and approved
// within 5 minutes of the approved commit or the PR opening, whichever is
// later.
func (h *historian) rubberStamp(ctx context.Context, p *github.PullRequest, r *github.PullRequestReview) (bool, error) {
	if p.GetReviewComments() > 0 || strings.TrimSpace(r.GetBody()) != "" || p.GetAdditions()+p.GetDeletions() <= rubberStampLines {
		return false, nil
	}
	start := p.GetCreatedAt().Time
	if sha := r.GetCommitID(); sha != "" {
		c, _, err := h.gh.Git.GetCommit(ctx, h.owner, h.repo, sha)
		if err != nil {
			return false, fmt.Errorf("reading approved commit %s: %w", sha, err)
		}
		if t := c.GetCommitter().GetDate().Time; t.After(start) {
			start = t
		}
	}
	return r.GetSubmittedAt().Sub(start) < rubberStampDelay, nil
}

// writer reports whether login has write access. The permission API needs
// push access, so on a repo the caller can't push to, replay falls back to
// the review's author association. That is fine for labelling history; the
// Action never uses it to decide trust.
func (h *historian) writer(ctx context.Context, login, assoc string) (bool, error) {
	if login == "" {
		return false, nil
	}
	h.mu.Lock()
	ok, seen := h.writers[login]
	noPermAPI := h.noPermAPI
	h.mu.Unlock()
	switch {
	case noPermAPI:
		return writerAssociations[assoc], nil
	case seen:
		return ok, nil
	}
	ok, err := trust.AuthorTrusted(ctx, h.gh, h.owner, h.repo, login)
	var ge *github.ErrorResponse
	h.mu.Lock()
	defer h.mu.Unlock()
	if errors.As(err, &ge) && ge.Response.StatusCode == http.StatusForbidden {
		h.noPermAPI = true
		return writerAssociations[assoc], nil
	}
	if err != nil {
		return false, err
	}
	h.writers[login] = ok
	return ok, nil
}

var writerAssociations = map[string]bool{"OWNER": true, "MEMBER": true, "COLLABORATOR": true}

// isBot also catches robot accounts that are plain users, such as
// k8s-triage-robot, whose /close comments close stale PRs.
func isBot(u *github.User) bool {
	l := strings.ToLower(u.GetLogin())
	return u.GetType() == "Bot" || strings.HasSuffix(l, "[bot]") || strings.HasSuffix(l, "-bot") || strings.HasSuffix(l, "-robot")
}
