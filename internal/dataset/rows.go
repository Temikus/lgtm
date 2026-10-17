package dataset

import (
	"strconv"
	"strings"
	"time"

	"github.com/Temikus/lgtm/internal/prstate"
)

// PR is one pull request as read from GitHub. FilesFetched means the files
// table holds its full diff, which may be no files at all.
type PR struct {
	Repo          string     `parquet:"repo,dict"`
	Number        int64      `parquet:"number"`
	BaseSHA       string     `parquet:"base_sha"`
	HeadSHA       string     `parquet:"head_sha"`
	BaseRef       string     `parquet:"base_ref,dict"`
	HeadRef       string     `parquet:"head_ref"`
	Title         string     `parquet:"title"`
	Body          string     `parquet:"body,zstd"`
	Labels        []string   `parquet:"labels,list"`
	Author        string     `parquet:"author,dict"`
	Fork          bool       `parquet:"fork"`
	ChangedFiles  int64      `parquet:"changed_files"`
	Created       *time.Time `parquet:"created_at,optional,timestamp(millisecond)"`
	Closed        *time.Time `parquet:"closed_at,optional,timestamp(millisecond)"` // nil while open
	Merged        *time.Time `parquet:"merged_at,optional,timestamp(millisecond)"` // nil unless merged
	Comments      int64      `parquet:"comments"`
	FilesFetched  bool       `parquet:"files_fetched"`
	FetchPartial  []string   `parquet:"fetch_partial,list"`
	LinkedIssues  []string   `parquet:"linked_issues,list"`
	TrustChecked  bool       `parquet:"trust_checked"`
	AuthorTrusted bool       `parquet:"author_trusted"`
	TrustErr      string     `parquet:"trust_err"`     // why the check couldn't be made, such as no permission to ask
	TrustBy       string     `parquet:"trust_by,dict"` // GitHub login that made the check
	TrustAt       *time.Time `parquet:"trust_at,optional,timestamp(millisecond)"`
	FetchedAt     time.Time  `parquet:"fetched_at,timestamp(millisecond)"`
}

// Label is a human outcome for a PR under one labelling mode. Source is
// "history" for labels read from the repo and "list" for a known-bad list,
// whose Kind says what sort of bad (attack, injection, spam).
type Label struct {
	Repo       string    `parquet:"repo,dict"`
	Number     int64     `parquet:"number"`
	HeadSHA    string    `parquet:"head_sha"`
	Mode       string    `parquet:"mode,dict"`
	Label      string    `parquet:"label,dict"`
	Reason     string    `parquet:"reason"`
	Low        bool      `parquet:"low_confidence"`
	Source     string    `parquet:"source,dict"`
	Kind       string    `parquet:"kind,dict"`
	LabelledAt time.Time `parquet:"labelled_at,timestamp(millisecond)"`
}

// File is one changed file, stored before exclude globs.
type File struct {
	Repo         string `parquet:"repo,dict"`
	Number       int64  `parquet:"number"`
	HeadSHA      string `parquet:"head_sha"`
	Path         string `parquet:"path"`
	PreviousPath string `parquet:"previous_path"`
	Status       string `parquet:"status,dict"`
	Additions    int64  `parquet:"additions"`
	Deletions    int64  `parquet:"deletions"`
	Changes      int64  `parquet:"changes"`
	Patch        string `parquet:"patch,zstd"`
}

// Response is one model reply, keyed by a hash of the request.
type Response struct {
	Key          string    `parquet:"key"`
	Model        string    `parquet:"model,dict"`
	Served       string    `parquet:"served,dict"`
	ID           string    `parquet:"id"`
	Answers      string    `parquet:"answers,zstd"` // JSON map of question ID to jev.Answer
	CostUSD      float64   `parquet:"cost_usd"`
	InputTokens  int64     `parquet:"input_tokens"`
	OutputTokens int64     `parquet:"output_tokens"`
	CreatedAt    time.Time `parquet:"created_at,timestamp(millisecond)"`
}

// Score is one question's value for one PR in one replay run: the table to
// fit thresholds or models against.
type Score struct {
	RunID     string    `parquet:"run_id,dict"`
	Repo      string    `parquet:"repo,dict"`
	Number    int64     `parquet:"number"`
	HeadSHA   string    `parquet:"head_sha"`
	Rubric    string    `parquet:"rubric,dict"` // hash of the rubric
	Model     string    `parquet:"model,dict"`
	Served    string    `parquet:"served,dict"`
	Question  string    `parquet:"question,dict"`
	Role      string    `parquet:"role,dict"`
	Value     float64   `parquet:"value"`
	Err       string    `parquet:"err"`
	Aggregate float64   `parquet:"aggregate"`
	Verdict   string    `parquet:"verdict,dict"`
	Mode      string    `parquet:"mode,dict"`
	Label     string    `parquet:"label,dict"`
	Low       bool      `parquet:"low_confidence"`
	Kind      string    `parquet:"kind,dict"`
	Untrusted bool      `parquet:"untrusted"`
	Partial   bool      `parquet:"partial"`
	CreatedAt time.Time `parquet:"created_at,timestamp(millisecond)"`
}

// Time returns a pointer to t, or nil for the zero time.
func Time(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// Label sources.
const (
	SourceHistory = "history" // labelled from the repo's own history
	SourceList    = "list"    // from a known-bad list
)

func prKey(repo string, n int64) string { return repo + "#" + strconv.FormatInt(n, 10) }

func (r PR) key() string    { return prKey(r.Repo, r.Number) }
func (r Label) key() string { return prKey(r.Repo, r.Number) + "@" + r.Mode }

// Repo is "owner/repo" for p.
func Repo(p prstate.PR) string { return p.Owner + "/" + p.Repo }

// FromPR copies the identifying fields of p into a row.
func FromPR(p prstate.PR) PR {
	return PR{
		Repo: Repo(p), Number: int64(p.Number), BaseSHA: p.BaseSHA, HeadSHA: p.HeadSHA,
		BaseRef: p.BaseRef, HeadRef: p.HeadRef, Title: p.Title, Body: p.Body, Labels: p.Labels,
		Author: p.Author, Fork: p.Fork, ChangedFiles: int64(p.ChangedFiles),
	}
}

// PR rebuilds the prstate.PR the row came from.
func (r PR) PR() prstate.PR {
	owner, repo := splitRepo(r.Repo)
	return prstate.PR{
		Owner: owner, Repo: repo, Number: int(r.Number), BaseSHA: r.BaseSHA, HeadSHA: r.HeadSHA,
		BaseRef: r.BaseRef, HeadRef: r.HeadRef, Title: r.Title, Body: r.Body, Labels: r.Labels,
		Author: r.Author, Fork: r.Fork, ChangedFiles: int(r.ChangedFiles),
	}
}

func splitRepo(s string) (string, string) {
	owner, repo, _ := strings.Cut(s, "/")
	return owner, repo
}

func fromFile(p prstate.PR, f prstate.File) File {
	return File{
		Repo: Repo(p), Number: int64(p.Number), HeadSHA: p.HeadSHA, Path: f.Path, PreviousPath: f.PreviousPath,
		Status: f.Status, Additions: int64(f.Additions), Deletions: int64(f.Deletions), Changes: int64(f.Changes), Patch: f.Patch,
	}
}

func (r File) file() prstate.File {
	return prstate.File{
		Path: r.Path, PreviousPath: r.PreviousPath, Status: r.Status,
		Additions: int(r.Additions), Deletions: int(r.Deletions), Changes: int(r.Changes), Patch: r.Patch,
	}
}
