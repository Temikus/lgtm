package prstate

import (
	"slices"
	"testing"
)

func TestLockSources(t *testing.T) {
	files := []File{
		{Path: "web/package-lock.json", Patch: "@@ -1,4 +1,6 @@\n" +
			`+      "resolved": "https://registry.npmjs.org/a/-/a-1.0.0.tgz",` + "\n" +
			`+      "resolved": "https://203.0.113.9/left-pad-1.3.0.tgz",` + "\n" +
			`+        "url": "https://opencollective.com/x"` + "\n" + // funding, not a source
			`-      "resolved": "https://old.example/b.tgz",` + "\n"}, // removed lines don't count
		{Path: "Cargo.lock", Patch: "@@ -1 +1,3 @@\n" +
			`+source = "registry+https://github.com/rust-lang/crates.io-index"` + "\n" +
			`+source = "git+https://github.com/someone/fork?rev=abc#abc"` + "\n"},
		{Path: "src/app.js", Patch: `+fetch("https://203.0.113.9/x")` + "\n"}, // not a lockfile
	}
	got := lockSources(files)
	var hosts []string
	for _, s := range got {
		hosts = append(hosts, s.Path+" "+s.Host)
	}
	want := []string{"web/package-lock.json 203.0.113.9", "Cargo.lock github.com"}
	if !slices.Equal(hosts, want) {
		t.Errorf("lockSources = %v, want %v", hosts, want)
	}
}

// Lookalikes of an allowed source still hold.
func TestLockSources_NoLookalikes(t *testing.T) {
	for _, line := range []string{
		`+source = "registry+https://evil.example/github.com/rust-lang/crates.io-index"`,
		`+source = "registry+https://github.com/rust-lang/crates.io-index?x=1"`,
		`+source = "git+https://github.com/rust-lang/crates.io-index"`,
		`+source = "git+ssh://git@github.com/attacker/crate"`,
		`+source = "git@github.com:attacker/crate.git"`,
		`+source = "registry+http://index.crates.io/"`,
	} {
		if got := lockSources([]File{{Path: "Cargo.lock", Patch: line}}); len(got) == 0 {
			t.Errorf("%s: not held", line)
		}
	}
	for _, line := range []string{
		`+source = "registry+https://github.com/rust-lang/crates.io-index"`,
		`+source = "sparse+https://Index.Crates.IO./"`,
		`+source = { editable = "." }`,
	} {
		if got := lockSources([]File{{Path: "Cargo.lock", Patch: line}}); len(got) != 0 {
			t.Errorf("%s: held as %+v", line, got)
		}
	}
}

// Source forms a first version missed (code review, 2026-10-17).
func TestLockSources_OtherForms(t *testing.T) {
	for _, tt := range []struct{ path, line string }{
		{"uv.lock", `+sdist = { url = "https://evil.example/pkg-1.0.tar.gz", hash = "sha256:00" }`},
		{"uv.lock", `+wheels = [{ url = "https://evil.example/pkg-1.0-py3-none-any.whl" }]`},
		{"yarn.lock", `+  resolution: "left-pad@https://evil.example/left-pad.tgz"`},
		{"pnpm-lock.yaml", `+    resolution: {commit: abc, repo: https://evil.example/x.git, type: git}`},
		{"Pipfile.lock", `+            "git": "https://evil.example/x.git",`},
		{"package-lock.json", `+      "resolved": "https:\/\/evil.example\/x.tgz",`},
		{"package-lock.json", `+      "resolved": "file:../vendored/x.tgz",`},
	} {
		if got := lockSources([]File{{Path: tt.path, Patch: tt.line}}); len(got) == 0 {
			t.Errorf("%s %s: not held", tt.path, tt.line)
		}
	}
	if got := lockSources([]File{{Path: "uv.lock", Patch: `+sdist = { url = "https://files.pythonhosted.org/p/x.tar.gz" }`}}); len(got) != 0 {
		t.Errorf("PyPI file held: %+v", got)
	}
}

// Every source is kept, so a config allowlist applied later can't hide one
// behind many allowed ones.
func TestLockSources_NoLimit(t *testing.T) {
	var patch string
	for i := 0; i < 20; i++ {
		patch += `+      "resolved": "https://npm.corp.example/p` + string(rune('a'+i)) + `.tgz",` + "\n"
	}
	patch += `+      "resolved": "https://evil.example/x.tgz",` + "\n"
	got := lockSources([]File{{Path: "package-lock.json", Patch: patch}})
	if len(got) != 21 || got[20].Host != "evil.example" {
		t.Errorf("got %d sources, last %+v", len(got), got[len(got)-1])
	}
}

// A lockfile GitHub sent no patch for can't be checked, so the view is
// partial and approve is held.
func TestSnapshot_LockfileWithoutPatchIsPartial(t *testing.T) {
	raw := &Raw{Files: []File{
		{Path: "package-lock.json", Status: "modified", Changes: 20000},
		{Path: "main.go", Status: "modified", Changes: 1, Patch: "+package main"},
	}}
	s, err := raw.Snapshot([]string{"**/package-lock.json"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Partial) == 0 {
		t.Errorf("partial = %v, unseen = %v", s.Partial, s.Unseen)
	}
}

// Excluded lockfiles lose their patch, but the check reads them first.
func TestSnapshot_LockSourcesSurviveExclude(t *testing.T) {
	raw := &Raw{Files: []File{
		{Path: "yarn.lock", Status: "modified", Changes: 1, Patch: "+  resolved \"https://evil.example/pkg.tgz#abc\"\n"},
		{Path: "main.go", Status: "modified", Changes: 1, Patch: "+package main\n"},
	}}
	s, err := raw.Snapshot([]string{"**/*.lock", "*.lock"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Excluded) != 1 || s.Excluded[0].Patch != "" {
		t.Fatalf("excluded = %+v", s.Excluded)
	}
	if len(s.LockSources) != 1 || s.LockSources[0].Host != "evil.example" {
		t.Errorf("lock sources = %+v", s.LockSources)
	}
}
