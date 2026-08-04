//go:build windows || plan9 || js || wasip1

package safeio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenWithinNoFollowFailsClosedWhenTraversalIsUnavailable(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "regular.txt"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := OpenWithinNoFollow(root, "regular.txt")
	if file != nil {
		_ = file.Close()
		t.Fatal("unsupported no-follow traversal returned a descriptor")
	}
	if !errors.Is(err, ErrNoFollowUnsupported) {
		t.Fatalf("unsupported traversal error = %v", err)
	}
}
