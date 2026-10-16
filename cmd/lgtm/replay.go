package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Temikus/lgtm/internal/config"
	"github.com/Temikus/lgtm/internal/replay"
)

type replayOpts struct {
	since, labels, cfgPath string
	maxPRs, sample         int
	budget, target         float64
	write, perturb, dryRun bool
}

func newReplayCmd() *cobra.Command {
	var o replayOpts
	cmd := &cobra.Command{
		Use:   "replay <owner/repo>",
		Short: "Score past PRs, compare with human decisions, and fit thresholds",
		Long: "Replay labels closed PRs from their reviews (or, with --labels merged, from\n" +
			"merges, for repos with one maintainer), scores the labelled ones, and fits\n" +
			"approve_at and deny_at. With --write it stores them in [calibration] in\n" +
			"--config, which enforce mode requires.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return o.run(cmd, args[0]) },
	}
	f := cmd.Flags()
	f.StringVar(&o.since, "since", "90d", "how far back to read closed PRs (e.g. 90d, 720h)")
	f.IntVar(&o.maxPRs, "max-prs", 300, "most PRs to read")
	f.Float64Var(&o.budget, "budget-usd", 2, "stop calling the model before spend passes this")
	f.Float64Var(&o.target, "target-precision", 0.95, "precision approve and deny must reach")
	f.StringVar(&o.labels, "labels", string(replay.Strict), "strict (reviews) or merged (merges, for one-maintainer repos)")
	f.BoolVar(&o.write, "write", false, "store the fitted thresholds in [calibration]")
	f.BoolVar(&o.perturb, "perturb", false, "run the robustness suite on PRs near approve_at")
	f.IntVar(&o.sample, "perturb-sample", 10, "PRs to perturb")
	f.BoolVar(&o.dryRun, "dry-run", false, "label history and stop, without calling the model")
	f.StringVar(&o.cfgPath, "config", config.Path, "config file to read and, with --write, update")
	return cmd
}

func (o replayOpts) run(cmd *cobra.Command, ref string) error {
	ctx := cmd.Context()
	owner, repo, ok := strings.Cut(ref, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return fmt.Errorf("%q: want owner/repo", ref)
	}
	age, mode, err := replayFlags(o.since, o.labels, o.target)
	if err != nil {
		return err
	}
	cfg, err := config.LoadFile(o.cfgPath)
	if err != nil {
		return err
	}
	gh, err := githubClient(ctx)
	if err != nil {
		return err
	}
	hist := replay.HistoryOptions{Since: time.Now().Add(-age), MaxPRs: o.maxPRs, Mode: mode}
	if o.dryRun {
		samples, err := replay.History(ctx, gh, owner, repo, hist)
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(cmd.OutOrStdout(), replay.LabelText(owner+"/"+repo, mode, samples))
		return err
	}
	d, err := decider(cfg, "")
	if err != nil {
		return err
	}

	rep, err := replay.Run(ctx, gh, replay.Options{
		Owner: owner, Repo: repo, Config: cfg, Decider: d,
		BudgetUSD: o.budget, TargetPrecision: o.target, History: hist,
		Perturb: o.perturb, PerturbSample: o.sample,
		Progress: cmd.ErrOrStderr(),
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprint(cmd.OutOrStdout(), rep.Text(o.target)); err != nil {
		return err
	}
	return o.save(cmd, rep)
}

// save stores the calibration when --write asks for it.
func (o replayOpts) save(cmd *cobra.Command, rep *replay.Report) error {
	if !o.write {
		return nil
	}
	if rep.Calibration == nil {
		return fmt.Errorf("not writing a calibration: %s", rep.WhyNot)
	}
	if err := writeCalibration(o.cfgPath, *rep.Calibration); err != nil {
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "\nWrote [calibration] to %s.\n", o.cfgPath)
	return err
}

func replayFlags(since, labels string, target float64) (time.Duration, replay.LabelMode, error) {
	age, err := parseAge(since)
	if err != nil {
		return 0, "", err
	}
	mode := replay.LabelMode(labels)
	if mode != replay.Strict && mode != replay.Merged {
		return 0, "", fmt.Errorf("--labels must be strict or merged, got %q", labels)
	}
	if target <= 0 || target >= 1 {
		return 0, "", fmt.Errorf("--target-precision must be between 0 and 1, got %v", target)
	}
	return age, mode, nil
}

var dayAge = regexp.MustCompile(`^(\d+)d$`)

// parseAge accepts a number of days ("90d") or a Go duration ("720h").
func parseAge(s string) (time.Duration, error) {
	if m := dayAge.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("--since %q: want days like 90d or a duration like 720h", s)
	}
	return d, nil
}

// writeCalibration updates path in place and keeps its permissions. A missing
// file is created.
func writeCalibration(path string, c config.Calibration) error {
	src, err := os.ReadFile(path) // #nosec G304 -- the user's own config path
	mode := os.FileMode(0o644)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm()
		}
	}
	out, err := replay.SetCalibration(src, c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, mode) // #nosec G306 G703 -- the user's own config path; keeps its mode
}
