package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/Temikus/lgtm/internal/dataset"
	"github.com/Temikus/lgtm/internal/replay"
)

// dataOpts are the dataset flags shared by data and replay.
type dataOpts struct {
	dir    string
	noData bool
}

func (o *dataOpts) flags(f *pflag.FlagSet) {
	f.StringVar(&o.dir, "data", "", "local dataset directory (default: lgtm/data in your cache directory)")
	f.BoolVar(&o.noData, "no-data", false, "read everything from GitHub and keep nothing")
}

// open returns nil when the dataset is off.
func (o dataOpts) open() (*dataset.Store, error) {
	if o.noData {
		return nil, nil
	}
	dir := o.dir
	if dir == "" {
		var err error
		if dir, err = dataset.DefaultDir(); err != nil {
			return nil, err
		}
	}
	return dataset.Open(dir)
}

func newDataCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "data",
		Short: "Manage the local dataset that replay reads",
		Long: "The dataset keeps PRs, labels, diffs, and model replies in Parquet files,\n" +
			"so replay doesn't read them again. Query it with DuckDB or pandas, for\n" +
			"example: select * from read_parquet('<dir>/scores/*.parquet').",
	}
	cmd.AddCommand(newDataPullCmd(), newDataDirCmd())
	return cmd
}

func newDataDirCmd() *cobra.Command {
	var d dataOpts
	cmd := &cobra.Command{
		Use:   "dir",
		Short: "Print the dataset directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := d.open()
			if err != nil || st == nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), st.Dir())
			return err
		},
	}
	d.flags(cmd.Flags())
	return cmd
}

type pullOpts struct {
	data                       dataOpts
	since, labels, list, label string
	maxPRs                     int
}

func newDataPullCmd() *cobra.Command {
	var o pullOpts
	cmd := &cobra.Command{
		Use:   "pull [<owner/repo>]",
		Short: "Store a repo's labelled history, or a list of PRs, without calling the model",
		Long: "pull reads closed PRs the way replay does and stores their labels, diffs, and\n" +
			"author checks, so `lgtm replay --offline` needs no GitHub calls. PRs already\n" +
			"stored are not read again.\n\n" +
			"With --list, it stores the PRs in a list instead (lines of\n" +
			"\"<kind> <owner/repo#N>\"), labelled --label: negative for known-bad PRs,\n" +
			"positive for known-good ones. `lgtm replay --lists` scores them.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return o.run(cmd, args) },
	}
	f := cmd.Flags()
	f.StringVar(&o.since, "since", "90d", "how far back to read closed PRs")
	f.IntVar(&o.maxPRs, "max-prs", 300, "most PRs to read")
	f.StringVar(&o.labels, "labels", string(replay.Strict), "strict, maintainer, or merged (see lgtm replay --help)")
	f.StringVar(&o.list, "list", "", "list of PRs to store instead of a repo's history")
	f.StringVar(&o.label, "label", string(replay.Negative), "label for --list PRs: negative (known-bad) or positive (known-good)")
	o.data.flags(f)
	return cmd
}

func (o pullOpts) run(cmd *cobra.Command, args []string) error {
	if (o.list == "") == (len(args) == 0) {
		return fmt.Errorf("give either <owner/repo> or --list")
	}
	st, err := o.data.open()
	if err != nil {
		return err
	}
	if st == nil {
		return fmt.Errorf("pull stores into the dataset, so --no-data makes no sense here")
	}
	gh, err := githubClient(cmd.Context())
	if err != nil {
		return err
	}
	data := replay.Data{Store: st}
	var stats replay.PullStats
	if o.list != "" {
		stats, err = o.pullList(cmd, gh, data)
	} else {
		stats, err = o.pullRepo(cmd, gh, data, args[0])
	}
	// Keep what was read even when the pull failed partway.
	if err = errors.Join(err, st.Flush()); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%d PRs, %d labelled, %d diffs stored (%d already there), %d failed · %s\n",
		stats.PRs, stats.Labelled, stats.Diffs, stats.Cached, stats.Failed, st.Dir())
	return err
}

func (o pullOpts) pullList(cmd *cobra.Command, gh *github.Client, data replay.Data) (replay.PullStats, error) {
	f, err := os.Open(o.list) // #nosec G304 -- the user's own list
	if err != nil {
		return replay.PullStats{}, err
	}
	entries, err := replay.ParseList(f)
	_ = f.Close()
	if err != nil {
		return replay.PullStats{}, fmt.Errorf("%s: %w", o.list, err)
	}
	return replay.PullList(cmd.Context(), gh, data, entries, replay.Label(o.label))
}

func (o pullOpts) pullRepo(cmd *cobra.Command, gh *github.Client, data replay.Data, ref string) (replay.PullStats, error) {
	owner, repo, ok := strings.Cut(ref, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return replay.PullStats{}, fmt.Errorf("%q: want owner/repo", ref)
	}
	age, err := parseAge(o.since)
	if err != nil {
		return replay.PullStats{}, err
	}
	mode, err := parseLabels(o.labels)
	if err != nil {
		return replay.PullStats{}, err
	}
	hist := replay.HistoryOptions{Since: time.Now().Add(-age), MaxPRs: o.maxPRs, Mode: mode, Data: data}
	return replay.Pull(cmd.Context(), gh, owner, repo, hist)
}
