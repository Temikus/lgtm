// Package dataset keeps what replay reads from GitHub and the model in local
// Parquet files, so later runs can skip those calls. Each table is a folder
// of part files that are never rewritten; a key's last write wins.
package dataset

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/Temikus/lgtm/internal/prstate"
)

// Table folders under the store's directory.
const (
	prsTable       = "prs"
	labelsTable    = "labels"
	filesTable     = "files"
	responsesTable = "responses"
	scoresTable    = "scores"
)

// DefaultDir is lgtm/data under the user's cache directory.
func DefaultDir() (string, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "lgtm", "data"), nil
}

// Store is safe for concurrent use. Writes stay in memory until Flush.
type Store struct {
	dir string

	mu        sync.Mutex
	prs       *table[PR]
	labels    *table[Label]
	responses *table[Response]
	files     map[string][]File // by prKey@head
	dirtyDiff map[string]bool
	scores    []Score
}

// Open loads every part under dir. A missing dir is an empty store.
func Open(dir string) (*Store, error) {
	s := &Store{dir: dir, files: map[string][]File{}, dirtyDiff: map[string]bool{}}
	var err error
	if s.prs, err = loadTable(dir, prsTable, PR.key); err != nil {
		return nil, err
	}
	if s.labels, err = loadTable(dir, labelsTable, Label.key); err != nil {
		return nil, err
	}
	if s.responses, err = loadTable(dir, responsesTable, func(r Response) string { return r.Key }); err != nil {
		return nil, err
	}
	files, err := readParts[File](dir, filesTable)
	if err != nil {
		return nil, err
	}
	byPath := map[string]map[string]File{}
	for _, f := range files {
		k := diffKey(f.Repo, f.Number, f.HeadSHA)
		if byPath[k] == nil {
			byPath[k] = map[string]File{}
		}
		byPath[k][f.Path] = f
	}
	for k, m := range byPath {
		s.files[k] = slices.SortedFunc(maps.Values(m), func(a, b File) int { return cmp.Compare(a.Path, b.Path) })
	}
	return s, nil
}

// Dir is where the store reads and writes.
func (s *Store) Dir() string { return s.dir }

// PR returns the stored PR repo#n.
func (s *Store) PR(repo string, n int) (PR, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.prs.rows[prKey(repo, int64(n))]
	return r, ok
}

// PRs returns repo's stored PRs, most recently closed first.
func (s *Store) PRs(repo string) []PR {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []PR
	for _, r := range s.prs.rows {
		if r.Repo == repo {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b PR) int {
		return cmp.Or(closedAt(b).Compare(closedAt(a)), cmp.Compare(b.Number, a.Number))
	})
	return out
}

// PutPR stores r, keeping the diff and trust fields of an earlier row for
// the same head.
func (s *Store) PutPR(r PR) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.FetchedAt.IsZero() {
		r.FetchedAt = time.Now().UTC()
	}
	if old, ok := s.prs.rows[r.key()]; ok && old.HeadSHA == r.HeadSHA {
		r.FilesFetched, r.FetchPartial, r.LinkedIssues = old.FilesFetched, old.FetchPartial, old.LinkedIssues
		r.TrustChecked, r.AuthorTrusted, r.TrustErr, r.TrustBy, r.TrustAt = old.TrustChecked, old.AuthorTrusted, old.TrustErr, old.TrustBy, old.TrustAt
	}
	s.prs.put(r)
}

// Raw returns the stored diff for repo#n at head.
func (s *Store) Raw(repo string, n int, head string) (*prstate.Raw, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.prs.rows[prKey(repo, int64(n))]
	if !ok || !r.FilesFetched || r.HeadSHA != head {
		return nil, false
	}
	raw := &prstate.Raw{PR: r.PR(), LinkedIssues: slices.Clone(r.LinkedIssues), Partial: slices.Clone(r.FetchPartial)}
	for _, f := range s.files[diffKey(repo, int64(n), head)] {
		raw.Files = append(raw.Files, f.file())
	}
	return raw, true
}

// PutRaw stores a fetched diff. It adds the PR row if there isn't one.
func (s *Store) PutRaw(raw *prstate.Raw, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.prs.rows[prKey(Repo(raw.PR), int64(raw.PR.Number))]
	if !ok || row.HeadSHA != raw.PR.HeadSHA {
		row = FromPR(raw.PR)
		row.FetchedAt = at.UTC()
	}
	row.FilesFetched, row.FetchPartial, row.LinkedIssues = true, raw.Partial, raw.LinkedIssues
	s.prs.put(row)
	k := diffKey(row.Repo, row.Number, row.HeadSHA)
	s.files[k] = nil
	for _, f := range raw.Files {
		s.files[k] = append(s.files[k], fromFile(raw.PR, f))
	}
	s.dirtyDiff[k] = true
}

// Trust is the outcome of a fork author check.
type Trust struct {
	Trusted bool
	Err     string // why the check couldn't be made; empty when it was
	By      string // GitHub login that made the check
	At      time.Time
}

// PutTrust records repo#n's author check. It does nothing if the PR isn't
// stored.
func (s *Store) PutTrust(repo string, n int, t Trust) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.prs.rows[prKey(repo, int64(n))]
	if !ok {
		return
	}
	r.TrustChecked, r.AuthorTrusted, r.TrustErr, r.TrustBy, r.TrustAt = true, t.Trusted, t.Err, t.By, Time(t.At.UTC())
	s.prs.put(r)
}

// Label returns repo#n's label under mode.
func (s *Store) Label(repo string, n int, mode string) (Label, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.labels.rows[Label{Repo: repo, Number: int64(n), Mode: mode}.key()]
	return r, ok
}

// Labels returns the labels from source ("history" or "list"), sorted by
// repo and number.
func (s *Store) Labels(source string) []Label {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Label
	for _, r := range s.labels.rows {
		if r.Source == source {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b Label) int { return cmp.Or(cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.Number, b.Number)) })
	return out
}

// PutLabel stores l.
func (s *Store) PutLabel(l Label) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.LabelledAt.IsZero() {
		l.LabelledAt = time.Now().UTC()
	}
	s.labels.put(l)
}

// Response returns the stored reply for a request key.
func (s *Store) Response(key string) (Response, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.responses.rows[key]
	return r, ok
}

// PutResponse stores r.
func (s *Store) PutResponse(r Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses.put(r)
}

// AddScores queues rows for the scores table.
func (s *Store) AddScores(rows []Score) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for _, r := range rows {
		if r.CreatedAt.IsZero() {
			r.CreatedAt = now
		}
		s.scores = append(s.scores, r)
	}
}

// ReadScores loads every scores part under dir.
func ReadScores(dir string) ([]Score, error) { return readParts[Score](dir, scoresTable) }

// Flush writes one new part per table with unsaved rows.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var files []File
	for _, k := range slices.Sorted(maps.Keys(s.dirtyDiff)) {
		files = append(files, s.files[k]...)
	}
	err := errors.Join(
		s.prs.flush(s.dir),
		s.labels.flush(s.dir),
		s.responses.flush(s.dir),
		writePart(s.dir, filesTable, files),
		writePart(s.dir, scoresTable, s.scores),
	)
	if err == nil {
		s.dirtyDiff, s.scores = map[string]bool{}, nil
	}
	return err
}

func closedAt(r PR) time.Time {
	if r.Closed == nil {
		return time.Time{}
	}
	return *r.Closed
}

func diffKey(repo string, n int64, head string) string { return prKey(repo, n) + "@" + head }

// table is an in-memory keyed table. dirty holds keys written since the
// last flush.
type table[T any] struct {
	name  string
	key   func(T) string
	rows  map[string]T
	dirty map[string]bool
}

func loadTable[T any](dir, name string, key func(T) string) (*table[T], error) {
	t := &table[T]{name: name, key: key, rows: map[string]T{}, dirty: map[string]bool{}}
	rows, err := readParts[T](dir, name)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		t.rows[key(r)] = r
	}
	return t, nil
}

func (t *table[T]) put(r T) {
	k := t.key(r)
	t.rows[k] = r
	t.dirty[k] = true
}

func (t *table[T]) flush(dir string) error {
	var rows []T
	for _, k := range slices.Sorted(maps.Keys(t.dirty)) {
		rows = append(rows, t.rows[k])
	}
	if err := writePart(dir, t.name, rows); err != nil {
		return err
	}
	t.dirty = map[string]bool{}
	return nil
}

// readParts reads a table's parts in name order, which is write order, so
// later rows override earlier ones when loaded into a map.
func readParts[T any](dir, name string) ([]T, error) {
	parts, err := filepath.Glob(filepath.Join(dir, name, "part-*.parquet"))
	if err != nil {
		return nil, err
	}
	slices.Sort(parts)
	var out []T
	for _, p := range parts {
		rows, err := parquet.ReadFile[T](p)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}
		out = append(out, rows...)
	}
	return out, nil
}

// writePart writes rows to a new part through a temporary file, so a reader
// never sees half a part.
func writePart[T any](dir, name string, rows []T) error {
	if len(rows) == 0 {
		return nil
	}
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o750); err != nil {
		return err
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	part := filepath.Join(d, "part-"+time.Now().UTC().Format("20060102T150405.000000000Z")+"-"+hex.EncodeToString(suffix)+".parquet")
	tmp := part + ".tmp"
	if err := parquet.WriteFile(tmp, rows); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if err := os.Rename(tmp, part); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
