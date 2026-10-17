// Command lgtm scores pull requests with System One decision models.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Set by goreleaser via -ldflags.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		var ee exitError
		if !errors.As(err, &ee) {
			fmt.Fprintln(os.Stderr, "lgtm:", err)
		}
		os.Exit(exitCode(err))
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "lgtm",
		Short:         "Score pull requests with System One decision models",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "lgtm %s (%s)\n", version, commit)
		},
	})
	root.AddCommand(newScoreCmd(), newReplayCmd(), newDataCmd(), newConfigCmd(), newActionCmd(), newInitCmd())
	return root
}
