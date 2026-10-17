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

var (
	sourceURL = regexp.MustCompile(`(?:(?:registry|sparse|git)\+)?(?:https?|ssh|git)://[^\s"'#,}]+`)
	// scpSource is git's user@host:path form, which has no scheme.
	scpSource = regexp.MustCompile(`\b[\w.-]+@([\w.-]+):[\w./~-]+`)
)

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
			for _, src := range sources(line) {
				if allowed(f.Path, src) || slices.ContainsFunc(out, func(s LockSource) bool { return s.URL == src.URL }) {
					continue
				}
				src.Path = f.Path
				out = append(out, src)
				if len(out) == maxLockSources {
					return out
				}
			}
		}
	}
	return out
}

// sources finds the remote sources on one lockfile line. Local paths have
// no scheme and aren't sources: their files are in the diff.
func sources(line string) []LockSource {
	var out []LockSource
	for _, u := range sourceURL.FindAllString(line, -1) {
		out = append(out, LockSource{URL: u, Host: hostOf(u)})
	}
	for _, m := range scpSource.FindAllStringSubmatch(line, -1) {
		out = append(out, LockSource{URL: m[0], Host: normHost(m[1])})
	}
	return out
}

// allowed is an https source on a default registry, or crates.io's own git
// index in Cargo.lock. Anything unparseable is not allowed.
func allowed(lockfile string, s LockSource) bool {
	u, err := url.Parse(stripScheme(s.URL))
	if err != nil || s.Host == "" {
		return false
	}
	if path.Base(lockfile) == "Cargo.lock" && strings.HasPrefix(s.URL, "registry+") && u.Scheme == "https" &&
		s.Host == "github.com" && strings.TrimSuffix(u.Path, ".git") == "/rust-lang/crates.io-index" && u.RawQuery == "" {
		return true
	}
	return u.Scheme == "https" && slices.Contains(DefaultRegistries, s.Host)
}

func stripScheme(u string) string {
	for _, p := range []string{"registry+", "sparse+", "git+"} {
		u = strings.TrimPrefix(u, p)
	}
	return u
}

func hostOf(u string) string {
	parsed, err := url.Parse(stripScheme(u))
	if err != nil {
		return ""
	}
	return normHost(parsed.Hostname())
}

func normHost(h string) string { return strings.TrimSuffix(strings.ToLower(h), ".") }
