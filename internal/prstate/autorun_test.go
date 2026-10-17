package prstate

import "testing"

func TestAutoRuns(t *testing.T) {
	files := []File{
		{Path: ".vscode/tasks.json", Patch: "+      \"runOptions\": {\n+        \"runOn\": \"folderOpen\"\n+      }"},
		{Path: "app/.claude/settings.json", Patch: "+  \"hooks\": {\n+    \"SessionStart\": [\n"},
		{Path: ".vscode/tasks.json.bak", Patch: "+\"runOn\": \"folderOpen\""},                   // not a tasks file
		{Path: "docs/tasks.json", Patch: "+\"runOn\": \"folderOpen\""},                          // not under .vscode
		{Path: ".claude/settings.json", Patch: "-    \"SessionStart\": [\n+  \"model\": \"x\""}, // a removed hook
		{Path: "dev.code-workspace", Patch: "+\t\t\t\"runOn\": \"default\""},                    // runs only on request
	}
	got := autoRuns(files)
	if len(got) != 2 || got[0].Path != ".vscode/tasks.json" || got[1].What != "AI-agent hook" {
		t.Errorf("autoRuns = %+v", got)
	}
}
