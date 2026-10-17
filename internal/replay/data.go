package replay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/Temikus/lgtm/internal/config"
	"github.com/Temikus/lgtm/internal/dataset"
	"github.com/Temikus/lgtm/internal/engine"
	"github.com/Temikus/lgtm/internal/prstate"
	"github.com/Temikus/lgtm/internal/trust"
)

// ErrNotStored means an offline run needed something the dataset lacks.
var ErrNotStored = errors.New("not in the local dataset")

// Data is where replay reads GitHub data. With a Store it reads the store
// first and keeps what it fetches; Offline never calls GitHub.
type Data struct {
	Store   *dataset.Store
	Offline bool
	Now     func() time.Time
}

func (d Data) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

// Fetcher reads a PR's diff from the store, then from GitHub.
func (d Data) Fetcher(gh *github.Client) prstate.Fetcher {
	return func(ctx context.Context, pr prstate.PR) (*prstate.Raw, error) {
		if d.Store != nil {
			if raw, ok := d.Store.Raw(dataset.Repo(pr), pr.Number, pr.HeadSHA); ok {
				return raw, nil
			}
		}
		if d.Offline {
			return nil, fmt.Errorf("diff of %s#%d: %w", dataset.Repo(pr), pr.Number, ErrNotStored)
		}
		raw, err := prstate.FetchRaw(ctx, gh, pr)
		if err == nil && d.Store != nil {
			d.Store.PutRaw(raw, d.now())
		}
		return raw, err
	}
}

// forbiddenTTL is how long a stored 403 from the permission API holds
// before it is checked again, in case the login has since gained access.
const forbiddenTTL = 30 * 24 * time.Hour

// Trusted answers the fork author check from the store, then from GitHub.
// It stores a refusal to say (403), which the same login would get again,
// so a different login, or one after forbiddenTTL, checks again. Other
// failures aren't stored and are retried next run. Offline runs use what
// is stored, whoever checked it.
func (d Data) Trusted(gh *github.Client) func(context.Context, prstate.PR) (bool, error) {
	var (
		once  sync.Once
		login string
	)
	me := func(ctx context.Context) string {
		once.Do(func() {
			if u, _, err := gh.Users.Get(ctx, ""); err == nil {
				login = u.GetLogin()
			}
		})
		return login
	}
	return func(ctx context.Context, pr prstate.PR) (bool, error) {
		if d.Store != nil {
			if r, ok := d.Store.PR(dataset.Repo(pr), pr.Number); ok && r.TrustChecked {
				switch {
				case r.TrustErr == "":
					return r.AuthorTrusted, nil
				case d.Offline || (r.TrustBy != "" && r.TrustBy == me(ctx) && r.TrustAt != nil && d.now().Sub(*r.TrustAt) < forbiddenTTL):
					return false, errors.New(r.TrustErr)
				}
			}
		}
		if d.Offline {
			return false, fmt.Errorf("%s's permission on %s: %w", pr.Author, dataset.Repo(pr), ErrNotStored)
		}
		ok, err := trust.AuthorTrusted(ctx, gh, pr.Owner, pr.Repo, pr.Author)
		if d.Store != nil {
			switch {
			case err == nil:
				d.Store.PutTrust(dataset.Repo(pr), pr.Number, dataset.Trust{Trusted: ok, At: d.now()})
			case forbidden(err):
				d.Store.PutTrust(dataset.Repo(pr), pr.Number, dataset.Trust{Err: err.Error(), By: me(ctx), At: d.now()})
			}
		}
		return ok, err
	}
}

// forbidden is a 403 that isn't a rate limit. go-github reports rate limits
// as their own error types, not as ErrorResponse.
func forbidden(err error) bool {
	var er *github.ErrorResponse
	return errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusForbidden
}

func (d Data) engineOptions(gh *github.Client, eo engine.Options) engine.Options {
	eo.Fetch, eo.Trusted = d.Fetcher(gh), d.Trusted(gh)
	eo.CI = func(context.Context, prstate.PR) ([]string, error) { return nil, nil }
	return eo
}

// cached returns p's stored sample under mode. It misses when the head
// moved, when a revert was found this run, or when the label was written
// within revertWindow of the close, since a revert could still change it.
func (d Data) cached(p *github.PullRequest, repo string, mode LabelMode, revertedBy int) (Sample, bool) {
	if d.Store == nil || revertedBy != 0 {
		return Sample{}, false
	}
	row, ok := d.Store.PR(repo, p.GetNumber())
	if !ok || row.HeadSHA != p.GetHead().GetSHA() {
		return Sample{}, false
	}
	l, ok := d.Store.Label(repo, p.GetNumber(), string(mode))
	if !ok || l.HeadSHA != row.HeadSHA || (row.Closed != nil && l.LabelledAt.Sub(*row.Closed) < revertWindow) {
		return Sample{}, false
	}
	return sampleFrom(row, l), true
}

func sampleFrom(row dataset.PR, l dataset.Label) Sample {
	s := Sample{PR: row.PR(), Label: Label(l.Label), Reason: l.Reason, Low: l.Low, Comments: int(row.Comments)}
	if row.Created != nil {
		s.Created = *row.Created
	}
	if row.Merged != nil {
		s.Merged = *row.Merged
	}
	return s
}

// keep stores a freshly labelled PR.
func (d Data) keep(full *github.PullRequest, s Sample, mode LabelMode) {
	if d.Store == nil {
		return
	}
	row := dataset.FromPR(s.PR)
	row.Created, row.Merged = dataset.Time(s.Created), dataset.Time(s.Merged)
	row.Closed = dataset.Time(full.GetClosedAt().Time)
	row.Comments, row.FetchedAt = int64(s.Comments), d.now()
	d.Store.PutPR(row)
	d.Store.PutLabel(dataset.Label{
		Repo: row.Repo, Number: row.Number, HeadSHA: row.HeadSHA, Mode: string(mode),
		Label: string(s.Label), Reason: s.Reason, Low: s.Low, Source: dataset.SourceHistory, LabelledAt: d.now(),
	})
}

// stored lists repo's PRs closed since opts.Since from the store, newest
// first, with their labels under opts.Mode.
func (d Data) stored(repo string, opts HistoryOptions) ([]Sample, error) {
	if d.Store == nil {
		return nil, fmt.Errorf("--offline needs a local dataset")
	}
	var out []Sample
	for _, row := range d.Store.PRs(repo) {
		if opts.MaxPRs > 0 && len(out) >= opts.MaxPRs {
			break
		}
		if row.Closed == nil || row.Closed.Before(opts.Since) {
			continue
		}
		if l, ok := d.Store.Label(repo, int(row.Number), string(opts.Mode)); ok && l.HeadSHA == row.HeadSHA {
			out = append(out, sampleFrom(row, l))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s since %s with --labels %s: %w; run lgtm data pull first", repo, opts.Since.Format(time.DateOnly), opts.Mode, ErrNotStored)
	}
	return out, nil
}

// Listed is one PR from a known-bad or known-good list and LGTM's verdict.
type Listed struct {
	Ref     string
	Kind    string
	Label   Label
	Verdict string
	Caps    []string
	Err     error
}

// listed returns the store's listed PRs as samples, with their kinds.
func (d Data) listed() ([]Sample, []string) {
	var ss []Sample
	var kinds []string
	for _, l := range d.Store.Labels(dataset.SourceList) {
		row, ok := d.Store.PR(l.Repo, int(l.Number))
		if !ok {
			continue
		}
		s := sampleFrom(row, l)
		// A history pull of the same PR can move the row's head; the label
		// names the commit that earned its place on the list.
		s.PR.HeadSHA = l.HeadSHA
		ss = append(ss, s)
		kinds = append(kinds, l.Kind)
	}
	return ss, kinds
}

// scoreRows turns one run's results into rows for the scores table.
func scoreRows(runID, rubricHash, mode string, ss []Sample, kinds []string, rs []*engine.Result) []dataset.Score {
	var out []dataset.Score
	for i, r := range rs {
		if r == nil || r.Verdict.Failure != nil {
			continue
		}
		s := ss[i]
		var kind string
		if kinds != nil {
			kind = kinds[i]
		}
		served := ""
		if len(r.Served) > 0 {
			served = r.Served[0]
		}
		for _, q := range r.Questions {
			row := dataset.Score{
				RunID: runID, Repo: dataset.Repo(s.PR), Number: int64(s.PR.Number), HeadSHA: s.PR.HeadSHA,
				Rubric: rubricHash, Model: r.Model, Served: served, Question: q.ID, Role: string(q.Role),
				Value: q.Value, Aggregate: r.Verdict.Aggregate, Verdict: string(r.Verdict.Verdict),
				Mode: mode, Label: string(s.Label), Low: s.Low, Kind: kind,
				Untrusted: r.Untrusted, Partial: len(r.Partial) > 0,
			}
			if q.Err != nil {
				row.Err = q.Err.Error()
			}
			out = append(out, row)
		}
	}
	return out
}

// rubricHash identifies the questions a run asked, so scores from
// different wordings can be told apart.
func rubricHash(cfg *config.Config) string {
	b, err := json.Marshal(cfg.Rubric) // #nosec G117 -- "Pass" is a list of passing options, not a secret
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}

func newRunID(now time.Time) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}
