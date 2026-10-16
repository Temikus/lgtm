// Package trust decides whose PRs and reviews count. It uses the
// collaborator permission API, never author_association: CONTRIBUTOR only
// means a merged commit, and COLLABORATOR includes read-only invitees.
package trust

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/go-github/v92/github"

	"github.com/Temikus/lgtm/internal/prstate"
)

// trustedRoles have write access or more. role_name distinguishes maintain
// and triage, which the legacy permission field folds into write and read.
var trustedRoles = map[string]bool{"admin": true, "maintain": true, "write": true}

// AuthorTrusted reports whether user has write, maintain, or admin on the
// repo. Not being a collaborator is untrusted, not an error. Any other
// failure is untrusted and returned.
func AuthorTrusted(ctx context.Context, gh *github.Client, owner, repo, user string) (bool, error) {
	p, resp, err := gh.Repositories.GetPermissionLevel(ctx, owner, repo, user)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking %s's permission: %w", user, err)
	}
	if role := p.GetRoleName(); role != "" {
		return trustedRoles[role], nil
	}
	return trustedRoles[p.GetPermission()], nil
}

// HumanApproval reports whether someone with write or more, other than the
// author, approved head.sha and hasn't since dismissed or requested changes.
func HumanApproval(ctx context.Context, gh *github.Client, pr prstate.PR) (bool, string, error) {
	latest := map[string]*github.PullRequestReview{}
	var order []string
	for r, err := range gh.PullRequests.ListReviewsIter(ctx, pr.Owner, pr.Repo, pr.Number, &github.ListOptions{PerPage: 100}) {
		if err != nil {
			return false, "", fmt.Errorf("listing reviews: %w", err)
		}
		// A comment doesn't change a reviewer's standing.
		if r.GetState() == "COMMENTED" || r.GetState() == "PENDING" {
			continue
		}
		login := r.GetUser().GetLogin()
		if _, seen := latest[login]; !seen {
			order = append(order, login)
		}
		latest[login] = r
	}
	for _, login := range order {
		r := latest[login]
		if r.GetState() != "APPROVED" || r.GetCommitID() != pr.HeadSHA || login == pr.Author {
			continue
		}
		ok, err := AuthorTrusted(ctx, gh, pr.Owner, pr.Repo, login)
		if err != nil {
			return false, "", err
		}
		if ok {
			return true, login, nil
		}
	}
	return false, "", nil
}
