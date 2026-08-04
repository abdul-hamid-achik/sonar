package permission

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceRulesBashMCPAndPaths(t *testing.T) {
	root := t.TempDir()
	store, err := NewWorkspaceRulesStore(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o700); err != nil {
		t.Fatal(err)
	}

	rules, err := store.AddBashPrefix(workspace, "git status *")
	if err != nil {
		t.Fatal(err)
	}
	if !rules.AllowsBash("git status -sb") {
		t.Fatal("expected bash glob allow")
	}
	if rules.AllowsBash("git log") {
		t.Fatal("unexpected bash allow")
	}

	rules, err = store.AddMCPTool(workspace, "mcphub__mcphub_list_servers")
	if err != nil {
		t.Fatal(err)
	}
	if !rules.AllowsMCPTool("mcphub__mcphub_list_servers") {
		t.Fatal("expected mcp allow")
	}

	target := filepath.Join(workspace, "src", "main.go")
	if err := os.WriteFile(target, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rules, err = store.AddWritePath(workspace, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules.WritePaths) != 1 || rules.WritePaths[0] != "src/main.go" {
		t.Fatalf("write paths = %#v", rules.WritePaths)
	}
	if !rules.AllowsWritePath(workspace, target) {
		t.Fatal("expected write path allow")
	}

	loaded, err := store.Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.BashPrefixes) != 1 || len(loaded.MCPTools) != 1 || len(loaded.WritePaths) != 1 {
		t.Fatalf("loaded = %#v", loaded)
	}

	if _, removed, err := store.RemoveBashPrefix(workspace, "git status *"); err != nil || !removed {
		t.Fatalf("remove bash = %v %v", removed, err)
	}
	if _, removed, err := store.RemoveMCPTool(workspace, "mcphub__mcphub_list_servers"); err != nil || !removed {
		t.Fatalf("remove mcp = %v %v", removed, err)
	}
	if _, removed, err := store.RemoveWritePath(workspace, target); err != nil || !removed {
		t.Fatalf("remove path = %v %v", removed, err)
	}
}
