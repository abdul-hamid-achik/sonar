package mcp

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveExecutableUsesLocalBinWithMinimalPATH(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(binDir, "test-mcp-server")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")

	got, err := resolveExecutable("test-mcp-server")
	if err != nil {
		t.Fatal(err)
	}
	if got != command {
		t.Fatalf("resolved command = %q, want %q", got, command)
	}
}

func TestVerifyTrustedExecutableRejectsReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo-mcp")
	original := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(path, original, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(original))
	if err := verifyTrustedExecutable(path, digest); err != nil {
		t.Fatalf("verify original executable: %v", err)
	}

	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := verifyTrustedExecutable(path, digest); err == nil {
		t.Fatal("changed executable retained repository trust")
	}
}
