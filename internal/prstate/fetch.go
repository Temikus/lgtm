// Package prstate fetches a PR's diff at pinned SHAs and builds the state
// the decision model sees.
package prstate

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/google/go-github/v92/github"
)

// GitHub API caps.
const (
	compareFileCap = 300
	listFilesCap   = 3000
	maxLinked      = 5
)

// ErrHeadMoved means the PR head changed while files were being listed, so
// the file list may not match the scored SHA.
var ErrHeadMoved = errors.New("PR head moved while listing files")

// ErrNoFiles means nothing is left to score after excludes.
var ErrNoFiles = errors.New("no files to score after excludes")

// PR identifies the scored PR. SHAs come from the event payload, never from
// a fresh read of the PR.
type PR struct {
	Owner, Repo  string
	Number       int
	BaseSHA      string
	HeadSHA      string
	BaseRef      string
	HeadRef      string
	Title        string
	Body         string
	Labels       []string
	Author       string
	Fork         bool
	ChangedFiles int
}

// FromPullRequest pins a PR from an event payload or API read.
func FromPullRequest(owner, repo string, p *github.PullRequest) PR {
	pr := PR{
		Owner: owner, Repo: repo, Number: p.GetNumber(),
		BaseSHA: p.GetBase().GetSHA(), HeadSHA: p.GetHead().GetSHA(),
		BaseRef: p.GetBase().GetRef(), HeadRef: p.GetHead().GetRef(),
		Title: p.GetTitle(), Body: p.GetBody(),
		Author:       p.GetUser().GetLogin(),
		ChangedFiles: p.GetChangedFiles(),
	}
	pr.Fork = p.GetHead().GetRepo().GetFullName() != p.GetBase().GetRepo().GetFullName()
	for _, l := range p.Labels {
		pr.Labels = append(pr.Labels, l.GetName())
	}
	return pr
}

// File is one changed file. Patch is empty for excluded and unseen files.
type File struct {
	Path         string `json:"path"`
	PreviousPath string `json:"previous_path,omitempty"`
	Status       string `json:"status"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
	Patch        string `json:"patch,omitempty"`
	Changes      int    `json:"-"` // GitHub's count; above 0 with no patch means GitHub omitted it
}

// Unseen is a file the model could not see in full, and why.
type Unseen struct {
	Path   string
	Reason string
}

// Snapshot is everything fetched for one scoring run.
type Snapshot struct {
	PR           PR
	LinkedIssues []string
	Files        []File // patches the model sees
	Excluded     []File // matched exclude globs; stats only
	Unseen       []Unseen
	// Partial lists reasons the model's view is incomplete. A partial view
	// never approves.
	Partial []string
	// LockSources are lockfile package sources outside DefaultRegistries,
	// read from patches before exclude globs drop them.
	LockSources []LockSource
	// AutoRuns are commands the diff sets to run on their own on a
	// developer's machine.
	AutoRuns []AutoRun
}

// Fetch reads the diff for pr and applies exclude globs.
func Fetch(ctx context.Context, gh *github.Client, pr PR, exclude []string) (*Snapshot, error) {
	raw, err := FetchRaw(ctx, gh, pr)
	if err != nil {
		return nil, err
	}
	return raw.Snapshot(exclude)
}

// Raw is what Fetch reads from GitHub, before exclude globs. Replay stores
// it so a later run can build the snapshot without GitHub.
type Raw struct {
	PR           PR
	Files        []File
	LinkedIssues []string
	Partial      []string // why GitHub's file list is incomplete
}

// Fetcher reads a PR's raw diff. Replay passes one that reads its local
// dataset first.
type Fetcher func(ctx context.Context, pr PR) (*Raw, error)

// FetchRaw reads pr's files and linked issue titles.
func FetchRaw(ctx context.Context, gh *github.Client, pr PR) (*Raw, error) {
	s := &Snapshot{PR: pr}
	files, err := s.fetchFiles(ctx, gh)
	if err != nil {
		return nil, err
	}
	raw := &Raw{PR: pr, Partial: s.Partial}
	for _, f := range files {
		raw.Files = append(raw.Files, File{
			Path: f.GetFilename(), PreviousPath: f.GetPreviousFilename(), Status: f.GetStatus(),
			Additions: f.GetAdditions(), Deletions: f.GetDeletions(), Patch: f.GetPatch(), Changes: f.GetChanges(),
		})
	}
	if len(raw.Files) > 0 {
		raw.LinkedIssues = linkedIssueTitles(ctx, gh, pr)
	}
	return raw, nil
}

// Snapshot applies exclude globs. It returns ErrNoFiles, with the snapshot,
// when nothing is left to score.
func (r *Raw) Snapshot(exclude []string) (*Snapshot, error) {
	s := &Snapshot{PR: r.PR, Partial: slices.Clone(r.Partial), LockSources: lockSources(r.Files), AutoRuns: autoRuns(r.Files)}
	for _, file := range r.Files {
		// The lockfile check reads patches, even of excluded files, so a
		// missing one leaves it blind.
		if file.Patch == "" && file.Changes > 0 && IsLockfile(file.Path) {
			s.Unseen = append(s.Unseen, Unseen{Path: file.Path, Reason: "lockfile with no patch from GitHub, so its sources weren't checked"})
			s.Partial = appendOnce(s.Partial, "a lockfile's sources couldn't be checked")
		}
		if matchAny(exclude, file.Path) {
			file.Patch = ""
			s.Excluded = append(s.Excluded, file)
			continue
		}
		if file.Patch == "" && file.Changes > 0 {
			s.Unseen = append(s.Unseen, Unseen{Path: file.Path, Reason: "no patch from GitHub (binary or too large)"})
			s.Partial = appendOnce(s.Partial, "GitHub omitted some patches")
		}
		s.Files = append(s.Files, file)
	}
	if len(s.Files) == 0 {
		return s, ErrNoFiles
	}
	s.LinkedIssues = slices.Clone(r.LinkedIssues)
	return s, nil
}

func (s *Snapshot) fetchFiles(ctx context.Context, gh *github.Client) ([]*github.CommitFile, error) {
	pr := s.PR
	if pr.ChangedFiles <= compareFileCap {
		cmp, _, err := gh.Repositories.CompareCommits(ctx, pr.Owner, pr.Repo, pr.BaseSHA, pr.HeadSHA, nil)
		if err != nil {
			return nil, fmt.Errorf("comparing %s...%s: %w", short(pr.BaseSHA), short(pr.HeadSHA), err)
		}
		if n := len(cmp.Files); n < pr.ChangedFiles || (pr.ChangedFiles == 0 && n >= compareFileCap) {
			s.Partial = appendOnce(s.Partial, fmt.Sprintf("compare returned %d of %d changed files", n, pr.ChangedFiles))
		}
		return cmp.Files, nil
	}

	s.Partial = appendOnce(s.Partial, fmt.Sprintf("%d changed files exceed the compare endpoint's %d-file cap", pr.ChangedFiles, compareFileCap))
	if err := s.checkHead(ctx, gh); err != nil {
		return nil, err
	}
	var files []*github.CommitFile
	for f, err := range gh.PullRequests.ListFilesIter(ctx, pr.Owner, pr.Repo, pr.Number, &github.ListOptions{PerPage: 100}) {
		if err != nil {
			return nil, fmt.Errorf("listing files: %w", err)
		}
		files = append(files, f)
	}
	if err := s.checkHead(ctx, gh); err != nil {
		return nil, err
	}
	if pr.ChangedFiles > listFilesCap || len(files) < pr.ChangedFiles {
		s.Partial = appendOnce(s.Partial, fmt.Sprintf("files endpoint returned %d of %d changed files", len(files), pr.ChangedFiles))
	}
	return files, nil
}

func (s *Snapshot) checkHead(ctx context.Context, gh *github.Client) error {
	cur, _, err := gh.PullRequests.Get(ctx, s.PR.Owner, s.PR.Repo, s.PR.Number)
	if err != nil {
		return fmt.Errorf("re-reading PR head: %w", err)
	}
	if got := cur.GetHead().GetSHA(); got != s.PR.HeadSHA {
		return fmt.Errorf("%w: scored %s, now %s", ErrHeadMoved, short(s.PR.HeadSHA), short(got))
	}
	return nil
}

var closingRef = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s+#(\d+)\b`)

// linkedIssueTitles reads same-repo issues the body says it closes. Titles
// are context only, so a failed read is skipped rather than failing the run.
func linkedIssueTitles(ctx context.Context, gh *github.Client, pr PR) []string {
	var titles []string
	seen := map[int]bool{}
	for _, m := range closingRef.FindAllStringSubmatch(pr.Body, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || seen[n] || n == pr.Number {
			continue
		}
		seen[n] = true
		if len(seen) > maxLinked {
			break
		}
		is, _, err := gh.Issues.Get(ctx, pr.Owner, pr.Repo, n)
		if err != nil {
			continue
		}
		titles = append(titles, is.GetTitle())
	}
	return titles
}

func matchAny(globs []string, path string) bool {
	for _, g := range globs {
		if ok, _ := doublestar.Match(g, path); ok {
			return true
		}
	}
	return false
}

func appendOnce(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
