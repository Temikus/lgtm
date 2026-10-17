package replay

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/go-github/v92/github"

	"github.com/Temikus/lgtm/internal/dataset"
	"github.com/Temikus/lgtm/internal/prstate"
)

// PullStats says what a pull read.
type PullStats struct {
	PRs, Labelled, Diffs, Failed int
	Cached                       int // already stored, so not read again
}

// Pull labels repo's history and stores each labelled PR's diff and author
// check, so a later replay can run with --offline.
func Pull(ctx context.Context, gh *github.Client, owner, repo string, opts HistoryOptions) (PullStats, error) {
	if opts.Data.Store == nil {
		return PullStats{}, errors.New("pull needs a local dataset")
	}
	samples, err := History(ctx, gh, owner, repo, opts)
	if err != nil {
		return PullStats{}, err
	}
	st := PullStats{PRs: len(samples)}
	var labelled []prstate.PR
	for _, s := range samples {
		if s.Label != Unlabelled {
			labelled = append(labelled, s.PR)
		}
	}
	st.Labelled = len(labelled)
	st.Diffs, st.Cached, st.Failed = opts.Data.fetchAll(ctx, gh, labelled)
	return st, nil
}

// fetchAll stores the diff of each PR, and the author check for forks.
// cached counts diffs that were already stored.
func (d Data) fetchAll(ctx context.Context, gh *github.Client, prs []prstate.PR) (diffs, cached, failed int) {
	fetch, trusted := d.Fetcher(gh), d.Trusted(gh)
	for _, pr := range prs {
		if _, ok := d.Store.Raw(dataset.Repo(pr), pr.Number, pr.HeadSHA); ok {
			cached++
		}
	}
	errs := make([]error, len(prs))
	_ = parallel(ctx, len(prs), historyWorkers, func(i int) error {
		if _, err := fetch(ctx, prs[i]); err != nil {
			errs[i] = err
			return nil
		}
		if prs[i].Fork {
			// A failed check makes the PR untrusted when scored; the diff is
			// still stored.
			_, _ = trusted(ctx, prs[i])
		}
		return nil
	})
	for _, err := range errs {
		if err != nil {
			failed++
		}
	}
	return len(prs) - failed, cached, failed
}

// ListEntry is one line of a PR list: "<kind> <owner/repo#N>[@sha]", or
// "<kind> <owner/repo@sha>" for a commit pushed without a PR. On a PR, SHA
// pins the commit to score, for a PR whose head later moved past the commit
// that earned it its place on the list. Number is 0 for a commit.
type ListEntry struct {
	Kind, Owner, Repo string
	Number            int
	SHA               string
}

var listLine = regexp.MustCompile(`^(\w[\w-]*)\s+([\w.-]+)/([\w.-]+)(?:#(\d+)(?:@([0-9a-f]{40}))?|@([0-9a-f]{40}))$`)

// ParseList reads a PR list. Blank lines and lines starting with #
// are skipped.
func ParseList(r io.Reader) ([]ListEntry, error) {
	var out []ListEntry
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := listLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("line %d: %q: want <kind> <owner/repo#N>[@sha] or <kind> <owner/repo@sha>, with a 40-hex sha", n, line)
		}
		if m[6] != "" {
			out = append(out, ListEntry{Kind: m[1], Owner: m[2], Repo: m[3], SHA: m[6]})
			continue
		}
		num, err := strconv.Atoi(m[4])
		if err != nil || num <= 0 {
			return nil, fmt.Errorf("line %d: %q: PR number must be positive", n, line)
		}
		out = append(out, ListEntry{Kind: m[1], Owner: m[2], Repo: m[3], Number: num, SHA: m[5]})
	}
	return out, sc.Err()
}

// PullList stores each listed PR with label (negative for a known-bad
// list, positive for a known-good one) and its kind. PRs already stored
// that way are skipped; ones GitHub no longer serves are counted as failed.
func PullList(ctx context.Context, gh *github.Client, d Data, entries []ListEntry, label Label) (PullStats, error) {
	if d.Store == nil {
		return PullStats{}, errors.New("pull needs a local dataset")
	}
	if label != Positive && label != Negative {
		return PullStats{}, fmt.Errorf("list label must be positive or negative, got %q", label)
	}
	st := PullStats{PRs: len(entries)}
	var todo []prstate.PR
	for _, e := range entries {
		repo, num := e.Owner+"/"+e.Repo, e.Number
		if num == 0 {
			num = commitNumber(e.SHA)
			// Two listed commits whose SHAs share a 7-hex prefix would share
			// a row; refuse the second rather than score the wrong one.
			if l, ok := d.Store.Label(repo, num, dataset.SourceList); ok && l.HeadSHA != e.SHA {
				st.Failed++
				continue
			}
		}
		if l, ok := d.Store.Label(repo, num, dataset.SourceList); ok && l.Kind == e.Kind && l.Label == string(label) && (e.SHA == "" || l.HeadSHA == e.SHA) {
			if _, ok := d.Store.Raw(repo, num, l.HeadSHA); ok {
				st.Labelled++
				st.Cached++
				continue
			}
		}
		row, pr, err := listedRow(ctx, gh, e)
		if err != nil {
			st.Failed++
			continue
		}
		row.FetchedAt = d.now()
		d.Store.PutPR(row)
		d.Store.PutLabel(dataset.Label{
			Repo: repo, Number: int64(num), HeadSHA: pr.HeadSHA, Mode: dataset.SourceList,
			Label: string(label), Reason: "listed as " + string(label) + ": " + e.Kind, Source: dataset.SourceList, Kind: e.Kind, LabelledAt: d.now(),
		})
		st.Labelled++
		todo = append(todo, pr)
	}
	diffs, _, failed := d.fetchAll(ctx, gh, todo)
	st.Diffs, st.Failed = st.Cached+diffs, st.Failed+failed
	return st, nil
}

// listedRow reads a list entry's PR, or its commit as a PR with no number.
func listedRow(ctx context.Context, gh *github.Client, e ListEntry) (dataset.PR, prstate.PR, error) {
	if e.Number == 0 {
		c, _, err := gh.Repositories.GetCommit(ctx, e.Owner, e.Repo, e.SHA, nil)
		if err != nil {
			return dataset.PR{}, prstate.PR{}, err
		}
		if len(c.Parents) == 0 {
			return dataset.PR{}, prstate.PR{}, fmt.Errorf("%s/%s@%s is a root commit", e.Owner, e.Repo, e.SHA)
		}
		title, body, _ := strings.Cut(c.GetCommit().GetMessage(), "\n")
		pr := prstate.PR{
			Owner: e.Owner, Repo: e.Repo, Number: commitNumber(e.SHA), BaseSHA: c.Parents[0].GetSHA(), HeadSHA: e.SHA,
			Title: title, Body: strings.TrimSpace(body), Author: c.GetAuthor().GetLogin(),
		}
		row := dataset.FromPR(pr)
		row.Created = dataset.Time(c.GetCommit().GetAuthor().GetDate().Time)
		return row, pr, nil
	}
	p, _, err := gh.PullRequests.Get(ctx, e.Owner, e.Repo, e.Number)
	if err != nil {
		return dataset.PR{}, prstate.PR{}, err
	}
	pr := prstate.FromPullRequest(e.Owner, e.Repo, p)
	if e.SHA != "" {
		// The PR's file count is for its current head, so it can't check
		// the pinned compare for missing files.
		pr.HeadSHA, pr.ChangedFiles = e.SHA, 0
	}
	row := dataset.FromPR(pr)
	row.Created, row.Closed, row.Merged = dataset.Time(p.GetCreatedAt().Time), dataset.Time(p.GetClosedAt().Time), dataset.Time(p.GetMergedAt().Time)
	row.Comments = int64(p.GetComments() + p.GetReviewComments())
	return row, pr, nil
}

// commitNumber stands in for a PR number on a listed commit. It is negative,
// so it can't clash with a real PR, and stable for the same SHA.
func commitNumber(sha string) int {
	n, _ := strconv.ParseInt(sha[:7], 16, 64)
	return -int(n) - 1
}

// listRef names a listed PR, or a listed commit by its SHA.
func listRef(pr prstate.PR) string {
	if pr.Number < 0 {
		return fmt.Sprintf("%s/%s@%.12s", pr.Owner, pr.Repo, pr.HeadSHA)
	}
	return fmt.Sprintf("%s/%s#%d", pr.Owner, pr.Repo, pr.Number)
}
