package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/Temikus/lgtm/internal/config"
)

func TestParseAge(t *testing.T) {
	for in, want := range map[string]time.Duration{"90d": 90 * 24 * time.Hour, "1d": 24 * time.Hour, "36h": 36 * time.Hour} {
		if got, err := parseAge(in); err != nil || got != want {
			t.Errorf("parseAge(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0d", "-5h", "90 days", "d"} {
		if _, err := parseAge(in); err == nil {
			t.Errorf("parseAge(%q) should fail", in)
		}
	}
}

func TestWriteCalibration_TemplateConfig(t *testing.T) {
	tmpl, err := templates.ReadFile("templates/lgtm.toml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "lgtm.toml")
	if err := os.WriteFile(path, tmpl, 0o600); err != nil {
		t.Fatal(err)
	}
	c := config.Calibration{Model: config.DefaultModel, Snapshot: config.DefaultModel + "-20260917", Date: toml.LocalDate{Year: 2026, Month: 10, Day: 16}, NLabelled: 40, ApproveAt: 0.93, DenyAt: 0.08, ApprovePrecision: 0.96}
	if err := writeCalibration(path, c); err != nil {
		t.Fatal(err)
	}
	got, err := config.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Calibrated() || got.Snapshot() != c.Snapshot {
		t.Errorf("config after write = %+v", got.Calibration)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), string(tmpl)) {
		t.Error("the template text should be kept as is")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 kept", fi.Mode().Perm())
	}
}
