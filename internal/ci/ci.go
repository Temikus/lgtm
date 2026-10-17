// Package ci reads whether a commit's required checks failed.
package ci

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/go-github/v92/github"
)

// Failed returns the required checks that finished as failures on sha.
// Required checks are the base branch's ruleset checks plus extra, from
// config. Classic branch protection needs admin access to read, so repos
// using it name their checks in config. ignore is LGTM's own check, which
// is never required of itself.
//
// Checks still running don't count: LGTM often runs alongside CI.
func Failed(ctx context.Context, gh *github.Client, owner, repo, base, sha string, extra []string, ignore string) ([]string, error) {
	required := slices.Clone(extra)
	rules, _, err := gh.Repositories.ListRulesForBranch(ctx, owner, repo, base, &github.ListOptions{PerPage: 100})
	if err != nil {
		return nil, fmt.Errorf("reading %s's required checks: %w", base, err)
	}
	for _, r := range rules.RequiredStatusChecks {
		for _, c := range r.Parameters.RequiredStatusChecks {
			required = append(required, c.Context)
		}
	}
	required = slices.DeleteFunc(required, func(c string) bool { return c == ignore })
	if len(required) == 0 {
		return nil, nil
	}
	var failed []string
	opts := &github.ListCheckRunsOptions{Filter: new("latest"), ListOptions: github.ListOptions{PerPage: 100}}
	for run, err := range gh.Checks.ListCheckRunsForRefIter(ctx, owner, repo, sha, opts) {
		if err != nil {
			return nil, fmt.Errorf("listing check runs: %w", err)
		}
		if run.GetStatus() == "completed" && failure(run.GetConclusion()) && slices.Contains(required, run.GetName()) {
			failed = appendOnce(failed, run.GetName())
		}
	}
	// Commit statuses need the statuses permission, which a workflow may
	// not grant; check runs alone still cover GitHub Actions.
	if st, _, err := gh.Repositories.GetCombinedStatus(ctx, owner, repo, sha, &github.ListOptions{PerPage: 100}); err == nil {
		for _, s := range st.Statuses {
			if (s.GetState() == "failure" || s.GetState() == "error") && slices.Contains(required, s.GetContext()) {
				failed = appendOnce(failed, s.GetContext())
			}
		}
	}
	return failed, nil
}

// failure is a conclusion that means the check ran and didn't pass.
// Cancelled, skipped, and neutral say nothing about the code.
func failure(conclusion string) bool {
	return conclusion == "failure" || conclusion == "timed_out" || conclusion == "startup_failure"
}

func appendOnce(s []string, v string) []string {
	if slices.Contains(s, v) {
		return s
	}
	return append(s, v)
}
