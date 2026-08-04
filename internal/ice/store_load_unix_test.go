//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package ice

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

func TestStoreLoadFIFOIsBoundedAndFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conversations.json")
	if err := exec.Command("mkfifo", path).Run(); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	oldReader := iceStoreFileReader
	oldTimeout := iceStoreReadTimeout
	reader := safeio.NewReader()
	iceStoreFileReader = reader
	iceStoreReadTimeout = 30 * time.Millisecond
	t.Cleanup(func() {
		iceStoreFileReader = oldReader
		iceStoreReadTimeout = oldTimeout
	})

	started := time.Now()
	store := NewStore(path)
	if !errors.Is(store.Err(), safeio.ErrNotRegular) {
		t.Fatalf("FIFO store error = %v", store.Err())
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("FIFO store load took %s", elapsed)
	}
	if _, err := store.Add("session", "user", "must not overwrite", []float32{1}, 0); err == nil {
		t.Fatal("FIFO ICE store accepted mutation")
	}

	probe := filepath.Join(dir, "probe.json")
	if err := os.WriteFile(probe, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadPrivateRegularFileNoFollow(probe, 16, time.Second); err != nil {
		t.Fatalf("ICE reader did not remain available: %v", err)
	}
}
