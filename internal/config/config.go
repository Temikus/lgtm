// Package config loads and validates .github/lgtm.toml.
package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/google/go-github/v92/github"
	"github.com/pelletier/go-toml/v2"

	"github.com/Temikus/lgtm/internal/jev"
	"github.com/Temikus/lgtm/internal/rubric"
	"github.com/Temikus/lgtm/internal/verdict"
)

// Path is where the config lives in a repo.
const Path = ".github/lgtm.toml"

// DefaultModel is the decision model used when config names none.
const DefaultModel = "typesafe/jev-1.13"

const (
	maxQuestions        = 20
	maxInstructionChars = 2000
	maxChoiceOptions    = 32
)

// Mode decides whether the verdict gates anything.
type Mode string

const (
	Shadow  Mode = "shadow"
	Enforce Mode = "enforce"
)

// DefaultExclude keeps generated and vendored files out of patches. Matching
// files are still listed in state.excluded and in the report.
var DefaultExclude = []string{
	"**/*.lock", "**/go.sum", "**/package-lock.json", "**/pnpm-lock.yaml",
	"vendor/**", "**/*.pb.go", "**/*.min.*",
}

// DefaultCapAt holds approve when a cap question scores below it. Injection
// attempts scored 0.14 or less and ordinary PRs 0.41 or more
// (docs/calibration.md).
const DefaultCapAt = 0.3

// DefaultVetoAt denies when a veto question scores below it, whatever the
// aggregate. It is global, not fitted per repo: what makes a diff malicious
// doesn't depend on the repo. It is the highest sweep value with no
// legitimate PR below it; the lowest scored 0.43 (docs/calibration.md#safety).
const DefaultVetoAt = 0.4

// Calibration is written by `lgtm replay --write`.
type Calibration struct {
	Model            string         `toml:"model"`
	Snapshot         string         `toml:"snapshot"` // dated version the replies named
	Date             toml.LocalDate `toml:"date"`
	NLabelled        int            `toml:"n_labelled"`
	ApproveAt        float64        `toml:"approve_at"`
	DenyAt           float64        `toml:"deny_at"`
	ApprovePrecision float64        `toml:"approve_precision"`
	FalseApprove     float64        `toml:"false_approve"` // share of rejected PRs approve_at would approve
	FalseDeny        float64        `toml:"false_deny"`    // share of merged PRs deny_at would deny
	NNegative        int            `toml:"n_negative"`
}

// Config is a loaded, validated config with defaults filled in.
type Config struct {
	Provider          string
	Model             string
	Mode              Mode
	ApproveAt         float64
	DenyAt            float64
	CapAt             float64
	VetoAt            float64
	Aggregate         verdict.Aggregate
	TrustForks        bool
	Exclude           []string
	BorderlineMargin  float64
	AllowUncalibrated bool
	Calibration       *Calibration
	Rubric            []rubric.Question
}

// file is the TOML shape. Pointers tell "unset" from zero.
type file struct {
	Provider          *string                    `toml:"provider"`
	Model             *string                    `toml:"model"`
	Mode              *string                    `toml:"mode"`
	ApproveAt         *float64                   `toml:"approve_at"`
	DenyAt            *float64                   `toml:"deny_at"`
	CapAt             *float64                   `toml:"cap_at"`
	VetoAt            *float64                   `toml:"veto_at"`
	Aggregate         *string                    `toml:"aggregate"`
	TrustForks        *bool                      `toml:"trust_forks"`
	Exclude           []string                   `toml:"exclude"`
	BorderlineMargin  *float64                   `toml:"borderline_margin"`
	AllowUncalibrated *bool                      `toml:"allow_uncalibrated"`
	Calibration       *Calibration               `toml:"calibration"`
	Questions         map[string]rubric.Override `toml:"questions"`
}

// Load parses and validates config. Empty input gives the defaults.
func Load(data []byte) (*Config, error) {
	var probe map[string]any
	if err := toml.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", Path, err)
	}
	for _, k := range []string{"base_url", "base-url", "url", "endpoint"} {
		if _, ok := probe[k]; ok {
			return nil, fmt.Errorf("%s: %q is not allowed; a custom provider URL can only come from LGTM_BASE_URL or the Action's base-url input", Path, k)
		}
	}

	var f file
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			return nil, fmt.Errorf("%s: unknown keys:\n%s", Path, strict.String())
		}
		return nil, fmt.Errorf("parsing %s: %w", Path, err)
	}

	c := &Config{
		Provider:          deref(f.Provider, "openrouter"),
		Model:             deref(f.Model, DefaultModel),
		Mode:              Mode(deref(f.Mode, string(Shadow))),
		ApproveAt:         deref(f.ApproveAt, 0.9),
		DenyAt:            deref(f.DenyAt, 0.1),
		CapAt:             deref(f.CapAt, DefaultCapAt),
		VetoAt:            deref(f.VetoAt, DefaultVetoAt),
		Aggregate:         verdict.Aggregate(deref(f.Aggregate, string(verdict.Min))),
		TrustForks:        deref(f.TrustForks, false),
		Exclude:           f.Exclude,
		BorderlineMargin:  deref(f.BorderlineMargin, 0.05),
		AllowUncalibrated: deref(f.AllowUncalibrated, false),
		Calibration:       f.Calibration,
	}
	if c.Exclude == nil {
		c.Exclude = slices.Clone(DefaultExclude)
	}
	q, err := rubric.Merge(rubric.Defaults(), f.Questions)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", Path, err)
	}
	c.Rubric = q
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", Path, err)
	}
	return c, nil
}

// LoadFile reads config from a local checkout. A missing file gives defaults.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is the user's own checkout
	if errors.Is(err, os.ErrNotExist) {
		return Load(nil)
	}
	if err != nil {
		return nil, err
	}
	return Load(data)
}

// FetchAtSHA reads config through the Contents API at the PR's base SHA, so a
// PR can never change its own rubric, thresholds, mode, or provider. A
// missing file gives defaults; any other error fails.
func FetchAtSHA(ctx context.Context, gh *github.Client, owner, repo, baseSHA string) (*Config, error) {
	fc, _, resp, err := gh.Repositories.GetContents(ctx, owner, repo, Path, &github.RepositoryContentGetOptions{Ref: baseSHA})
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return Load(nil)
	}
	if err != nil {
		return nil, fmt.Errorf("fetching %s at %s: %w", Path, baseSHA, err)
	}
	if fc == nil {
		return nil, fmt.Errorf("%s at %s is not a file", Path, baseSHA)
	}
	content, err := fc.GetContent()
	if err != nil {
		return nil, fmt.Errorf("decoding %s: %w", Path, err)
	}
	return Load([]byte(content))
}

// Thresholds returns the approve and deny thresholds in effect: the
// calibration's when it was made for the configured model, else the
// top-level ones.
func (c *Config) Thresholds() (approveAt, denyAt float64) {
	if c.Calibrated() {
		return c.Calibration.ApproveAt, c.Calibration.DenyAt
	}
	return c.ApproveAt, c.DenyAt
}

// Policy is the verdict policy in effect.
func (c *Config) Policy() verdict.Policy {
	a, d := c.Thresholds()
	return verdict.Policy{ApproveAt: a, DenyAt: d, CapAt: c.CapAt, VetoAt: c.VetoAt, Margin: c.BorderlineMargin, Aggregate: c.Aggregate}
}

// Snapshot is the dated model version the thresholds in effect were fitted
// on, or "" when they don't come from a calibration that recorded one.
func (c *Config) Snapshot() string {
	if c.Calibrated() {
		return c.Calibration.Snapshot
	}
	return ""
}

// Calibrated reports whether a calibration exists for the configured model.
func (c *Config) Calibrated() bool {
	return c.Calibration != nil && c.Calibration.Model == c.Model
}

func (c *Config) validate() error {
	if _, err := jev.BaseURL(c.Provider, ""); err != nil {
		return fmt.Errorf("provider: %w", err)
	}
	if c.Mode != Shadow && c.Mode != Enforce {
		return fmt.Errorf("mode must be shadow or enforce, got %q", c.Mode)
	}
	if c.Aggregate != verdict.Min && c.Aggregate != verdict.WeightedMean {
		return fmt.Errorf("aggregate must be min or weighted_mean, got %q", c.Aggregate)
	}
	if err := validThresholds(c.ApproveAt, c.DenyAt); err != nil {
		return err
	}
	if c.BorderlineMargin < 0 || c.BorderlineMargin >= 0.5 {
		return fmt.Errorf("borderline_margin must be in [0, 0.5), got %v", c.BorderlineMargin)
	}
	for _, g := range c.Exclude {
		if !doublestar.ValidatePattern(g) {
			return fmt.Errorf("exclude: invalid glob %q", g)
		}
	}
	if err := c.validateCalibration(); err != nil {
		return err
	}
	if err := c.validateCuts(); err != nil {
		return err
	}
	return c.validateRubric()
}

// validateCuts checks cap_at and veto_at against the approve_at in effect.
func (c *Config) validateCuts() error {
	approve, _ := c.Thresholds()
	if !(0 < c.CapAt && c.CapAt < 1 && c.CapAt <= approve) {
		return fmt.Errorf("need 0 < cap_at <= approve_at < 1, got cap_at=%v approve_at=%v", c.CapAt, approve)
	}
	if !(0 < c.VetoAt && c.VetoAt < approve) {
		return fmt.Errorf("need 0 < veto_at < approve_at < 1, got veto_at=%v approve_at=%v", c.VetoAt, approve)
	}
	for _, q := range c.Rubric {
		if q.Role == rubric.Cap && q.CapAt > approve {
			return fmt.Errorf("question %q: need cap_at <= approve_at, got cap_at=%v approve_at=%v", q.ID, q.CapAt, approve)
		}
	}
	return nil
}

func validThresholds(approve, deny float64) error {
	if !(0 < deny && deny < approve && approve < 1) {
		return fmt.Errorf("need 0 < deny_at < approve_at < 1, got deny_at=%v approve_at=%v", deny, approve)
	}
	return nil
}

func (c *Config) validateCalibration() error {
	if c.Calibration != nil {
		if err := validThresholds(c.Calibration.ApproveAt, c.Calibration.DenyAt); err != nil {
			return fmt.Errorf("calibration: %w", err)
		}
	}
	if c.Mode != Enforce || c.AllowUncalibrated {
		return nil
	}
	if c.Calibration == nil {
		return errors.New("enforce mode needs a [calibration] table from `lgtm replay --write` (or allow_uncalibrated = true)")
	}
	if !c.Calibrated() {
		return fmt.Errorf("calibration was made for %q but model is %q; re-run `lgtm replay --write`", c.Calibration.Model, c.Model)
	}
	return nil
}

func (c *Config) validateRubric() error {
	if len(c.Rubric) > maxQuestions {
		return fmt.Errorf("at most %d questions, got %d", maxQuestions, len(c.Rubric))
	}
	gates := 0
	for _, q := range c.Rubric {
		if q.Role.Gating() {
			gates++
		}
		if len(q.Instructions) > maxInstructionChars {
			return fmt.Errorf("question %q: instructions are %d characters; at most %d", q.ID, len(q.Instructions), maxInstructionChars)
		}
		if len(q.Choices) > maxChoiceOptions {
			return fmt.Errorf("question %q: at most %d options, got %d", q.ID, maxChoiceOptions, len(q.Choices))
		}
	}
	if gates == 0 {
		return errors.New("at least one gating question is required")
	}
	return nil
}

func deref[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}
