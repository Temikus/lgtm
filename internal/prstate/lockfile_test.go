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
