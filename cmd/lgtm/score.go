package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Temikus/lgtm/internal/config"
	"github.com/Temikus/lgtm/internal/engine"
	"github.com/Temikus/lgtm/internal/ghout"
	"github.com/Temikus/lgtm/internal/prstate"
	"github.com/Temikus/lgtm/internal/report"
)

func newScoreCmd() *cobra.Command {
	var (
		asJSON  bool
		post    bool
		cfgPath string
	)
	cmd := &cobra.Command{
		Use:   "score <owner/repo#N | PR URL>",
		Short: "Score a pull request and print the result",
		Long: "Score a pull request from your machine. Config comes from the local checkout\n" +
			"(" + config.Path + " by default). With --post, the sticky comment is posted\n" +
			"as you; check runs need a GitHub App token, so only the Action creates them.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			owner, repo, n, err := parsePRRef(args[0])
			if err != nil {
				return err
			}
			cfg, err := config.LoadFile(cfgPath)
			if err != nil {
				return err
			}
			d, err := decider(cfg, "")
			if err != nil {
				return err
			}
			gh, err := githubClient(ctx)
			if err != nil {
				return err
			}
			p, _, err := gh.PullRequests.Get(ctx, owner, repo, n)
			if err != nil {
				return fmt.Errorf("reading %s/%s#%d: %w", owner, repo, n, err)
			}

			pr := prstate.FromPullRequest(owner, repo, p)
			r := engine.Score(ctx, gh, pr, engine.Options{Config: cfg, Decider: d})
			if post {
				me, _, err := gh.Users.Get(ctx, "")
				if err != nil {
					return fmt.Errorf("reading your GitHub login: %w", err)
				}
				pub := ghout.Publisher{GH: gh, PR: pr, CommentAuthor: me.GetLogin()}
				if err := pub.Comment(ctx, report.Comment(r)); err != nil {
					return err
				}
			}
			// Only the comment is public; the terminal gets every value.
			r.ShowScores, r.Blind = true, false
			if asJSON {
				b, err := report.JSON(r)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), report.Text(r))
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&post, "post", false, "post or update the sticky comment on the PR")
	cmd.Flags().StringVar(&cfgPath, "config", config.Path, "config file")
	return cmd
}

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Work with " + config.Path}
	cmd.AddCommand(&cobra.Command{
		Use:   "validate [path]",
		Short: "Check a config file and print the rubric it gives",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := config.Path
			if len(args) == 1 {
				path = args[0]
			}
			cfg, err := config.LoadFile(path)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			approve, deny := cfg.Thresholds()
			calib := "uncalibrated"
			if cfg.Calibrated() {
				calib = fmt.Sprintf("calibrated %s on %d PRs", cfg.Calibration.Date, cfg.Calibration.NLabelled)
			}
			_, _ = fmt.Fprintf(out, "%s: ok\nprovider %s · model %s · mode %s · %s\napprove >= %.2f · deny <= %.2f · aggregate %s\n\n",
				path, cfg.Provider, cfg.Model, cfg.Mode, calib, approve, deny, cfg.Aggregate)
			for _, q := range cfg.Rubric {
				_, _ = fmt.Fprintf(out, "  %-16s %-6s %-5s %s\n", q.ID, q.Type, q.Role, q.Context)
			}
			return nil
		},
	})
	return cmd
}
