package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInit_WritesFiles(t *testing.T) {
	dir := t.TempDir()
	cmd := newRootCmd()
	cmd.SetArgs([]string{"init", "--dir", dir, "--ref", "v9"})
	cmd.SetOut(&strings.Builder{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	wf, err := os.ReadFile(filepath.Join(dir, ".github/workflows/lgtm.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(wf)
	for _, want := range []string{"uses: Temikus/lgtm@v9", "${{ secrets.OPENROUTER_API_KEY }}", "pull_request_target", "timeout-minutes: 10", "cancel-in-progress: true", "checks: write"} {
		if !strings.Contains(s, want) {
			t.Errorf("workflow missing %q", want)
		}
	}
	if strings.Contains(s, "{{.Ref}}") || strings.Contains(s, "actions/checkout") {
		t.Error("workflow must have the ref filled in and no checkout")
	}
	if _, err := os.Stat(filepath.Join(dir, ".github/lgtm.toml")); err != nil {
		t.Error(err)
	}

	cmd = newRootCmd()
	cmd.SetArgs([]string{"init", "--dir", dir})
	if err := cmd.Execute(); err == nil {
		t.Error("init must not overwrite without --force")
	}
}

func TestInit_TemplateConfigValidates(t *testing.T) {
	dir := t.TempDir()
	cmd := newRootCmd()
	cmd.SetArgs([]string{"init", "--dir", dir})
	cmd.SetOut(&strings.Builder{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	cmd = newRootCmd()
	cmd.SetArgs([]string{"config", "validate", filepath.Join(dir, ".github/lgtm.toml")})
	cmd.SetOut(&strings.Builder{})
	if err := cmd.Execute(); err != nil {
		t.Errorf("the generated config must validate: %v", err)
	}
}
