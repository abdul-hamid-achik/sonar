package goaladvisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCurrentWorkspaceRevisionTracksHeadAndDirtyBytes(t *testing.T) {
	dir := t.TempDir()
	runRevisionGitTest(t, dir, "init", "-q")
	runRevisionGitTest(t, dir, "config", "user.name", "Goal Test")
	runRevisionGitTest(t, dir, "config", "user.email", "goal@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("tracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRevisionGitTest(t, dir, "add", "tracked.txt")
	runRevisionGitTest(t, dir, "commit", "-qm", "initial")

	clean, err := CurrentWorkspaceRevision(context.Background(), dir)
	if err != nil || !clean.Valid() {
		t.Fatalf("clean revision = %#v err=%v", clean, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := CurrentWorkspaceRevision(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Commit != clean.Commit || first.DirtyDigest == clean.DirtyDigest {
		t.Fatalf("untracked state = clean %#v first %#v", clean, first)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := CurrentWorkspaceRevision(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if second.DirtyDigest == first.DirtyDigest {
		t.Fatalf("untracked byte mutation did not change digest: %#v", second)
	}
}

func TestCurrentWorkspaceRevisionRejectsUntrackedSymlinkOutsideWorkspace(t *testing.T) {
	dir := t.TempDir()
	runRevisionGitTest(t, dir, "init", "-q")
	runRevisionGitTest(t, dir, "config", "user.name", "Goal Test")
	runRevisionGitTest(t, dir, "config", "user.email", "goal@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("tracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRevisionGitTest(t, dir, "add", "tracked.txt")
	runRevisionGitTest(t, dir, "commit", "-qm", "initial")

	target := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(target, []byte("secret one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "outside-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := CurrentWorkspaceRevision(context.Background(), dir); err == nil {
		t.Fatal("outside untracked symlink was followed while computing completion proof")
	}
}

func runRevisionGitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
