package safeio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestValidatePublishPathRejectsFinalParentSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	parent := filepath.Join(root, "store-parent")
	if err := os.Symlink(outside, parent); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := ValidatePublishPath(filepath.Join(parent, "store.json")); !errors.Is(err, ErrSymlink) {
		t.Fatalf("symlink parent error = %v", err)
	}
}

func TestValidatePublishPathChecksManagedHomeComponents(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	configDir := filepath.Join(home, ".config")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	managedLink := filepath.Join(configDir, "sonar")
	if err := os.Symlink(outside, managedLink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	path := filepath.Join(managedLink, "ice", "workspace", "store.json")
	if err := ValidatePublishPath(path); !errors.Is(err, ErrSymlink) {
		t.Fatalf("managed intermediate symlink error = %v", err)
	}
}
