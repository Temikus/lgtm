package main

import "testing"

func TestParsePRRef(t *testing.T) {
	tests := []struct {
		in      string
		owner   string
		repo    string
		n       int
		wantErr bool
	}{
		{in: "Temikus/lgtm#1", owner: "Temikus", repo: "lgtm", n: 1},
		{in: "https://github.com/Temikus/lgtm/pull/42", owner: "Temikus", repo: "lgtm", n: 42},
		{in: "https://github.com/Temikus/lgtm/pull/42/files", owner: "Temikus", repo: "lgtm", n: 42},
		{in: "https://evil.example/Temikus/lgtm/pull/42", wantErr: true},
		{in: "http://github.com/Temikus/lgtm/pull/42", wantErr: true},
		{in: "https://github.com/Temikus/lgtm/issues/42", wantErr: true},
		{in: "Temikus/lgtm", wantErr: true},
	}
	for _, tt := range tests {
		owner, repo, n, err := parsePRRef(tt.in)
		if (err != nil) != tt.wantErr || owner != tt.owner || repo != tt.repo || n != tt.n {
			t.Errorf("parsePRRef(%q) = %q %q %d %v", tt.in, owner, repo, n, err)
		}
	}
}
