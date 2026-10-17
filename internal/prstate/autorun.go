package prstate

import (
	"path"
	"regexp"
	"strings"
)

// AutoRun is a command a diff sets to run on its own on a developer's
// machine: an editor task on folder open, or an AI-agent hook. Attacks use
// these to run code without anyone starting it (docs/calibration.md#safety).
type AutoRun struct {
	Path, What string
}

var (
	folderOpen = regexp.MustCompile(`"runOn"\s*:\s*"folderOpen"`)
	agentHook  = regexp.MustCompile(`"(SessionStart|SessionEnd|UserPromptSubmit|PreToolUse|PostToolUse|Notification|Stop|SubagentStop|PreCompact)"\s*:`)
)

// autoRuns returns the auto-run commands that added lines set up.
func autoRuns(files []File) []AutoRun {
	var out []AutoRun
	for _, f := range files {
		var re *regexp.Regexp
		var what string
		switch base := path.Base(f.Path); {
		case base == "tasks.json" && strings.Contains(f.Path, ".vscode/"), strings.HasSuffix(base, ".code-workspace"):
			re, what = folderOpen, "editor task that runs on folder open"
		case (base == "settings.json" || base == "settings.local.json") && (strings.Contains(f.Path, ".claude/") || strings.Contains(f.Path, ".gemini/")),
			base == "hooks.json" && strings.Contains(f.Path, ".cursor/"):
			re, what = agentHook, "AI-agent hook"
		default:
			continue
		}
		for _, line := range strings.Split(f.Patch, "\n") {
			if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") && re.MatchString(line) {
				out = append(out, AutoRun{Path: f.Path, What: what})
				break
			}
		}
	}
	return out
}
