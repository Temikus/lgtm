package prstate

import (
	"slices"
	"strings"
	"testing"
)

func patch(lines ...string) string { return "@@ -1,9 +1,9 @@\n" + strings.Join(lines, "\n") + "\n" }

func TestHashSwaps(t *testing.T) {
	for _, tt := range []struct {
		name, path, patch string
		want              []string
	}{
		{"npm hash only", "package-lock.json", patch(
			`     "node_modules/left-pad": {`,
			`       "version": "1.3.0",`,
			`       "resolved": "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz",`,
			`-      "integrity": "sha512-AAAA",`,
			`+      "integrity": "sha512-BBBB",`,
		), []string{`"integrity": "sha512-BBBB",`}},
		{"npm bump", "web/package-lock.json", patch(
			`     "node_modules/left-pad": {`,
			`-      "version": "1.3.0",`,
			`-      "resolved": "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz",`,
			`-      "integrity": "sha512-AAAA",`,
			`+      "version": "1.3.1",`,
			`+      "resolved": "https://registry.npmjs.org/left-pad/-/left-pad-1.3.1.tgz",`,
			`+      "integrity": "sha512-BBBB",`,
		), nil},
		{"npm new and removed packages", "package-lock.json", patch(
			`+    "node_modules/a": {`,
			`+      "version": "1.0.0",`,
			`+      "integrity": "sha512-AAAA"`,
			`+    },`,
			`     "node_modules/b": {`,
			`-    "node_modules/c": {`,
			`-      "version": "2.0.0",`,
			`-      "integrity": "sha512-CCCC"`,
			`-    },`,
		), nil},
		{"npm one bumped, one swapped", "package-lock.json", patch(
			`     "node_modules/a": {`,
			`-      "version": "1.0.0",`,
			`-      "integrity": "sha512-A1",`,
			`+      "version": "1.0.1",`,
			`+      "integrity": "sha512-A2",`,
			`       "license": "MIT"`,
			`     },`,
			`     "node_modules/b": {`,
			`       "version": "2.0.0",`,
			`-      "integrity": "sha512-B1",`,
			`+      "integrity": "sha512-B2",`,
		), []string{`"integrity": "sha512-B2",`}},
		{"yarn swap also changes the resolved fragment", "yarn.lock", patch(
			` left-pad@^1.3.0:`,
			`   version "1.3.0"`,
			`-  resolved "https://registry.yarnpkg.com/left-pad/-/left-pad-1.3.0.tgz#aaaa"`,
			`-  integrity sha512-AAAA`,
			`+  resolved "https://registry.yarnpkg.com/left-pad/-/left-pad-1.3.0.tgz#bbbb"`,
			`+  integrity sha512-BBBB`,
		), []string{`integrity sha512-BBBB`}},
		{"yarn bump", "yarn.lock", patch(
			` left-pad@^1.3.0:`,
			`-  version "1.3.0"`,
			`-  resolved "https://registry.yarnpkg.com/left-pad/-/left-pad-1.3.0.tgz#aaaa"`,
			`-  integrity sha512-AAAA`,
			`+  version "1.3.1"`,
			`+  resolved "https://registry.yarnpkg.com/left-pad/-/left-pad-1.3.1.tgz#bbbb"`,
			`+  integrity sha512-BBBB`,
		), nil},
		{"cargo hash only", "Cargo.lock", patch(
			` name = "serde"`,
			` version = "1.0.200"`,
			` source = "registry+https://github.com/rust-lang/crates.io-index"`,
			`-checksum = "aaaa"`,
			`+checksum = "bbbb"`,
		), []string{`checksum = "bbbb"`}},
		{"cargo bump", "Cargo.lock", patch(
			` name = "serde"`,
			`-version = "1.0.200"`,
			`+version = "1.0.201"`,
			` source = "registry+https://github.com/rust-lang/crates.io-index"`,
			`-checksum = "aaaa"`,
			`+checksum = "bbbb"`,
		), nil},
		// From nodejs/node#65161: a package deleted between version and checksum.
		{"cargo bump with a package removed in between", "Cargo.lock", patch(
			` name = "serde"`,
			`-version = "1.0.219"`,
			`+version = "1.0.164"`,
			` source = "registry+https://github.com/rust-lang/crates.io-index"`,
			`-checksum = "aaaa"`,
			`-dependencies = [`,
			`- "serde_derive",`,
			`-]`,
			`-`,
			`-[[package]]`,
			`-name = "serde_derive"`,
			`-version = "1.0.219"`,
			`-source = "registry+https://github.com/rust-lang/crates.io-index"`,
			`-checksum = "cccc"`,
			`+checksum = "bbbb"`,
		), nil},
		// A removed version line doesn't vouch for an added hash.
		{"cargo swap below a removed package", "Cargo.lock", patch(
			`-[[package]]`,
			`-name = "gone"`,
			`-version = "0.1.0"`,
			`-checksum = "cccc"`,
			`-`,
			` [[package]]`,
			` name = "serde"`,
			` version = "1.0.200"`,
			`-checksum = "aaaa"`,
			`+checksum = "bbbb"`,
		), []string{`checksum = "bbbb"`}},
		{"cargo swap behind a rewritten version line", "Cargo.lock", patch(
			` name = "serde"`,
			`-version = "1.0.200"`,
			`+version =  '1.0.200'`,
			` source = "registry+https://github.com/rust-lang/crates.io-index"`,
			`-checksum = "aaaa"`,
			`+checksum = "bbbb"`,
		), []string{`checksum = "bbbb"`}},
		{"npm swap behind a rewritten version line", "package-lock.json", patch(
			`-      "version": "1.3.0",`,
			`-      "integrity": "sha512-AAAA",`,
			`+      "version":  "1.3.0",`,
			`+      "integrity": "sha512-BBBB",`,
		), []string{`"integrity": "sha512-BBBB",`}},
		// A neighbour removed at the version this package moves to is no rewrite.
		{"cargo bump to a removed neighbour's version", "Cargo.lock", patch(
			`-[[package]]`,
			`-name = "gone"`,
			`-version = "1.0.201"`,
			`-checksum = "cccc"`,
			`-`,
			` [[package]]`,
			` name = "serde"`,
			`-version = "1.0.200"`,
			`+version = "1.0.201"`,
			` source = "registry+https://github.com/rust-lang/crates.io-index"`,
			`-checksum = "aaaa"`,
			`+checksum = "bbbb"`,
		), nil},
		{"pnpm hash only", "pnpm-lock.yaml", patch(
			`   /left-pad@1.3.0:`,
			`-    resolution: {integrity: sha512-AAAA}`,
			`+    resolution: {integrity: sha512-BBBB}`,
		), []string{`resolution: {integrity: sha512-BBBB}`}},
		{"pnpm bump", "pnpm-lock.yaml", patch(
			`-  /left-pad@1.3.0:`,
			`-    resolution: {integrity: sha512-AAAA}`,
			`+  /left-pad@1.3.1:`,
			`+    resolution: {integrity: sha512-BBBB}`,
		), nil},
		{"gem hash only, next to a bump", "Gemfile.lock", patch(
			` CHECKSUMS`,
			`-  rack (3.0.8) sha256=aaaa`,
			`-  rake (13.0.6) sha256=cccc`,
			`+  rack (3.0.8) sha256=bbbb`,
			`+  rake (13.1.0) sha256=dddd`,
		), []string{`rack (3.0.8) sha256=bbbb`}},
		{"moved, not changed", "package-lock.json", patch(
			`-      "integrity": "sha512-AAAA",`,
			`+      "integrity": "sha512-AAAA",`,
		), nil},
		{"not a checked lockfile", "poetry.lock", patch(
			`-checksum = "aaaa"`,
			`+checksum = "bbbb"`,
		), nil},
	} {
		var got []string
		for _, s := range hashSwaps([]File{{Path: tt.path, Patch: tt.patch}}) {
			got = append(got, s.Line)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s: hashSwaps = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// A version change in an earlier hunk can't vouch for a hash in a later one.
func TestHashSwaps_HunksAreSeparate(t *testing.T) {
	p := "@@ -1,2 +1,2 @@\n-      \"version\": \"1.0.0\",\n+      \"version\": \"1.0.1\",\n" +
		"@@ -40,2 +40,2 @@\n-      \"integrity\": \"sha512-AAAA\",\n+      \"integrity\": \"sha512-BBBB\",\n"
	if got := hashSwaps([]File{{Path: "package-lock.json", Patch: p}}); len(got) != 1 {
		t.Errorf("hashSwaps = %+v, want the second hunk's hash", got)
	}
}
