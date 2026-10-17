package prstate

import (
	"path"
	"strings"
)

// AutoRun is a file the diff changes that sets commands to run on their
// own on a developer's machine: editor tasks, dev containers, git hooks,
// and AI-agent hooks. Attacks use these to run code without anyone
// starting it (docs/calibration.md#safety). Any change to one holds,
// because a command edit or a trigger outside the patch's context would
// slip past a check of the trigger line alone.
type AutoRun struct {
	Path, What string
}

// autoRunKind names what p configures, or "" if it isn't an auto-run file.
func autoRunKind(p string) string {
	base, dir := path.Base(p), "/"+path.Dir(p)+"/"
	switch {
	case base == "tasks.json" && strings.HasSuffix(dir, "/.vscode/"), strings.HasSuffix(base, ".code-workspace"):
		return "editor tasks"
	case strings.Contains(dir, "/.devcontainer/"), base == ".devcontainer.json":
		return "dev container commands"
	case strings.Contains(dir, "/.husky/"), strings.Contains(dir, "/.githooks/"):
		return "git hooks"
	case (base == "settings.json" || base == "settings.local.json") && (strings.HasSuffix(dir, "/.claude/") || strings.HasSuffix(dir, "/.gemini/")),
		base == "hooks.json" && strings.HasSuffix(dir, "/.cursor/"):
		return "AI-agent settings and hooks"
	}
	return ""
}

// IsAutoRun reports whether p is an auto-run file.
func IsAutoRun(p string) bool { return autoRunKind(p) != "" }

// autoRuns returns the auto-run files the diff adds or changes.
func autoRuns(files []File) []AutoRun {
	var out []AutoRun
	for _, f := range files {
		if k := autoRunKind(f.Path); k != "" && f.Status != "removed" {
			out = append(out, AutoRun{Path: f.Path, What: k})
		}
	}
	return out
}
