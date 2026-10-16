package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/Temikus/lgtm/internal/config"
	"github.com/Temikus/lgtm/internal/jev"
)

// githubClient authenticates with GITHUB_TOKEN, falling back to `gh auth token`.
func githubClient(ctx context.Context) (*github.Client, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
		if err != nil {
			return nil, errors.New("no GitHub token: set GITHUB_TOKEN or run `gh auth login`")
		}
		token = strings.TrimSpace(string(out))
	}
	return github.NewClient(github.WithAuthToken(token), github.WithTimeout(30*time.Second), github.WithUserAgent("lgtm/"+version))
}

// apiKeyVars lists where each provider's key may come from, in order.
var apiKeyVars = map[string][]string{
	"openrouter": {"LGTM_API_KEY", "OPENROUTER_API_KEY"},
	"typesafe":   {"LGTM_API_KEY", "TYPESAFE_API_KEY"},
}

// decider builds the model client. A custom base URL comes only from
// LGTM_BASE_URL, never from repo config.
func decider(cfg *config.Config, baseOverride string) (*jev.Client, error) {
	if baseOverride == "" {
		baseOverride = os.Getenv("LGTM_BASE_URL")
	}
	base, err := jev.BaseURL(cfg.Provider, baseOverride)
	if err != nil {
		return nil, err
	}
	for _, v := range apiKeyVars[cfg.Provider] {
		if key := os.Getenv(v); key != "" {
			return jev.NewClient(base, key), nil
		}
	}
	return nil, fmt.Errorf("no API key for %s: set %s", cfg.Provider, strings.Join(apiKeyVars[cfg.Provider], " or "))
}

var shortRef = regexp.MustCompile(`^([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)#([0-9]+)$`)

// parsePRRef accepts owner/repo#N or https://github.com/owner/repo/pull/N.
func parsePRRef(ref string) (owner, repo string, number int, err error) {
	if m := shortRef.FindStringSubmatch(ref); m != nil {
		n, err := strconv.Atoi(m[3])
		return m[1], m[2], n, err
	}
	u, err := url.Parse(ref)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" {
		return "", "", 0, fmt.Errorf("%q: want owner/repo#N or https://github.com/owner/repo/pull/N", ref)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return "", "", 0, fmt.Errorf("%q is not a pull request URL", ref)
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return "", "", 0, fmt.Errorf("%q: bad PR number", ref)
	}
	return parts[0], parts[1], n, nil
}
