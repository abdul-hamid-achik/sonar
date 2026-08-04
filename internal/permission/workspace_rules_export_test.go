package permission

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceRulesExportImportRoundTrip(t *testing.T) {
	root := t.TempDir()
	store, err := NewWorkspaceRulesStore(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddBashPrefix(workspace, "go test *"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddMCPTool(workspace, "mcphub__mcphub_list_servers"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspace, "src", "main.go")
	if err := os.WriteFile(target, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddWritePath(workspace, target); err != nil {
		t.Fatal(err)
	}

	rules, err := store.Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	doc := rules.ExportDocument()
	if doc.RuleCount() != 3 {
		t.Fatalf("export count = %d", doc.RuleCount())
	}
	out := filepath.Join(t.TempDir(), "rules.json")
	if err := WriteExportFile(out, doc); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadExportFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RuleCount() != 3 {
		t.Fatalf("loaded count = %d", loaded.RuleCount())
	}

	// Import into a fresh workspace store entry (same store, different workspace).
	other := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	imported, added, err := store.Import(other, loaded, false)
	if err != nil {
		t.Fatal(err)
	}
	if added != 3 {
		t.Fatalf("added = %d", added)
	}
	if !imported.AllowsBash("go test ./...") {
		t.Fatal("imported bash pattern missing")
	}
	if !imported.AllowsMCPTool("mcphub__mcphub_list_servers") {
		t.Fatal("imported mcp missing")
	}
	if len(imported.WritePaths) != 1 || imported.WritePaths[0] != "src/main.go" {
		t.Fatalf("write paths = %#v", imported.WritePaths)
	}

	// Replace clears prior rules.
	if _, err := store.AddBashPrefix(other, "git status *"); err != nil {
		t.Fatal(err)
	}
	replaced, added, err := store.Import(other, WorkspaceRulesExport{
		FormatVersion: WorkspaceRulesExportFormat,
		BashPrefixes:  []string{"npm test *"},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || len(replaced.BashPrefixes) != 1 || replaced.BashPrefixes[0] != "npm test *" {
		t.Fatalf("replace = %#v added=%d", replaced, added)
	}
	if replaced.AllowsBash("git status") {
		t.Fatal("replace left old bash rule")
	}

	cleared, err := store.ClearAll(other)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.AllowsBash("npm test") || len(cleared.MCPTools) != 0 || len(cleared.WritePaths) != 0 {
		t.Fatalf("clear failed: %#v", cleared)
	}
}

func TestReadExportFileRejectsBadWritePaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{"format_version":1,"write_paths":["../escape"]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadExportFile(path); err == nil {
		t.Fatal("expected invalid export")
	}
}
