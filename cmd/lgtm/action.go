package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Temikus/lgtm/internal/action"
	"github.com/Temikus/lgtm/internal/config"
	"github.com/Temikus/lgtm/internal/ghout"
	"github.com/Temikus/lgtm/internal/jev"
)

// exitError carries a job exit code out of cobra.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

func newActionCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "action",
		Short:  "GitHub Action entrypoint (reads GITHUB_EVENT_PATH)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			ev, err := os.ReadFile(os.Getenv("GITHUB_EVENT_PATH")) // #nosec G703 -- path set by the runner
			if err != nil {
				return fmt.Errorf("reading event: %w", err)
			}
			gh, err := githubClient(ctx)
			if err != nil {
				return err
			}
			author := os.Getenv("LGTM_COMMENT_AUTHOR")
			if author == "" {
				author = ghout.ActionsBot
			}
			env := action.Env{
				EventName: os.Getenv("GITHUB_EVENT_NAME"),
				Event:     ev,
				GH:        gh,
				NewDecider: func(cfg *config.Config) (jev.Decider, error) {
					d, err := decider(cfg, os.Getenv("INPUT_BASE-URL"))
					if err != nil {
						return nil, err
					}
					return debugDecider(d), nil
				},
				CommentAuthor: author,
				Log:           cmd.OutOrStdout(),
				SetOutput:     setOutput,
			}
			if f := appendFile(os.Getenv("GITHUB_STEP_SUMMARY")); f != nil {
				defer func() { _ = f.Close() }()
				env.StepSummary = f
			}
			code, err := action.Run(ctx, env)
			if err != nil {
				return err
			}
			if code != 0 {
				return exitError{code}
			}
			return nil
		},
	}
}

func appendFile(path string) *os.File {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0) // #nosec G304 G703 -- path set by the runner
	if err != nil {
		return nil
	}
	return f
}

func setOutput(name, value string) {
	f := appendFile(os.Getenv("GITHUB_OUTPUT"))
	if f == nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "%s=%s\n", name, value)
}

// debugDecider logs request bodies when LGTM_DEBUG=1. Off by default: on a
// public repo the Action log is public.
func debugDecider(d jev.Decider) jev.Decider {
	if os.Getenv("LGTM_DEBUG") != "1" {
		return d
	}
	return debugging{d}
}

type debugging struct{ jev.Decider }

func (d debugging) Decide(ctx context.Context, req jev.Request) (*jev.Response, error) {
	b, _ := json.MarshalIndent(req.State, "", "  ")
	fmt.Fprintf(os.Stderr, "lgtm debug: request with %d questions, state:\n%s\n", len(req.Questions), b)
	return d.Decider.Decide(ctx, req)
}

//go:embed templates
var templates embed.FS

func newInitCmd() *cobra.Command {
	var (
		force bool
		ref   string
		dir   string
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write the LGTM workflows and a commented " + config.Path,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			files := map[string]string{
				"templates/lgtm.yml":        ".github/workflows/lgtm.yml",
				"templates/lgtm-review.yml": ".github/workflows/lgtm-review.yml",
				"templates/lgtm.toml":       config.Path,
			}
			for src, dst := range files {
				if err := writeTemplate(src, filepath.Join(dir, dst), ref, force); err != nil {
					return err
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "wrote", dst)
			}
			_, _ = fmt.Fprint(cmd.OutOrStdout(), `
Next:
  1. Create a dedicated OpenRouter key with a spend limit.
  2. gh secret set OPENROUTER_API_KEY
  3. Commit and push these files to the default branch.
`)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing files")
	cmd.Flags().StringVar(&ref, "ref", "v0", "Action version to use")
	cmd.Flags().StringVar(&dir, "dir", ".", "repository root")
	return cmd
}

func writeTemplate(src, dst, ref string, force bool) error {
	if _, err := os.Stat(dst); err == nil && !force {
		return fmt.Errorf("%s exists (use --force to overwrite)", dst)
	}
	raw, err := templates.ReadFile(src)
	if err != nil {
		return err
	}
	out := strings.ReplaceAll(string(raw), "{{.Ref}}", ref)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	return os.WriteFile(dst, []byte(out), 0o600)
}

// exitCode maps an error from Execute to a process exit code.
func exitCode(err error) int {
	var ee exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return 1
}
