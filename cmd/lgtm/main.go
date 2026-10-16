// Command lgtm scores pull requests with System One decision models.
package main

import (
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
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "lgtm",
		Short:         "Score pull requests with System One decision models",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "lgtm %s (%s)\n", version, commit)
		},
	})
	root.AddCommand(newScoreCmd(), newConfigCmd())
	return root
}
