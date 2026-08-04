package safeio

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestExclusiveFileLockSerializesIndependentCallers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lock")
	entered := make(chan struct{})
	release := make(chan struct{})
	firstErr := make(chan error, 1)
	go func() {
		firstErr <- WithExclusiveFileLock(path, time.Second, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	var callbackMu sync.Mutex
	callbackRan := false
	err := WithExclusiveFileLock(path, 30*time.Millisecond, func() error {
		callbackMu.Lock()
		callbackRan = true
		callbackMu.Unlock()
		return nil
	})
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("contended lock error = %v", err)
	}
	callbackMu.Lock()
	if callbackRan {
		t.Fatal("timed-out lock ran its callback")
	}
	callbackMu.Unlock()
	close(release)
	if err := <-firstErr; err != nil {
		t.Fatal(err)
	}
}

func TestExclusiveFileLockRejectsSymlinkWithoutTouchingVictim(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim.lock")
	victimData := []byte("outside lock victim")
	if err := os.WriteFile(victim, victimData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(victim, 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "store.lock")
	if err := os.Symlink(victim, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := WithExclusiveFileLock(path, time.Second, func() error {
		t.Fatal("symlink lock ran callback")
		return nil
	}); !errors.Is(err, ErrSymlink) {
		t.Fatalf("symlink lock error = %v", err)
	}
	data, err := os.ReadFile(victim)
	if err != nil || string(data) != string(victimData) {
		t.Fatalf("lock victim content changed: data=%q err=%v", data, err)
	}
	info, err := os.Stat(victim)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("lock victim mode changed: info=%v err=%v", info, err)
	}
}
