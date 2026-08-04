package safeio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadRegularFileBoundsAndRejectsNonRegular(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "input")
	if err := os.WriteFile(path, []byte("abcd"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := ReadRegularFile(path, 4, time.Second)
	if err != nil || string(data) != "abcd" {
		t.Fatalf("exact-limit read = %q, %v", data, err)
	}
	if _, err := ReadRegularFile(path, 3, time.Second); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize error = %v", err)
	}
	if _, err := ReadRegularFile(dir, 32, time.Second); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("directory error = %v", err)
	}
}

func TestReadRegularFileNoFollowRejectsOutsideSymlink(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	secretData := []byte("outside-secret-bytes")
	if err := os.WriteFile(secret, secretData, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "AGENTS.md")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	reader := NewReader()
	if data, err := reader.ReadRegularFileNoFollow(link, 1024, time.Second); !errors.Is(err, ErrSymlink) || data != nil {
		t.Fatalf("no-follow symlink read = %q, %v", data, err)
	}
	data, err := reader.ReadRegularFile(link, 1024, time.Second)
	if err != nil || string(data) != string(secretData) {
		t.Fatalf("explicit follow read = %q, %v", data, err)
	}
}

func TestOpenWithinNoFollowRejectsIntermediateSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "redirect")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	file, err := OpenWithinNoFollow(root, filepath.Join("redirect", "secret"))
	if file != nil {
		_ = file.Close()
		t.Fatal("intermediate symlink returned a readable descriptor")
	}
	if err == nil {
		t.Fatal("intermediate symlink was followed")
	}

	if _, err := OpenWithinNoFollow(root, filepath.Join("..", "secret")); err == nil {
		t.Fatal("lexical workspace escape was accepted")
	}
}

func TestReadPrivateRegularFileChmodsVerifiedDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	if err := os.WriteFile(path, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	reader := NewReader()
	if _, err := reader.ReadPrivateRegularFileNoFollow(path, 1024, time.Second); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("private descriptor mode = %04o, want 0600", got)
	}
}
