package prstate

import "testing"

func TestAutoRuns(t *testing.T) {
	files := []File{
		{Path: ".vscode/tasks.json", Status: "modified"},
		{Path: "app/.claude/settings.json", Status: "added"},
		{Path: ".devcontainer/devcontainer.json", Status: "modified"},
		{Path: ".husky/pre-commit", Status: "added"},
		{Path: ".cursor/hooks.json", Status: "modified"},
		{Path: "dev.code-workspace", Status: "modified"},
		{Path: ".vscode/tasks.json.bak", Status: "added"},     // not a tasks file
		{Path: "docs/tasks.json", Status: "added"},            // not under .vscode
		{Path: ".gemini/settings.json", Status: "removed"},    // removing one can't run anything
		{Path: ".vscode/settings.json", Status: "modified"},   // editor settings, not tasks
		{Path: "my.claude/settings.json", Status: "modified"}, // a lookalike directory
	}
	got := autoRuns(files)
	var paths []string
	for _, a := range got {
		paths = append(paths, a.Path)
	}
	want := []string{".vscode/tasks.json", "app/.claude/settings.json", ".devcontainer/devcontainer.json", ".husky/pre-commit", ".cursor/hooks.json", "dev.code-workspace"}
	if len(paths) != len(want) {
		t.Fatalf("autoRuns = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("autoRuns[%d] = %s, want %s", i, paths[i], want[i])
		}
	}
}
