package prstate

import (
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
)

// LockSource is a package source a lockfile change adds that isn't one of
// the usual registries. Lockfiles are excluded from the model by default,
// so this check is the only thing that reads them.
type LockSource struct {
	Path, URL, Host string
}

// lockKeys are, per lockfile name, the keys that say where a package comes
// from. Other URLs in lockfiles, such as npm's funding links, don't count.
// composer.lock is left out: its dist URLs point at GitHub for every
// package.
var lockKeys = map[string]*regexp.Regexp{
	"package-lock.json":   regexp.MustCompile(`^\s*"resolved"\s*:`),
	"npm-shrinkwrap.json": regexp.MustCompile(`^\s*"resolved"\s*:`),
	"yarn.lock":           regexp.MustCompile(`^\s*resolved\s`),
	"pnpm-lock.yaml":      regexp.MustCompile(`\btarball\s*:`),
	"Cargo.lock":          regexp.MustCompile(`^\s*source\s*=`),
	"poetry.lock":         regexp.MustCompile(`^\s*url\s*=`),
	"uv.lock":             regexp.MustCompile(`^\s*source\s*=`),
	"Gemfile.lock":        regexp.MustCompile(`^\s*remote\s*:`),
	"Pipfile.lock":        regexp.MustCompile(`^\s*"url"\s*:`),
	"pubspec.lock":        regexp.MustCompile(`^\s*url\s*:`),
}

// DefaultRegistries are the hosts a lockfile may resolve packages from
// without holding the PR. Config adds to them.
var DefaultRegistries = []string{
	"registry.npmjs.org", "registry.yarnpkg.com", "pypi.org", "files.pythonhosted.org",
	"index.crates.io", "static.crates.io", "rubygems.org", "pub.dev", "pub.dartlang.org",
}

// crates.io's git index is a registry, not a git dependency.
const cratesIndex = "github.com/rust-lang/crates.io-index"

var sourceURL = regexp.MustCompile(`(?:(?:registry|sparse|git)\+)?(?:https?|ssh|git)://[^\s"'#,}]+`)

// maxLockSources bounds how many sources a report lists.
const maxLockSources = 10

// lockSources returns the sources that added lines in lockfile patches
// point at, other than DefaultRegistries.
func lockSources(files []File) []LockSource {
	var out []LockSource
	for _, f := range files {
		key, ok := lockKeys[path.Base(f.Path)]
		if !ok {
			continue
		}
		for _, line := range strings.Split(f.Patch, "\n") {
			if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") || !key.MatchString(line[1:]) {
				continue
			}
			for _, u := range sourceURL.FindAllString(line, -1) {
				if strings.Contains(u, cratesIndex) {
					continue
				}
				host := hostOf(u)
				if slices.Contains(DefaultRegistries, host) || slices.ContainsFunc(out, func(s LockSource) bool { return s.URL == u }) {
					continue
				}
				out = append(out, LockSource{Path: f.Path, URL: u, Host: host})
				if len(out) == maxLockSources {
					return out
				}
			}
		}
	}
	return out
}

func hostOf(u string) string {
	for _, p := range []string{"registry+", "sparse+", "git+"} {
		u = strings.TrimPrefix(u, p)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}
