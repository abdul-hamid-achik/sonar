package initcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_GoProject(t *testing.T) {
	dir := t.TempDir()

	// Create a go.mod marker file.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Create a source directory with a file.
	if err := os.Mkdir(filepath.Join(dir, "cmd"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmd", "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := Run(dir, Options{}); err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("reading AGENTS.md: %v", err)
	}

	content := string(data)

	// Check project type detection.
	if !strings.Contains(content, "Go") {
		t.Error("expected AGENTS.md to contain 'Go' project type")
	}

	// Check directory listing includes go.mod.
	if !strings.Contains(content, "go.mod") {
		t.Error("expected AGENTS.md to list go.mod")
	}

	// Check directory listing includes cmd/ subdirectory.
	if !strings.Contains(content, "cmd/") {
		t.Error("expected AGENTS.md to list cmd/ directory")
	}

	// Check that main.go appears under cmd/.
	if !strings.Contains(content, "main.go") {
		t.Error("expected AGENTS.md to list main.go inside cmd/")
	}

	// Check placeholder sections exist.
	for _, section := range []string{"## Build Commands", "## Architecture", "## Key Files", "## Notes"} {
		if !strings.Contains(content, section) {
			t.Errorf("expected AGENTS.md to contain section %q", section)
		}
	}
}

func TestRun_EmptyDirectory(t *testing.T) {
	dir := t.TempDir()

	if err := Run(dir, Options{}); err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("reading AGENTS.md: %v", err)
	}

	content := string(data)

	// With no marker files, project type should be "Unknown".
	if !strings.Contains(content, "Unknown") {
		t.Error("expected AGENTS.md to contain 'Unknown' project type for empty dir")
	}
}

func TestRun_ExistingAgentMD_NoOverwrite(t *testing.T) {
	dir := t.TempDir()

	agentPath := filepath.Join(dir, "AGENTS.md")
	original := "# Original content\n"
	if err := os.WriteFile(agentPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	err := Run(dir, Options{})
	if err == nil {
		t.Fatal("expected error when AGENTS.md already exists")
	}

	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' in error, got: %v", err)
	}

	// Verify file was not modified.
	data, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Error("AGENTS.md was unexpectedly modified")
	}
}

func TestRun_Force(t *testing.T) {
	dir := t.TempDir()

	agentPath := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(agentPath, []byte("# Old content\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a go.mod so the new content is distinguishable.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := Run(dir, Options{Force: true}); err != nil {
		t.Fatalf("Run() with Force=true returned error: %v", err)
	}

	data, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}

	content := string(data)
	if strings.Contains(content, "Old content") {
		t.Error("AGENTS.md should have been overwritten with Force=true")
	}
	if !strings.Contains(content, "Go") {
		t.Error("expected new AGENTS.md to detect Go project type")
	}
}

func TestRunRejectsDanglingAgentSymlinkEscape(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%v", force), func(t *testing.T) {
			dir := t.TempDir()
			outside := filepath.Join(t.TempDir(), "escaped.md")
			agentPath := filepath.Join(dir, "AGENTS.md")
			if err := os.Symlink(outside, agentPath); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			err := Run(dir, Options{Force: force})
			if err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("dangling symlink Run error = %v", err)
			}
			if _, err := os.Stat(outside); !os.IsNotExist(err) {
				t.Fatalf("dangling symlink target was created: %v", err)
			}
			if info, err := os.Lstat(agentPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("destination symlink changed: info=%v err=%v", info, err)
			}
		})
	}
}

func TestRunRejectsExistingAgentSymlinkWithoutChangingTarget(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := Run(dir, Options{Force: true}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("existing symlink Run error = %v", err)
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "keep" {
		t.Fatalf("outside target changed: data=%q err=%v", data, err)
	}
}

func TestRunPublishesAtomicFileWithoutTemporaryArtifacts(t *testing.T) {
	dir := t.TempDir()
	if err := Run(dir, Options{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o644 {
		t.Fatalf("AGENTS.md mode = %s", info.Mode())
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".AGENTS.md-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temporary artifacts remain: %v", temps)
	}
}

func TestRunRefusesToShadowLegacyAgentInstructions(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%v", force), func(t *testing.T) {
			dir := t.TempDir()
			legacyPath := filepath.Join(dir, "AGENT.md")
			if err := os.WriteFile(legacyPath, []byte("authored legacy instructions"), 0o600); err != nil {
				t.Fatal(err)
			}
			err := Run(dir, Options{Force: force})
			if err == nil || !strings.Contains(err.Error(), "rename or migrate") {
				t.Fatalf("legacy shadow error = %v", err)
			}
			if _, err := os.Lstat(filepath.Join(dir, "AGENTS.md")); !os.IsNotExist(err) {
				t.Fatalf("plural instructions were created: %v", err)
			}
			data, err := os.ReadFile(legacyPath)
			if err != nil || string(data) != "authored legacy instructions" {
				t.Fatalf("legacy instructions changed: data=%q err=%v", data, err)
			}
		})
	}
}

func TestRunRefusesToShadowLegacyAgentSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.md")
	legacyPath := filepath.Join(dir, "AGENT.md")
	if err := os.Symlink(outside, legacyPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	err := Run(dir, Options{Force: true})
	if err == nil || !strings.Contains(err.Error(), "AGENT.md symlink") || !strings.Contains(err.Error(), "rename or migrate") {
		t.Fatalf("legacy symlink shadow error = %v", err)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("legacy symlink target was created: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("plural instructions were created: %v", err)
	}
}
