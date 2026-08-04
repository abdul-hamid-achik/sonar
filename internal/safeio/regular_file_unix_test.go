//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package safeio

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestReadRegularFileFIFOIsRejectedWithoutStrandingWorker(t *testing.T) {
	reader := NewReader()
	dir := t.TempDir()
	fifo := filepath.Join(dir, "blocked")
	if err := exec.Command("mkfifo", fifo).Run(); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if _, err := reader.ReadRegularFile(fifo, 64, time.Second); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("FIFO read error = %v", err)
	}
	if got := len(reader.slot); got != 0 {
		t.Fatalf("stranded worker slots = %d, want 0", got)
	}

	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := reader.ReadRegularFile(regular, 64, time.Second); err != nil || string(data) != "ok" {
		t.Fatalf("reader did not recover immediately: data=%q err=%v", data, err)
	}
}

func TestReadRegularFileRejectsDevice(t *testing.T) {
	if _, err := ReadRegularFile("/dev/null", 64, time.Second); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("device error = %v", err)
	}
}
