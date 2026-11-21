package prstate

import (
	"path"
	"regexp"
	"slices"
	"strings"
)

// HashSwap is a lockfile line whose integrity hash changed while the
// package's version didn't: the same name and version would install
// different bytes. A hash algorithm upgrade looks the same and also holds.
type HashSwap struct {
	Path, Line string
}

// hashFormat finds a lockfile's hash lines and the lines that carry the
// version. With keyed set, the hash line itself names the package and
// version in its first group, and version is unused.
type hashFormat struct {
	hash    *regexp.Regexp // last group is the hash
	version *regexp.Regexp
	keyed   bool
}

var npmHash = hashFormat{
	hash:    regexp.MustCompile(`^\s*"integrity"\s*:\s*"([^"]+)"`),
	version: regexp.MustCompile(`^\s*"version"\s*:`),
}

// hashFormats covers lockfiles that keep the version within a few lines of
// the hash, so a patch's context can tie them together. Yarn Berry,
// poetry.lock, uv.lock, and Pipfile.lock keep hash lists further away and
// aren't checked. yarn.lock's resolved line doesn't count as a version: its
// fragment is a hash and changes with a swap.
var hashFormats = map[string]hashFormat{
	"package-lock.json":   npmHash,
	"npm-shrinkwrap.json": npmHash,
	"yarn.lock": {
		hash:    regexp.MustCompile(`^\s+integrity\s+(\S+)`),
		version: regexp.MustCompile(`^\s+version\s`),
	},
	"Cargo.lock": {
		hash:    regexp.MustCompile(`^\s*checksum\s*=\s*"([^"]+)"`),
		version: regexp.MustCompile(`^\s*version\s*=`),
	},
	// pnpm names the version in the entry's key line, such as /a@1.0.0:
	"pnpm-lock.yaml": {
		hash:    regexp.MustCompile(`\bintegrity:\s*([^\s,}]+)`),
		version: regexp.MustCompile(`^\s*\S+:\s*$`),
	},
	"Gemfile.lock": {
		hash:  regexp.MustCompile(`^\s+(\S+ \([^)]+\)) sha256=(\S+)`),
		keyed: true,
	},
}

// versionWindow is how many new-side lines above a hash line may hold its
// version. The furthest is Cargo.lock: version, source, checksum.
const versionWindow = 4

type hashLine struct {
	at       int // index in the hunk
	key, val string
	text     string
}

// hashSwaps returns the hash lines that changed without their version.
func hashSwaps(files []File) []HashSwap {
	var out []HashSwap
	for _, f := range files {
		hf, ok := hashFormats[path.Base(f.Path)]
		if !ok {
			continue
		}
		var hunk []string
		flush := func() {
			for _, line := range hf.swaps(hunk) {
				out = append(out, HashSwap{Path: f.Path, Line: line})
			}
			hunk = hunk[:0]
		}
		for _, line := range strings.Split(f.Patch, "\n") {
			switch {
			case strings.HasPrefix(line, "@@"):
				flush()
			case strings.HasPrefix(line, `\`): // "\ No newline at end of file"
			default:
				hunk = append(hunk, line)
			}
		}
		flush()
	}
	return out
}

// swaps checks one hunk. A swap is an added hash line in the same run of
// changed lines as a removed one with another value, with no added version
// line just above it.
func (hf hashFormat) swaps(hunk []string) []string {
	var out []string
	for start := 0; start < len(hunk); {
		if !changed(hunk[start]) {
			start++
			continue
		}
		end := start
		for end < len(hunk) && changed(hunk[end]) {
			end++
		}
		var removed, added []hashLine
		for i := start; i < end; i++ {
			m := hf.hash.FindStringSubmatch(hunk[i][1:])
			if m == nil {
				continue
			}
			h := hashLine{at: i, val: m[len(m)-1], text: strings.TrimSpace(hunk[i][1:])}
			if hf.keyed {
				h.key = m[1]
			}
			if hunk[i][0] == '-' {
				removed = append(removed, h)
			} else {
				added = append(added, h)
			}
		}
		for _, a := range added {
			// A hash that only moved within the run isn't new.
			if slices.ContainsFunc(removed, func(r hashLine) bool { return r.val == a.val }) {
				continue
			}
			if !slices.ContainsFunc(removed, func(r hashLine) bool { return r.key == a.key }) {
				continue
			}
			if !hf.keyed && hf.versionChangedAbove(hunk, a.at) {
				continue
			}
			out = append(out, a.text)
		}
		start = end
	}
	return out
}

// versionChangedAbove looks at the new side of the patch only: removed
// lines may belong to a package deleted in between, so they neither count
// towards the window nor vouch for the hash.
func (hf hashFormat) versionChangedAbove(hunk []string, at int) bool {
	for i, seen := at-1, 0; i >= 0 && seen < versionWindow; i-- {
		if strings.HasPrefix(hunk[i], "-") {
			continue
		}
		seen++
		if strings.HasPrefix(hunk[i], "+") && hf.version.MatchString(hunk[i][1:]) {
			return true
		}
	}
	return false
}

func changed(line string) bool {
	return strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-")
}
